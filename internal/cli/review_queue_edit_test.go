package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/reviews"
)

func queueEditCLIDocument(revision int) []byte {
	return []byte(fmt.Sprintf(`{"expected_revision":%d,"repository_id":1,"source":"deliberate queue editor","queue":[{"number":1,"title":"PRIVATE_SELECTED_METADATA","state":"closed","ci_state":"unknown","mergeability":"unknown"}]}`, revision))
}
func TestReviewQueueEditCLIArgumentsAndPrivateFilesBeforeCredentials(t *testing.T) {
	file := importCLIFile(t, queueEditCLIDocument(0))
	cases := [][]string{
		{"reviews", "queue-edit"},
		{"reviews", "queue-edit", "--file", file},
		{"reviews", "queue-edit", "--file", "relative.json", "--key", "key"},
		{"reviews", "queue-edit", "--file", file, "--key", "key", "--file", file},
		{"reviews", "queue-edit", "--file", file, "--key", "key", "--key", "key"},
		{"reviews", "queue-edit", "--file", file, "--key", "bad\nkey"},
		{"reviews", "queue-edit", "--file", file, "--idempotency-key", "key"},
		{"reviews", "queue-edit", "--file", file, "--key", "key", "--preview", strings.Repeat("a", 64)},
		{"reviews", "queue-edit", "--file", file, "--key", "key", "extra"},
	}
	for _, args := range cases {
		code, out, stderr := run(append([]string{"--profiles-file", "/missing/config"}, args...), map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal("invalid syntax reached credentials", code, out, stderr)
		}
	}
	paths := []string{filepath.Join(t.TempDir(), "missing"), t.TempDir()}
	for _, doc := range [][]byte{
		[]byte(`null`), []byte(`[]`), []byte(`{"expected_revision":0,"repository_id":1,"source":"PRIVATE_INVALID_BODY","queue":null}`),
		[]byte(`{"expected_revision":0,"repository_id":1,"source":"PRIVATE_INVALID_BODY","order":null}`),
		[]byte(`{"expected_revision":0,"repository_id":1,"source":"PRIVATE_INVALID_BODY","order":[]}`),
		[]byte(`{"expected_revision":0,"repository_id":1,"source":"PRIVATE_INVALID_BODY","order":[1,1]}`),
		[]byte(`{"expected_revision":0,"repository_id":1,"source":"PRIVATE_INVALID_BODY","order":[null]}`),
		[]byte(`{"expected_revision":0,"repository_id":1,"source":"PRIVATE_INVALID_BODY","order":[2147483648]}`),
		[]byte(strings.Replace(string(queueEditCLIDocument(0)), `"number":1`, `"number":1,"number":2`, 1)),
		[]byte(strings.Replace(string(queueEditCLIDocument(0)), `PRIVATE_SELECTED_METADATA`, `\ud800`, 1)),
		[]byte(strings.Replace(string(queueEditCLIDocument(0)), `"title":`, `"runs":[],"title":`, 1)),
		[]byte(strings.Replace(string(queueEditCLIDocument(0)), `"title":`, `"base_ref":null,"title":`, 1)),
		bytes.Repeat([]byte("x"), (256<<10)+1),
	} {
		paths = append(paths, importCLIFile(t, doc))
	}
	symlink := filepath.Join(t.TempDir(), "symlink")
	if err := os.Symlink(file, symlink); err == nil {
		paths = append(paths, symlink)
	}
	if runtime.GOOS != "windows" {
		public := importCLIFile(t, queueEditCLIDocument(0))
		if err := os.Chmod(public, 0644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, public)
	}
	if program, err := exec.LookPath("mkfifo"); err == nil {
		fifo := filepath.Join(t.TempDir(), "fifo")
		if err = exec.Command(program, fifo).Run(); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, fifo)
	}
	for _, path := range paths {
		code, out, stderr := run([]string{"--profiles-file", "/missing/config", "reviews", "queue-edit", "--file", path, "--key", "key"}, map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || strings.Contains(stderr, "profiles_") || strings.Contains(stderr, path) || strings.Contains(stderr, "PRIVATE_INVALID_BODY") || strings.Contains(stderr, `\ud800`) {
			t.Fatal("invalid file leaked/reached credentials", code, out, stderr)
		}
	}
	code, out, stderr := run([]string{"--profiles-file", "/missing/config", "reviews", "queue-edit", "--file", file, "--key", "key"}, map[string]string{"FC_PROFILE": "missing"})
	if code != cli.ExitConfig || out != "" || strings.Contains(stderr, "PRIVATE_SELECTED_METADATA") {
		t.Fatal("valid file did not reach profile boundary safely", code, out, stderr)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderrBuffer bytes.Buffer
	code = cli.RunContext(ctx, []string{"reviews", "queue-edit", "--file", file, "--key", "key"}, nil, &stdout, &stderrBuffer)
	if code != cli.ExitCancelled || stdout.Len() != 0 {
		t.Fatal("preparation cancellation ignored", code, stderrBuffer.String())
	}
}

func TestReviewQueueEditCLIOrderOnlyActualHTTP(t *testing.T) {
	f, project, worker := importCLISetup(t)
	zero := int64(0)
	f.call(t, "POST", "/api/v1/projects/"+project+"/reviews/observations", f.admin, "", reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 1, Source: "order seed", ObservedAt: "2026-01-01T00:00:00Z", Queue: []reviews.PullInput{{Number: 1, Title: "One", State: "open", CIState: "unknown", Mergeability: "unknown"}, {Number: 2, Title: "Two", State: "open", CIState: "unknown", Mergeability: "unknown"}, {Number: 3, Title: "Three", State: "open", CIState: "unknown", Mergeability: "unknown"}}})
	document := []byte(`{"expected_revision":1,"repository_id":1,"source":"order-only","order":[3,1]}`)
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/queue/edits") {
			calls.Add(1)
			raw, err := io.ReadAll(r.Body)
			if err != nil || !bytes.Equal(raw, document) || r.Header.Get("X-FC-Worker") != worker || r.Header.Get("Idempotency-Key") != "order-only" {
				t.Error("order-only CLI wire changed", err)
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"admin": {project, worker}})
	file := importCLIFile(t, document)
	provider := f.resolverHit.Load()
	receipt := mutationSuccess(t, profiles, "admin", "reviews", "queue-edit", "--file", file, "--key", "order-only")
	if receipt["revision"] != float64(2) || receipt["outcome"] != "queue_edited" || calls.Load() != 1 {
		t.Fatal(receipt, calls.Load())
	}
	rows, err := f.store.DB().Query(`SELECT number FROM review_current_pulls WHERE project_id=? ORDER BY position`, project)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for _, n := range []int64{3, 2, 1} {
		var got int64
		if !rows.Next() || rows.Scan(&got) != nil || got != n {
			t.Fatal("CLI order not applied", got, n)
		}
	}
	if rows.Next() || rows.Err() != nil || f.resolverHit.Load() != provider {
		t.Fatal("unexpected rows or provider work")
	}
}

