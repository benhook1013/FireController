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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/reviews"
)

func importCLIFile(t *testing.T, document []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prepared-private.json")
	if err := os.WriteFile(path, document, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func importCLIDocument() []byte {
	return []byte(`{"expected_revision":0,"repository_id":1,"source":"prepared source","observed_at":"2026-10-15T00:00:00Z","queue":[],"retired":[]}`)
}
func importCLISetup(t *testing.T) (*serviceFixture, string, string) {
	t.Helper()
	f := setupServiceFixture(t)
	project := f.project(t, "prepared-review")
	worker := f.worker(t, project, "Importer")
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	reviews.Register(mux, f.store)
	f.handler = mux
	return f, project, worker
}

func TestReviewImportCLIPublicPreviewLostResultAndExactRecovery(t *testing.T) {
	f, project, worker := importCLISetup(t)
	var requests, applies atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.HasSuffix(r.URL.Path, "/import/preview") && r.Header.Get("Idempotency-Key") != "" {
			t.Error("preview has mutation key")
		}
		if strings.HasSuffix(r.URL.Path, "/import/apply") {
			recorded := httptest.NewRecorder()
			f.handler.ServeHTTP(recorded, r)
			if recorded.Code == 201 && applies.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(201)
				_, _ = io.WriteString(w, `{"id":`)
				return
			}
			for key, values := range recorded.Header() {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"admin": {project, worker}})
	doc := reviewObservation()
	document, _ := json.MarshalIndent(doc, "", " ")
	file := importCLIFile(t, document)
	provider := f.resolverHit.Load()
	var beforeReceipts, beforeEvents int
	f.store.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&beforeReceipts)
	f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&beforeEvents)
	code, out, stderr := mutationInvoke(t, profiles, "admin", "reviews", "import-preview", "--file", file)
	var preview client.ReviewImportPreview
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &preview) != nil || preview.ProjectID != project || preview.Counts != (client.ReviewImportCounts{Pulls: 6, Runs: 6, Attempts: 1, Findings: 1}) || !preview.ImportedHistory || preview.NativeCoverage || strings.Contains(out, "PRIVATE_FINDING_DETAIL") || requests.Load() != 2 {
		t.Fatal(code, out, stderr)
	}
	var receipts, events int
	f.store.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&receipts)
	f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events)
	if receipts != beforeReceipts || events != beforeEvents {
		t.Fatal("preview wrote durable state")
	}
	args := []string{"reviews", "import-apply", "--file", file, "--preview", preview.PreviewSHA256, "--key", "retained-import"}
	code, out, stderr = mutationInvoke(t, profiles, "admin", args...)
	if code != cli.ExitService || out != "" || !strings.Contains(stderr, `"outcome_unknown":true`) || strings.Contains(stderr, "PRIVATE_FINDING_DETAIL") {
		t.Fatal("lost committed result not unknown", code, out, stderr)
	}
	var snapshotCount int
	f.store.DB().QueryRow(`SELECT count(*) FROM review_snapshots`).Scan(&snapshotCount)
	if snapshotCount != 1 {
		t.Fatal("apply was not actually committed", snapshotCount)
	}
	receipt := mutationSuccess(t, profiles, "admin", args...)
	if receipt["revision"] != float64(1) || receipt["outcome"] != "observed" || applies.Load() != 2 {
		t.Fatal(receipt)
	}
	f.store.DB().QueryRow(`SELECT count(*) FROM review_snapshots`).Scan(&snapshotCount)
	if snapshotCount != 1 {
		t.Fatal("recovery duplicated snapshot")
	}
	code, out, stderr = mutationInvoke(t, profiles, "admin", "reviews", "queue")
	if code != 0 || stderr != "" || !strings.Contains(out, "Stored PR 1") || f.resolverHit.Load() != provider {
		t.Fatal("import readback/provider isolation", code, out, stderr)
	}
	wrong := append([]string{}, args...)
	wrong[5] = strings.Repeat("f", 64)
	code, out, stderr = mutationInvoke(t, profiles, "admin", wrong...)
	if code != cli.ExitConflict || out != "" || !strings.Contains(stderr, "import_preview_mismatch") || !strings.Contains(stderr, "inspect the new preview") || strings.Contains(stderr, "outcome_unknown") {
		t.Fatal("known mismatch classified as ambiguous", code, out, stderr)
	}
}

