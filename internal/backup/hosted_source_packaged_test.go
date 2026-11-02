//go:build packaged_smoke

package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
)

// Following tools/packagedsmoke's supplied-binary/private-fixture pattern, trusted
// setup calls the real retainer before starting fcd. This proves packaged
// retention/read/restart/backup, not provider collection by the daemon.
func TestPackagedHostedSubmittedSourceRetention(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owned process stop uses POSIX interrupt; Linux packaged boundary")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	bin := os.Getenv("FC_SMOKE_BIN_DIR")
	if !filepath.IsAbs(bin) {
		t.Fatal("FC_SMOKE_BIN_DIR must select prebuilt absolute inputs")
	}
	fcd, fc := filepath.Join(bin, "fcd"), filepath.Join(bin, "fc")
	hash := func(p string) [32]byte {
		t.Helper()
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal("packaged input unavailable")
		}
		return sha256.Sum256(b)
	}
	hashes := [][32]byte{hash(fcd), hash(fc)}
	f, evidence, capturePath := submittedReviewInventoryFixture(t)
	original, err := os.ReadFile(filepath.Join(f.dir, capturePath))
	if err != nil {
		t.Fatal(err)
	}
	var retainedBody, retainedFingerprint string
	if err := f.store.DB().QueryRow(`SELECT body_json,fingerprint FROM review_execution_evidence WHERE id=?`, evidence.ID).Scan(&retainedBody, &retainedFingerprint); err != nil {
		t.Fatal(err)
	}
	instance := f.store.InstanceID()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	private, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	defer deny.Close()
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	credential := filepath.Join(private, "credential")
	if err := os.WriteFile(credential, []byte(f.token), 0600); err != nil {
		t.Fatal(err)
	}
	var originalRead []byte
	for pass := 0; pass < 2; pass++ {
		command := exec.CommandContext(ctx, fcd, "--data-dir", f.dir, "--listen", "127.0.0.1:0")
		command.Env = env
		command.WaitDelay = time.Second
		stdout, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		command.Stderr = &stderr
		if err := command.Start(); err != nil {
			t.Fatal("packaged daemon failed to start")
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		var once sync.Once
		stop := func() {
			once.Do(func() {
				_ = command.Process.Signal(os.Interrupt)
				select {
				case err := <-done:
					if err != nil {
						t.Error("packaged daemon stop failed")
					}
				case <-time.After(5 * time.Second):
					_ = command.Process.Kill()
					<-done
					t.Error("packaged daemon stop exceeded bound")
				}
			})
		}
		t.Cleanup(stop)
		type listening struct {
			Event      string `json:"event"`
			Address    string `json:"address"`
			InstanceID string `json:"instance_id"`
		}
		ready := make(chan listening, 1)
		go func() { var v listening; _ = json.NewDecoder(io.LimitReader(stdout, 64<<10)).Decode(&v); ready <- v }()
		var record listening
		select {
		case record = <-ready:
		case <-time.After(5 * time.Second):
			stop()
			t.Fatal("packaged daemon readiness exceeded bound")
		}
		host, _, err := net.SplitHostPort(record.Address)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || record.Event != "listening" || record.InstanceID != instance {
			stop()
			t.Fatal("invalid packaged listening identity")
		}
		profiles := filepath.Join(private, "profiles.json")
		profileBody, _ := json.Marshal(map[string]any{"version": 1, "profiles": map[string]any{"source": map[string]any{"server_url": "http://" + record.Address, "instance_id": instance, "project_id": f.project, "worker_id": f.worker, "credential_file": credential}}})
		if err := os.WriteFile(profiles, profileBody, 0600); err != nil {
			t.Fatal(err)
		}
		readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
		call := exec.CommandContext(readCtx, fc, "--profiles-file", profiles, "--profile", "source", "reviews", "evidence", evidence.ID)
		call.Env = env
		output, err := call.Output()
		readCancel()
		if err != nil {
			stop()
			t.Fatal("actual CLI submitted evidence read failed")
		}
		var got client.ReviewEvidence
		if json.Unmarshal(output, &got) != nil || got.Capture.Hosted == nil || got.Capture.Hosted.SubmittedReview == nil || got.Capture.Hosted.SubmittedReview.ProviderRunID != evidence.Capture.Hosted.SubmittedReview.ProviderRunID || got.Capture.Hosted.SubmittedReview.AcknowledgementCommentID != 400 || got.Capture.Hosted.PublicationID != "" || got.Capture.Hosted.ProviderAttemptID != "" || !reflect.DeepEqual(got.Capture.Hosted.ReviewIDs, evidence.Capture.Hosted.ReviewIDs) || len(got.Capture.Findings) != 1 || got.Capture.Findings[0].Detail != evidence.Capture.Review.Findings[0].Detail {
			stop()
			t.Fatal("packaged evidence lost real identities or original finding")
		}
		if pass == 0 {
			originalRead = append([]byte(nil), output...)
		} else if !bytes.Equal(originalRead, output) {
			stop()
			t.Fatal("restart changed exact evidence response")
		}
		stop()
	}
	local := func(args ...string) []byte {
		t.Helper()
		bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		command := exec.CommandContext(bounded, fcd, args...)
		command.Env = env
		output, err := command.Output()
		if err != nil {
			t.Fatal("packaged local backup operation failed")
		}
		return output
	}
	var bundle Summary
	if json.Unmarshal(local("backup", "--data-dir", f.dir, "--destination", filepath.Join(private, "bundles")), &bundle) != nil || bundle.State != "complete" {
		t.Fatal("packaged backup failed")
	}
	var inspected Summary
	if json.Unmarshal(local("backup-inspect", "--bundle", bundle.Path), &inspected) != nil || inspected.State != "verified" {
		t.Fatal("packaged inspect failed")
	}
	destination := filepath.Join(private, "restored")
	var restored RestoreSummary
	if json.Unmarshal(local("restore", "--bundle", bundle.Path, "--data-dir", destination), &restored) != nil || restored.State != "inactive" || restored.InstanceID == instance {
		t.Fatal("packaged inactive restore failed")
	}
	for _, dir := range []string{f.dir, destination} {
		raw, err := os.ReadFile(filepath.Join(dir, capturePath))
		if err != nil || !bytes.Equal(raw, original) {
			t.Fatal("packaged operations changed original capture bytes")
		}
		store, err := core.Open(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		var body, fingerprint string
		err = store.DB().QueryRow(`SELECT body_json,fingerprint FROM review_execution_evidence WHERE id=?`, evidence.ID).Scan(&body, &fingerprint)
		if err != nil || body != retainedBody || fingerprint != retainedFingerprint {
			store.Close()
			t.Fatal("packaged operations changed immutable ledger")
		}
		if dir == destination {
			backupCode(t, StartupGuard(ctx, store), "restore_inactive")
			var active int
			if store.DB().QueryRow(`SELECT count(*) FROM credentials WHERE revoked<>1`).Scan(&active) != nil || active != 0 {
				store.Close()
				t.Fatal("restore revived old credentials")
			}
		}
		_ = store.Close()
	}
	if external.Load() != 0 || hash(fcd) != hashes[0] || hash(fc) != hashes[1] {
		t.Fatal("packaged proof contacted provider or changed supplied binaries")
	}
	t.Logf("actual fcd/fc submitted retention, restart, backup/inspect/inactive restore passed; fcd=%x fc=%x external_http=0", hashes[0], hashes[1])
}
