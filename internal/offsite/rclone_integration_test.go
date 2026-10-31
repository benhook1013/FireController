//go:build offsite_integration

package offsite

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
)

// This explicit opt-in workload uses a real rclone binary and S3 protocol. By
// default rclone serves the disposable S3 fixture; an independently supplied
// loopback MinIO endpoint exercises a different server implementation too.
func TestOffsiteRealRcloneS3AndCryptRecovery(t *testing.T) {
	binary := os.Getenv("FC_TEST_RCLONE")
	if binary == "" {
		t.Skip("set FC_TEST_RCLONE to the explicit test rclone executable")
	}
	binary, err := exec.LookPath(binary)
	if err == nil {
		binary, err = filepath.Abs(binary)
	}
	if err != nil {
		t.Fatal(err)
	}
	endpoint := os.Getenv("FC_TEST_S3_ENDPOINT")
	access, secret := "offsite-fixture-key", "offsite-fixture-password-only"
	if endpoint == "" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		listener.Close()
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		cmd := exec.CommandContext(ctx, binary, "serve", "s3", t.TempDir(), "--addr", address, "--auth-key", access+","+secret, "--config", os.DevNull)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		processGroup(cmd)
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cancel(); cmd.Wait() })
		endpoint = "http://" + address
		client := &http.Client{Timeout: time.Second}
		deadline := time.Now().Add(5 * time.Second)
		for {
			response, err := client.Get(endpoint)
			if err == nil {
				response.Body.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("S3 fixture did not start")
			}
			time.Sleep(20 * time.Millisecond)
		}
	} else {
		// External fixture credentials are synthetic, never real cloud accounts.
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "http" || u.User != nil || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.Port() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			t.Fatal("only an explicit loopback S3 fixture is allowed")
		}
		if value := os.Getenv("FC_TEST_S3_ACCESS_KEY"); value != "" {
			access = value
		}
		if value := os.Getenv("FC_TEST_S3_SECRET_KEY"); value != "" {
			secret = value
		}
	}
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%t", encrypted), func(t *testing.T) {
			f := newFixture(t)
			f.c.RcloneBinary = binary
			f.c.Timeout = "60s"
			f.c.RcloneConfig = filepath.Join(t.TempDir(), "rclone.conf")
			bucket := "fc-offsite-" + strings.TrimPrefix(f.c.InstanceID, "instance_")
			config := fmt.Sprintf("[store]\ntype = s3\nprovider = Other\naccess_key_id = %s\nsecret_access_key = %s\nendpoint = %s\nregion = us-east-1\nforce_path_style = true\n", access, secret, endpoint)
			f.c.Remote = "store:" + bucket + "/backups"
			if encrypted {
				obscure := exec.CommandContext(t.Context(), binary, "obscure", "fixture-encryption-secret")
				value, err := obscure.Output()
				if err != nil {
					t.Fatal(err)
				}
				config += fmt.Sprintf("\n[encrypted]\ntype = crypt\nremote = store:%s/encrypted\npassword = %s\nfilename_encryption = standard\ndirectory_name_encryption = true\n", bucket, strings.TrimSpace(string(value)))
				f.c.Remote = "encrypted:backups"
			}
			if err = os.WriteFile(f.c.RcloneConfig, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			// Ambient agent options cannot redirect this explicit private connection.
			t.Setenv("RCLONE_CONFIG_STORE_ENDPOINT", "http://127.0.0.1:1")
			operator := os.Getenv("FC_TEST_FCD")
			configPath := filepath.Join(t.TempDir(), "offsite.json")
			call := func(command string, args ...string) []byte {
				t.Helper()
				if err := writeJSON(configPath, f.c); err != nil {
					t.Fatal(err)
				}
				cmd := exec.CommandContext(t.Context(), operator, append([]string{command, "--config", configPath}, args...)...)
				var diagnostic strings.Builder
				cmd.Stderr = &diagnostic
				b, err := cmd.Output()
				if err != nil {
					t.Fatalf("operator %s failed: %v %s", command, err, diagnostic.String())
				}
				return b
			}
			cycle := func() (Result, error) {
				if operator == "" {
					return push(t.Context(), f.c, nil)
				}
				var value Result
				err := json.Unmarshal(call("backup-push"), &value)
				return value, err
			}
			first := f.bundle(t)
			result, err := cycle()
			if err != nil {
				t.Fatalf("first transfer: %#v", err)
			}
			if result.BackupID != first.ID {
				t.Fatal(result)
			}
			latest := f.bundle(t)
			result, err = cycle()
			if err != nil {
				t.Fatalf("second transfer: %#v", err)
			}
			if result.BackupID != latest.ID || result.Retained != 1 {
				t.Fatal(result)
			}
			if _, err = cycle(); err != nil {
				t.Fatal(err)
			}
			if operator != "" {
				var report Report
				if json.Unmarshal(call("backup-offsite-status"), &report) != nil || report.LastSuccess == nil || report.LastSuccess.ID != latest.ID || report.Retained != 1 {
					t.Fatal("packaged status mismatch")
				}
			}
			// A real rejected S3 credential must preserve the prior verified copy
			// and retain the next publication for exact recovery, not reset it.
			prior := latest
			latest = f.bundle(t)
			bad := strings.Replace(config, "secret_access_key = "+secret, "secret_access_key = invalid-fixture-secret", 1)
			if err = os.WriteFile(f.c.RcloneConfig, []byte(bad), 0600); err != nil {
				t.Fatal(err)
			}
			if operator != "" {
				if err = writeJSON(configPath, f.c); err != nil {
					t.Fatal(err)
				}
				cmd := exec.CommandContext(t.Context(), operator, "backup-push", "--config", configPath)
				output, e := cmd.CombinedOutput()
				if e == nil || strings.Contains(string(output), secret) {
					t.Fatal("credential failure was hidden or leaked")
				}
			} else {
				_, err = push(t.Context(), f.c, nil)
				wantCode(t, err, "offsite_transport_failed")
			}
			status := state(t, f.c)
			if status.LastSuccess.ID != prior.ID || status.Pending == nil || status.Pending.ID != latest.ID {
				t.Fatal("credential failure lost recovery state")
			}
			if err = os.WriteFile(f.c.RcloneConfig, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			if result, err = cycle(); err != nil || result.BackupID != latest.ID {
				t.Fatal("authentication recovery", result, err)
			}
			// The old completion must actually be absent at the remote, not merely
			// removed from our local accounting.
			s, err := openSession(f.c, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.remoteReceipt(t.Context(), Receipt{ID: first.ID}); err != notFound {
				s.close()
				t.Fatal("old remote receipt not pruned", err)
			}
			s.close()
			// Mutate the actual remote archive through its selected backend. A
			// fetch must reject it and remove its own incomplete destination.
			s, err = openSession(f.c, nil)
			if err != nil {
				t.Fatal(err)
			}
			receipt := *state(t, f.c).LastSuccess
			badArchive := filepath.Join(t.TempDir(), "corrupt.gz")
			if err = os.WriteFile(badArchive, []byte("damaged remote archive"), 0600); err != nil {
				t.Fatal(err)
			}
			if err = s.transport.put(t.Context(), badArchive, s.object(receipt, "archive.tar.gz")); err != nil {
				t.Fatal(err)
			}
			failedDestination := filepath.Join(t.TempDir(), "rejected-download")
			_, err = s.fetch(t.Context(), "", failedDestination)
			wantCode(t, err, "offsite_archive_invalid")
			if _, e := os.Stat(failedDestination); !os.IsNotExist(e) {
				t.Fatal("failed fetch left a usable-looking destination")
			}
			repair := filepath.Join(t.TempDir(), "repair.gz")
			recreated, e := pack(t.Context(), latest.Path, repair, f.c)
			if e != nil || recreated != receipt {
				t.Fatal("fixture repair lost original identity", e)
			}
			if err = s.transport.put(t.Context(), repair, s.object(receipt, "archive.tar.gz")); err != nil {
				t.Fatal(err)
			}
			s.close()
			if err = f.store.Close(); err != nil {
				t.Fatal(err)
			}
			if err = os.RemoveAll(f.c.SourceRoot); err != nil {
				t.Fatal(err)
			}
			if err = os.RemoveAll(f.dir); err != nil {
				t.Fatal(err)
			}
			f.c.StateDir = filepath.Join(t.TempDir(), "fresh-transfer-state")
			var got backup.Summary
			if operator != "" {
				err = json.Unmarshal(call("backup-fetch", "--destination", filepath.Join(t.TempDir(), "download")), &got)
			} else {
				s, err = openSession(f.c, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer s.close()
				got, err = s.fetch(t.Context(), "", filepath.Join(t.TempDir(), "download"))
			}
			if err != nil || got.ID != latest.ID {
				t.Fatalf("fetch: %+v %#v", got, err)
			}
			restored, err := backup.Restore(t.Context(), got.Path, filepath.Join(t.TempDir(), "inactive"))
			if err != nil || restored.State != "inactive" {
				t.Fatal(restored, err)
			}
		})
	}
}