func TestReviewQueueEditCLIRealAPILostReceiptOutputAndReplay(t *testing.T) {
	f, project, worker := importCLISetup(t)
	zero := int64(0)
	f.call(t, "POST", "/api/v1/projects/"+project+"/reviews/observations", f.admin, "", reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 1, Source: "original source", ObservedAt: "2026-01-01T00:00:00Z", Queue: []reviews.PullInput{{Number: 2, Title: "Retained merged PR", State: "merged", CIState: "success", Mergeability: "mergeable"}}})
	var editCalls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reviews/queue/edits") {
			if r.Method != "POST" || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("X-FC-Worker") != worker || r.Header.Get("Idempotency-Key") == "" || r.URL.RawQuery != "" || r.Header.Get("Prefer") != "" {
				t.Error("queue edit CLI wire/binding changed")
			}
			response := httptest.NewRecorder()
			f.handler.ServeHTTP(response, r)
			if response.Code == 201 && editCalls.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(201)
				_, _ = io.WriteString(w, `{"id":`)
				return
			}
			for key, values := range response.Header() {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(response.Code)
			_, _ = w.Write(response.Body.Bytes())
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"admin": {project, worker}})
	file := importCLIFile(t, queueEditCLIDocument(1))
	fileBefore, _ := os.ReadFile(file)
	profileBefore, _ := os.ReadFile(profiles)
	args := []string{"reviews", "queue-edit", "--file", file, "--key", "original-edit"}
	provider := f.resolverHit.Load()
	code, out, stderr := mutationInvoke(t, profiles, "admin", args...)
	if code != cli.ExitService || out != "" || !strings.Contains(stderr, `"outcome_unknown":true`) || strings.Contains(stderr, "PRIVATE_SELECTED_METADATA") || editCalls.Load() != 1 {
		t.Fatal("lost receipt/retry contract", code, out, stderr)
	}
	receipt := mutationSuccess(t, profiles, "admin", args...)
	if receipt["revision"] != float64(2) || receipt["outcome"] != "queue_edited" || editCalls.Load() != 2 || len(receipt) != 3 {
		t.Fatal("compact exact replay failed", receipt)
	}
	// One deliberate later edit commits, even when its stdout is unavailable.
	nextFile := importCLIFile(t, []byte(`{"expected_revision":2,"repository_id":1,"source":"retirement editor","retired":[{"number":1,"reason":"defunct"}]}`))
	next := []string{"reviews", "queue-edit", "--file", nextFile, "--key", "later-edit"}
	var errOut bytes.Buffer
	code = cli.RunContext(t.Context(), append([]string{"--profiles-file", profiles}, next...), map[string]string{"FC_PROFILE": "admin"}, failedMutationOutput{}, &errOut)
	if code != cli.ExitConfig || !strings.Contains(errOut.String(), `"outcome_unknown":true`) || !strings.Contains(errOut.String(), "exact same") || strings.Contains(errOut.String(), "PRIVATE_OUTPUT_DESTINATION") {
		t.Fatal("failed output lost recovery guidance", code, errOut.String())
	}
	later := mutationSuccess(t, profiles, "admin", next...)
	if later["revision"] != float64(3) {
		t.Fatal(later)
	}
	replay := mutationSuccess(t, profiles, "admin", args...)
	if replay["id"] != receipt["id"] || replay["revision"] != float64(2) {
		t.Fatal("replay after advancement failed", replay)
	}
	code, out, stderr = mutationInvoke(t, profiles, "admin", "reviews", "queue")
	if code != 0 || stderr != "" || !strings.Contains(out, "Retained merged PR") || strings.Contains(out, "PRIVATE_SELECTED_METADATA") {
		t.Fatal("omitted merged history/retirement changed", code, out, stderr)
	}
	stale := append([]string{}, args...)
	stale[len(stale)-1] = "stale-new-key"
	code, out, stderr = mutationInvoke(t, profiles, "admin", stale...)
	if code != cli.ExitConflict || out != "" || strings.Contains(stderr, "outcome_unknown") {
		t.Fatal("known CAS failure became uncertain", code, out, stderr)
	}
	var snapshots, receipts int
	if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM review_snapshots),(SELECT count(*) FROM receipts WHERE operation='reviews.queue.edit')`).Scan(&snapshots, &receipts); err != nil || snapshots != 3 || receipts != 2 {
		t.Fatalf("replay created extra rows %d/%d %v", snapshots, receipts, err)
	}
	fileAfter, _ := os.ReadFile(file)
	profileAfter, _ := os.ReadFile(profiles)
	if !bytes.Equal(fileBefore, fileAfter) || !bytes.Equal(profileBefore, profileAfter) || f.resolverHit.Load() != provider {
		t.Fatal("command mutated inputs/profile or performed repository lookup")
	}
	var decoded client.ReviewQueueEditReceipt
	encoded, _ := json.Marshal(receipt)
	if json.Unmarshal(encoded, &decoded) != nil || decoded.ID == "" {
		t.Fatal("receipt was not typed compact output")
	}
}
