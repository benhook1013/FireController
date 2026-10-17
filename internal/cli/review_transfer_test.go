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
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/reviews"
)

func transferCLIRequest() reviews.TransferRequest {
	zero := int64(0)
	pull := reviews.PullInput{Number: 7, Title: "Selected transferred PR", State: "merged", CIState: "unknown", Mergeability: "unknown", Runs: []reviews.RunInput{{SourceRunID: "overlap-run", Channel: "manual", Scope: "selected history", Outcome: "unknown", Found: 1, Findings: []reviews.Finding{{SourceFindingID: "overlap-finding", Title: "Retained finding", Detail: "PRIVATE_TRANSFER_DETAIL", Severity: "unknown", Disposition: "unadjudicated"}}}}, Attempts: []reviews.Attempt{{SourceAttemptID: "overlap-attempt", Channel: "manual", State: "unknown", SourceRunID: "overlap-run"}}}
	return reviews.TransferRequest{Version: 1, ExpectedRevision: &zero, RepositoryID: 1, Publisher: "prepared transfer", Groups: []reviews.TransferGroup{{Source: "original-a", Pulls: []reviews.PullInput{pull}}, {Source: "original-b", Pulls: []reviews.PullInput{pull}}}}
}
func transferCLIBytes() []byte { raw, _ := json.Marshal(transferCLIRequest()); return raw }