func TestReviewImportCLIArgumentsAndFilesBeforeCredentials(t *testing.T) {
	validFile := importCLIFile(t, importCLIDocument())
	cases := [][]string{{"reviews", "import-preview"}, {"reviews", "import-preview", "--file", "relative.json"}, {"reviews", "import-preview", "--file", validFile, "--key", "key"}, {"reviews", "import-preview", "--file", validFile, "--file", validFile}, {"reviews", "import-apply", "--file", validFile}, {"reviews", "import-apply", "--file", validFile, "--preview", strings.Repeat("B", 64), "--key", "key"}, {"reviews", "import-apply", "--file", validFile, "--preview", strings.Repeat("b", 64), "--key", "bad\nkey"}, {"reviews", "import-preview", "--file", string([]byte{'/', 255})}}
	for _, args := range cases {
		code, out, stderr := run(append([]string{"--profiles-file", "/missing-credential-config"}, args...), map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal(args, code, out, stderr)
		}
	}
	documents := [][]byte{[]byte(`null`), []byte(`[]`), []byte(`{"expected_revision":0,"repository_id":1,"source":"x","observed_at":"2026-10-15T00:00:00Z","queue":[{"x":1,"x":2}]}`), []byte(`{"expected_revision":0,"repository_id":1,"source":"\ud800","observed_at":"2026-10-15T00:00:00Z"}`), []byte(`{"Expected_revision":0}`), bytes.Repeat([]byte("x"), (256<<10)+1)}
	paths := []string{filepath.Join(t.TempDir(), "missing-private.json"), t.TempDir()}
	for _, doc := range documents {
		paths = append(paths, importCLIFile(t, doc))
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(validFile, link); err == nil {
		paths = append(paths, link)
	}
	if program, err := exec.LookPath("mkfifo"); err == nil {
		fifo := filepath.Join(t.TempDir(), "fifo")
		if err = exec.Command(program, fifo).Run(); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, fifo)
	}
	for _, file := range paths {
		code, out, stderr := run([]string{"--profiles-file", "/missing-credential-config", "reviews", "import-preview", "--file", file}, map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || strings.Contains(stderr, "profiles_") || strings.Contains(stderr, file) || strings.Contains(stderr, `\ud800`) {
			t.Fatal("file failure reached config or leaked source", code, out, stderr)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out, stderr bytes.Buffer
	code := cli.RunContext(ctx, []string{"reviews", "import-preview", "--file", validFile}, nil, &out, &stderr)
	if code != cli.ExitCancelled || out.Len() != 0 || strings.Contains(stderr.String(), "outcome_unknown") {
		t.Fatal("pre-cancelled import reached credentials", code, stderr.String())
	}
}

func TestReviewImportCLIOutputFailureAndPreviewDeadlineStayDistinct(t *testing.T) {
	f, project, worker := importCLISetup(t)
	service := httptest.NewServer(f.handler)
	defer service.Close()
	profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"admin": {project, worker}})
	file := importCLIFile(t, importCLIDocument())
	var stderr bytes.Buffer
	args := []string{"--profiles-file", profiles, "--profile", "admin", "reviews", "import-preview", "--file", file}
	if code := cli.RunContext(t.Context(), args, nil, noteFailWriter{}, &stderr); code != cli.ExitConfig || strings.Contains(stderr.String(), "outcome_unknown") {
		t.Fatal("preview output became mutation", code, stderr.String())
	}
	code, out, errOut := mutationInvoke(t, profiles, "admin", "reviews", "import-preview", "--file", file)
	var preview client.ReviewImportPreview
	if code != 0 || errOut != "" || json.Unmarshal([]byte(out), &preview) != nil {
		t.Fatal(code, out, errOut)
	}
	args = []string{"--profiles-file", profiles, "--profile", "admin", "reviews", "import-apply", "--file", file, "--preview", preview.PreviewSHA256, "--key", "failed-output"}
	stderr.Reset()
	if code = cli.RunContext(t.Context(), args, nil, noteFailWriter{}, &stderr); code != cli.ExitConfig || !strings.Contains(stderr.String(), `"outcome_unknown":true`) || !strings.Contains(stderr.String(), "exact same file contents") {
		t.Fatal("apply output lost recovery", code, stderr.String())
	}
	mutationSuccess(t, profiles, "admin", args[4:]...)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer slow.Close()
	slowProfiles := mutationProfiles(t, f, slow.URL, map[string][2]string{"admin": {project, worker}})
	begin := time.Now()
	code, out, errOut = mutationInvoke(t, slowProfiles, "admin", "reviews", "import-preview", "--file", file)
	if code != cli.ExitService || out != "" || !strings.Contains(errOut, "deadline_exceeded") || strings.Contains(errOut, "outcome_unknown") || time.Since(begin) > 6*time.Second {
		t.Fatal("normal preview budget/cancellation classification changed", code, out, errOut)
	}
}

func TestReviewImportCLIPreviewsBudgetSameFileApplyBeforeCredentials(t *testing.T) {
	zero := int64(0)
	req := reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 1, Source: "prepared export", ObservedAt: "2026-10-15T00:00:00Z", Queue: []reviews.PullInput{{Number: 1, Title: "Bounded imported history", State: "open", CIState: "unknown", Mergeability: "unknown", Runs: []reviews.RunInput{{SourceRunID: "budget-run", Channel: "manual", Scope: "selected history", Outcome: "unknown", Found: 3}}}}}
	encode := func(n int) []byte {
		req.Queue[0].Runs[0].Findings = make([]reviews.Finding, 3)
		for i := 0; i < 3; i++ {
			size := 16 << 10
			if i == 2 {
				size = n
			}
			req.Queue[0].Runs[0].Findings[i] = reviews.Finding{SourceFindingID: fmt.Sprintf("budget-finding-%d", i), Title: "Retained finding", Detail: strings.Repeat("z", size), Severity: "unknown", Disposition: "unadjudicated"}
		}
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		return bytes.ReplaceAll(raw, []byte("z"), []byte(`\u007a`))
	}
	target := (256 << 10) - 32
	low, high := 0, 16<<10
	for low < high {
		mid := (low + high + 1) / 2
		if len(encode(mid)) <= target {
			low = mid
		} else {
			high = mid - 1
		}
	}
	document := encode(low)
	if len(document) > target || len(document) < target-6 {
		t.Fatal("fixture did not reach wire boundary", len(document))
	}
	file := importCLIFile(t, document)
	for _, command := range []string{"import-preview", "import-apply"} {
		args := []string{"--profiles-file", "/missing-private-profile", "--profile", "chosen", "reviews", command, "--file", file}
		if command == "import-apply" {
			args = append(args, "--preview", strings.Repeat("a", 64), "--key", "retained-import")
		}
		code, out, stderr := run(args, nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "validation_failed") || !strings.Contains(stderr, "smaller") || strings.Contains(stderr, "profiles_") || strings.Contains(stderr, "outcome_unknown") || strings.Contains(stderr, file) {
			t.Fatal("unapplyable file reached credentials or unsafe error", command, code, out, stderr)
		}
	}
}
