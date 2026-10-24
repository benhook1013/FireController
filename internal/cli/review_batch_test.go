package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewBatchCLIPrivateStrictFileBeforeConnection(t *testing.T) {
	valid := `{"expected_revision":0,"items":[{"finding":0,"disposition":"accepted","reason":"PRIVATE_REASON"}]}`
	file := importCLIFile(t, []byte(valid))
	paths := []string{t.TempDir(), filepath.Join(t.TempDir(), "absent")}
	for _, body := range []string{`null`, strings.Replace(valid, `"finding":0,`, "", 1), strings.Replace(valid, `"finding":0`, `"finding":0,"finding":1`, 1), strings.Replace(valid, `PRIVATE_REASON`, `\ud800`, 1), strings.Replace(valid, `"reason":"PRIVATE_REASON"`, `"reason":"PRIVATE_REASON","extra":true`, 1), strings.Repeat("x", client.ReviewBatchMaxBytes+1)} {
		paths = append(paths, importCLIFile(t, []byte(body)))
	}
	link := filepath.Join(t.TempDir(), "link")
	if os.Symlink(file, link) == nil {
		paths = append(paths, link)
	}
	if runtime.GOOS != "windows" {
		public := importCLIFile(t, []byte(valid))
		if err := os.Chmod(public, 0644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, public)
	}
	for _, path := range paths {
		code, out, diagnostic := run([]string{"--profiles-file", "/missing/config", "reviews", "decide-batch", cliEvidenceID, "--file", path, "--key", "explicit"}, map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || strings.Contains(diagnostic, "profiles_") || strings.Contains(diagnostic, path) || strings.Contains(diagnostic, "PRIVATE_REASON") {
			t.Fatal("invalid batch file leaked or loaded connection", code, diagnostic)
		}
	}
	for _, extra := range [][]string{{"--file", file, "--file", file, "--key", "explicit"}, {"--file", file}, {"--file", file, "--key", "explicit", "--accept-all"}, {"--file", file, "--key", "explicit", "extra"}} {
		args := append([]string{"--profiles-file", "/missing/config", "reviews", "decide-batch", cliEvidenceID}, extra...)
		code, _, diagnostic := run(args, map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || strings.Contains(diagnostic, "profiles_") {
			t.Fatal("invalid batch syntax loaded connection", code, diagnostic)
		}
	}
	code, _, diagnostic := run([]string{"--profiles-file", "/missing/config", "reviews", "decide-batch", cliEvidenceID, "--file", file, "--key", "explicit"}, map[string]string{"FC_PROFILE": "missing"})
	if code != cli.ExitConfig || strings.Contains(diagnostic, "PRIVATE_REASON") {
		t.Fatal("valid batch did not reach profile safely", code, diagnostic)
	}
}

func TestReviewBatchCLISendsOneExplicitRequestAndCompactReceipt(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "batch-cli")
	worker := f.worker(t, project, "Builder")
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		calls.Add(1)
		if r.Header.Get("X-FC-Worker") != worker || r.Header.Get("Idempotency-Key") != "retained-key" {
			t.Error("CLI batch lost binding/key")
		}
		state := "adjudicated_batch"
		if strings.HasSuffix(r.URL.Path, "/fixes/batch") {
			state = "fixes_recorded_batch"
		}
		var request map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&request) != nil || len(request) != 2 || string(request["expected_revision"]) != "0" {
			t.Error("CLI batch invented request fields")
		}
		_ = json.NewEncoder(w).Encode(client.ReviewControlReceipt{ID: cliEvidenceID, State: state, Revision: 1})
	}))
	defer server.Close()
	profiles := mutationProfiles(t, f, server.URL, map[string][2]string{"builder": {project, worker}})
	for _, operation := range []string{"decide-batch", "fix-batch"} {
		body := `{"expected_revision":0,"items":[{"finding":0,"disposition":"accepted","reason":"PRIVATE_REASON"}]}`
		if operation == "fix-batch" {
			body = `{"expected_revision":0,"items":[{"finding":0,"expected_sha":"","fix_sha":"` + strings.Repeat("a", 40) + `","proof_note":"PRIVATE_PROOF"}]}`
		}
		file := importCLIFile(t, []byte(body))
		receipt := mutationSuccess(t, profiles, "builder", "reviews", operation, cliEvidenceID, "--file", file, "--key", "retained-key")
		if len(receipt) != 3 || strings.Contains(encodeString(receipt), "PRIVATE_") {
			t.Fatal("batch output expanded private inputs")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("batch adapter retried or looped individual mutations", calls.Load())
	}
}
