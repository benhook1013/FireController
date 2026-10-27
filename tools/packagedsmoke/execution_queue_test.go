//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

// Actual daemon HTTP configuration is sufficient for visible enrollment/order,
// even while both independent GitHub observation lanes are deliberately blocked.
func TestPackagedExecutionQueueEnrollmentSmoke(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX executable fixture and child CA trust qualified on Linux")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	private := privateFixtureDirectory(t)
	bin := os.Getenv("FC_SMOKE_BIN_DIR")
	if bin == "" {
		bin, _ = filepath.Abs("../../bin")
	}
	daemonBinary := filepath.Join(bin, "fcd")
	beforeHash := binaryHash(t, daemonBinary)
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(secret)
	tokenFile := filepath.Join(private, "provider.token")
	if err := os.WriteFile(tokenFile, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	provider := newRunSelectionHTTPSFixture(t, private, token)
	stub := filepath.Join(private, "never-run")
	stubLog := filepath.Join(private, "unexpected-command")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf 'unexpected\\n' >> "+runSelectionShellQuote(stubLog)+"\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(private, "catalog.json")
	raw, _ := json.Marshal(map[string]any{"version": 1, "capabilities": []any{map[string]any{"id": "queue-smoke", "account": "synthetic-account", "organization": "synthetic-org", "channels": []string{"cli"}, "github_credential_file": tokenFile, "git_executable": stub, "cli_executable": stub, "authentication_environment": map[string]string{}}}})
	if err := os.WriteFile(catalog, raw, 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + provider.proxy, "HTTPS_PROXY=" + provider.proxy, "NO_PROXY=127.0.0.1,localhost", "SSL_CERT_FILE=" + provider.ca, "SSL_CERT_DIR=" + private}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, daemonBinary, data, env)
	adminBytes, err := os.ReadFile(filepath.Join(data, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal("bootstrap credential unavailable")
	}
	admin := strings.TrimSpace(string(adminBytes))
	bootstrap.stop(t)
	project, worker, _, _ := seedAccess(t, ctx, data, bootstrap.instance, admin, private)
	gate := provider.holdNextObserver(t)
	daemon := startDaemon(t, ctx, daemonBinary, data, env, "--review-catalog", catalog)
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	prefix := "http://" + daemon.address + "/api/v1/projects/" + project + "/reviews"
	call := func(method, path, key string, body []byte) []byte {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, method, prefix+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+admin)
		r.Header.Set("X-FC-Instance", bootstrap.instance)
		r.Header.Set("X-FC-Worker", worker)
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
			r.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal("packaged queue request failed", err)
		}
		defer response.Body.Close()
		out, err := io.ReadAll(io.LimitReader(response.Body, 128<<10))
		if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
			t.Fatalf("queue request status %d", response.StatusCode)
		}
		if bytes.Contains(out, adminBytes) || bytes.Contains(out, []byte(token)) {
			t.Fatal("credential disclosed")
		}
		return out
	}
	configure := func(revision int64, numbers []int64) ([]byte, []byte) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"expected_revision": revision, "capability_id": "queue-smoke", "mode": "independent", "queue": numbers, "channels": []string{"cli"}, "paused": true})
		key := "queue-config-" + string(rune('a'+revision))
		return body, call("POST", "/execution/configuration", key, body)
	}
	readQueue := func(numbers []int64) reviews.QueueResponse {
		t.Helper()
		var q reviews.QueueResponse
		decode(t, call("GET", "/queue?limit=100", "", nil), &q)
		got := []int64{}
		for _, p := range q.Items {
			got = append(got, p.Number)
			if p.HeadSHA != "" || p.State != "unknown" || p.RunCount != 0 {
				t.Fatal("blocked observer invented source/review facts")
			}
		}
		if !reflect.DeepEqual(got, numbers) {
			t.Fatalf("visible queue %v want %v", got, numbers)
		}
		return q
	}
	configure(0, []int64{2})
	readQueue([]int64{2})
	gate.awaitStarted(t, ctx)
	configure(1, []int64{2, 3})
	readQueue([]int64{2, 3})
	body, receipt := configure(2, []int64{3, 2})
	last := readQueue([]int64{3, 2})
	if !bytes.Equal(receipt, call("POST", "/execution/configuration", "queue-config-c", body)) || readQueue([]int64{3, 2}).SnapshotID != last.SnapshotID {
		t.Fatal("configuration replay published another snapshot")
	}
	daemon.stop(t)
	gate.awaitCancelled(t, ctx)
	if _, err := os.Stat(stubLog); !os.IsNotExist(err) {
		t.Fatal("configuration launched provider process", err)
	}
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_evidence", "review_round_completions"} {
		var count int
		if err := store.DB().QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatal("configuration started review work", table, count, err)
		}
	}
	if binaryHash(t, daemonBinary) != beforeHash {
		t.Fatal("daemon binary changed")
	}
	t.Logf("packaged single-queue enrollment/insertion/reorder/replay passed in %s; no display write, provider dispatch or owned process remains", time.Since(started).Round(time.Millisecond))
}