func TestReviewTransferCLIRealPreviewLostOutputAndExactRecovery(t *testing.T) {
	f, project, worker := importCLISetup(t)
	var requests, applies atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.HasSuffix(r.URL.Path, "/transfer/preview") && r.Header.Get("Idempotency-Key") != "" {
			t.Error("preview carried mutation key")
		}
		if strings.HasSuffix(r.URL.Path, "/transfer/apply") {
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
	file := importCLIFile(t, transferCLIBytes())
	provider := f.resolverHit.Load()
	count := func() string {
		var value string
		if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM review_snapshots)||':'||(SELECT count(*) FROM events)||':'||(SELECT count(*) FROM receipts)`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := count()
	code, out, stderr := mutationInvoke(t, profiles, "admin", "reviews", "transfer-preview", "--file", file)
	var preview client.ReviewTransferPreview
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &preview) != nil || preview.ProjectID != project || preview.Counts != (client.ReviewTransferCounts{Pulls: 1, Runs: 2, Attempts: 2, Findings: 2}) || preview.NativeCoverage || !preview.ImportedHistory || preview.Coverage != "partial" || strings.Contains(out, "PRIVATE_TRANSFER_DETAIL") || requests.Load() != 2 || count() != before {
		t.Fatal("preview boundary changed", code, out, stderr)
	}
	args := []string{"reviews", "transfer-apply", "--file", file, "--preview", preview.PreviewSHA256, "--key", "retained-transfer"}
	code, out, stderr = mutationInvoke(t, profiles, "admin", args...)
	if code != cli.ExitService || out != "" || !strings.Contains(stderr, `"outcome_unknown":true`) || strings.Contains(stderr, "PRIVATE_TRANSFER_DETAIL") || applies.Load() != 1 {
		t.Fatal("lost result not single-shot unknown", code, out, stderr)
	}
	var snapshots int
	_ = f.store.DB().QueryRow(`SELECT count(*) FROM review_snapshots`).Scan(&snapshots)
	if snapshots != 1 {
		t.Fatal("lost response was not committed")
	}
	receipt := mutationSuccess(t, profiles, "admin", args...)
	if receipt["revision"] != float64(1) || receipt["outcome"] != "transferred" || applies.Load() != 2 {
		t.Fatal("retry lost original outcome", receipt)
	}
	saved := count()
	full := append([]string{"--profiles-file", profiles, "--profile", "admin"}, args...)
	var diagnostic bytes.Buffer
	if code = cli.RunContext(t.Context(), full, nil, noteFailWriter{}, &diagnostic); code != cli.ExitConfig || !strings.Contains(diagnostic.String(), `"outcome_unknown":true`) || !strings.Contains(diagnostic.String(), "exact same file contents") || count() != saved {
		t.Fatal("output loss duplicated transfer or lost exact recovery", code, diagnostic.String())
	}
	code, out, stderr = mutationInvoke(t, profiles, "admin", "reviews", "runs", "7")
	if code != 0 || stderr != "" || !strings.Contains(out, "original-a") || !strings.Contains(out, "original-b") || strings.Contains(out, "PRIVATE_TRANSFER_DETAIL") || f.resolverHit.Load() != provider {
		t.Fatal("producer-preserving readback/provider isolation", code, out, stderr)
	}
	wrong := append([]string{}, args...)
	wrong[5] = strings.Repeat("f", 64)
	code, out, stderr = mutationInvoke(t, profiles, "admin", wrong...)
	if code != cli.ExitConflict || out != "" || !strings.Contains(stderr, "import_preview_mismatch") || !strings.Contains(stderr, "inspect the new preview") || strings.Contains(stderr, "outcome_unknown") {
		t.Fatal("known mismatch became ambiguous", code, out, stderr)
	}
	diagnostic.Reset()
	previewArgs := []string{"--profiles-file", profiles, "--profile", "admin", "reviews", "transfer-preview", "--file", file}
	// A fresh preview is stale after the transfer, so use the current revision
	// in the selected file before proving output failure remains a read failure.
	next := transferCLIRequest()
	*next.ExpectedRevision = 1
	raw, _ := json.Marshal(next)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if code = cli.RunContext(t.Context(), previewArgs, nil, noteFailWriter{}, &diagnostic); code != cli.ExitConfig || strings.Contains(diagnostic.String(), "outcome_unknown") {
		t.Fatal("preview output became mutation", code, diagnostic.String())
	}
}

func TestReviewTransferCLIArgumentsAndPrivateFilesBeforeCredentials(t *testing.T) {
	file := importCLIFile(t, transferCLIBytes())
	for _, args := range [][]string{{"transfer-preview"}, {"transfer-preview", "--file", "relative.json"}, {"transfer-preview", "--file", file, "--key", "key"}, {"transfer-preview", "--file", file, "--file", file}, {"transfer-apply", "--file", file}, {"transfer-apply", "--file", file, "--preview", strings.Repeat("B", 64), "--key", "key"}, {"transfer-apply", "--file", file, "--preview", strings.Repeat("b", 64), "--key", "bad\nkey"}, {"transfer-apply", "--file", file, "--preview", strings.Repeat("b", 64), "--preview", strings.Repeat("b", 64), "--key", "key"}, {"transfer-preview", "--file", string([]byte{'/', 255})}} {
		code, out, stderr := run(append([]string{"--profiles-file", "/missing-private-profile", "reviews"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal("syntax reached credentials", code, out, stderr)
		}
	}
	doc := string(transferCLIBytes())
	paths := []string{filepath.Join(t.TempDir(), "missing"), t.TempDir()}
	for _, data := range [][]byte{[]byte(`null`), []byte(`[]`), []byte(strings.Replace(doc, `"version":1`, `"version":1,"version":1`, 1)), []byte(strings.Replace(doc, `"expected_revision":0`, `"expected_revision":null`, 1)), []byte(strings.Replace(doc, `"complete":false`, `"complete":true`, 1)), []byte(strings.Replace(doc, `"complete":false`, `"complete":null`, 1)), []byte(strings.Replace(doc, `"publisher":"prepared transfer"`, `"publisher":"\ud800"`, 1)), []byte(strings.Replace(doc, `"groups":[`, `"groups":null,"ignored":[`, 1)), []byte(strings.Replace(doc, `"source":"original-a"`, `"Source":"original-a"`, 1)), []byte{255}, bytes.Repeat([]byte("x"), (256<<10)+1)} {
		paths = append(paths, importCLIFile(t, data))
	}
	link := filepath.Join(t.TempDir(), "link")
	if os.Symlink(file, link) == nil {
		paths = append(paths, link)
	}
	if runtime.GOOS != "windows" {
		exposed := importCLIFile(t, transferCLIBytes())
		if err := os.Chmod(exposed, 0644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, exposed)
	}
	if program, err := exec.LookPath("mkfifo"); err == nil {
		fifo := filepath.Join(t.TempDir(), "fifo")
		if err = exec.Command(program, fifo).Run(); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, fifo)
	}
	for _, path := range paths {
		code, out, stderr := run([]string{"--profiles-file", "/missing-private-profile", "reviews", "transfer-preview", "--file", path}, nil)
		if code != cli.ExitUsage || out != "" || strings.Contains(stderr, "profiles_") || strings.Contains(stderr, path) || strings.Contains(stderr, "PRIVATE_TRANSFER_DETAIL") {
			t.Fatal("file failure reached credentials or disclosed selected data", code, out, stderr)
		}
	}
	code, out, stderr := run([]string{"--profiles-file", "/missing-private-profile", "--profile", "chosen", "reviews", "transfer-preview", "--file", file}, nil)
	if code != cli.ExitConfig || out != "" || !strings.Contains(stderr, "profiles_") {
		t.Fatal("owner-private valid file failed local admission", code, out, stderr)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var outbuf, errbuf bytes.Buffer
	if code = cli.RunContext(ctx, []string{"reviews", "transfer-preview", "--file", file}, nil, &outbuf, &errbuf); code != cli.ExitCancelled || outbuf.Len() != 0 || strings.Contains(errbuf.String(), "outcome_unknown") {
		t.Fatal("unsent cancellation loaded credentials", code, errbuf.String())
	}
}

func TestReviewTransferCLIOrdinaryFiveSecondBudget(t *testing.T) {
	f, project, worker := importCLISetup(t)
	var calls atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer slow.Close()
	profiles := mutationProfiles(t, f, slow.URL, map[string][2]string{"admin": {project, worker}})
	file := importCLIFile(t, transferCLIBytes())
	start := time.Now()
	code, out, stderr := mutationInvoke(t, profiles, "admin", "reviews", "transfer-preview", "--file", file)
	if code != cli.ExitService || out != "" || !strings.Contains(stderr, "deadline_exceeded") || strings.Contains(stderr, "outcome_unknown") || calls.Load() != 1 || time.Since(start) > 6*time.Second {
		t.Fatal("transfer preview changed ordinary deadline/direct result semantics", code, out, stderr)
	}
}

func TestReviewTransferCLIExactApplyWrapperBeforeCredentials(t *testing.T) {
	req := transferCLIRequest()
	req.Groups = req.Groups[:1]
	req.Groups[0].Pulls[0].Attempts = nil
	encode := func(n int) []byte {
		run := &req.Groups[0].Pulls[0].Runs[0]
		run.Found = 3
		run.Findings = nil
		for i := 0; i < 3; i++ {
			size := 16 << 10
			if i == 2 {
				size = n
			}
			run.Findings = append(run.Findings, reviews.Finding{SourceFindingID: fmt.Sprintf("budget-finding-%d", i), Title: "Bounded finding", Detail: strings.Repeat("z", size), Severity: "unknown", Disposition: "unadjudicated"})
		}
		raw, _ := json.Marshal(req)
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
	wrapper, _ := json.Marshal(struct {
		Preview string          `json:"preview_sha256"`
		Batch   json.RawMessage `json:"batch"`
	}{strings.Repeat("b", 64), document})
	if len(document) > target || len(document) < target-6 || len(wrapper) <= 256<<10 {
		t.Fatal("fixture did not isolate exact wrapper budget", len(document), len(wrapper))
	}
	file := importCLIFile(t, document)
	for _, command := range []string{"transfer-preview", "transfer-apply"} {
		args := []string{"--profiles-file", "/missing-private-profile", "--profile", "chosen", "reviews", command, "--file", file}
		if command == "transfer-apply" {
			args = append(args, "--preview", strings.Repeat("b", 64), "--key", "retained-transfer")
		}
		code, out, stderr := run(args, nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "validation_failed") || !strings.Contains(stderr, "smaller") || strings.Contains(stderr, "profiles_") || strings.Contains(stderr, "outcome_unknown") || strings.Contains(stderr, file) {
			t.Fatal("unapplyable exact file reached credentials or unsafe error", code, out, stderr)
		}
	}
}
