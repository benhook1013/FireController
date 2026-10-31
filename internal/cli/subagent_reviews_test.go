package cli_test

import (
	"bytes"
	"context"
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

const subagentCLIRunID = "review_run_0123456789abcdef0123456789abcdef"

func subagentCLIRecord() []byte {
	return []byte(`{"pull_number":7,"head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reviewer":"Sol","scope":"Database invariants","started_at":"2026-10-25T01:00:00Z","finished_at":"2026-10-25T01:01:00Z","findings":[{"title":"Source identity","detail":"PRIVATE_SUBAGENT_TEXT","severity":"high"}]}`)
}
func TestSubagentCLIValidationAndLocalHelpBeforeCredentials(t *testing.T) {
	file := importCLIFile(t, subagentCLIRecord())
	for _, args := range [][]string{
		{"reviews", "subagent-record"}, {"reviews", "subagent-record", "--file", file},
		{"reviews", "subagent-record", "--file", "relative.json", "--key", "key"},
		{"reviews", "subagent-record", "--file", file, "--key", "key", "--file", file},
		{"reviews", "subagent-record", "--file", file, "--key", "key", "--key", "key"},
		{"reviews", "subagent-record", "--file", file, "--key", "bad\nkey"},
		{"reviews", "subagent-read", "bad"}, {"reviews", "subagent-read", subagentCLIRunID, "--snapshot", "anything"},
		{"reviews", "subagent-update", subagentCLIRunID, "--file", file},
	} {
		code, out, errout := run(append([]string{"--profiles-file", "/missing/config"}, args...), map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || !strings.Contains(errout, "invalid_arguments") {
			t.Fatal("invalid grammar reached configuration", code, out, errout)
		}
	}
	paths := []string{filepath.Join(t.TempDir(), "missing"), t.TempDir()}
	for _, body := range [][]byte{[]byte(`null`), []byte(`{}`), []byte(strings.Replace(string(subagentCLIRecord()), `"findings":[`, `"findings":null,"other":[`, 1)), []byte(strings.Replace(string(subagentCLIRecord()), `"pull_number":7,`, "", 1)), bytes.Repeat([]byte("x"), (256<<10)+1)} {
		paths = append(paths, importCLIFile(t, body))
	}
	symlink := filepath.Join(t.TempDir(), "symlink")
	if err := os.Symlink(file, symlink); err == nil {
		paths = append(paths, symlink)
	}
	if runtime.GOOS != "windows" {
		public := importCLIFile(t, subagentCLIRecord())
		if err := os.Chmod(public, 0644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, public)
	}
	for _, path := range paths {
		code, out, errout := run([]string{"--profiles-file", "/missing/config", "reviews", "subagent-record", "--file", path, "--key", "key"}, map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || strings.Contains(errout, "profiles_") || strings.Contains(errout, path) || strings.Contains(errout, "PRIVATE_SUBAGENT_TEXT") {
			t.Fatal("invalid file escaped local admission", code, out, errout)
		}
	}
	for _, operation := range []string{"subagent-record", "subagent-read", "subagent-update"} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var out, errout bytes.Buffer
		code := cli.RunContext(ctx, []string{"--profiles-file", "/missing/config", "reviews", operation, "--help"}, nil, &out, &errout)
		if code != cli.ExitSuccess || errout.Len() != 0 || out.Len() > 4096 || !strings.Contains(out.String(), "docs/subagent-reviews.md") {
			t.Fatal("help used files/network", code, &out, &errout)
		}
	}
	// A valid file reaches profile selection only after local request validation.
	code, out, errout := run([]string{"--profiles-file", "/missing/config", "reviews", "subagent-record", "--file", file, "--key", "key"}, map[string]string{"FC_PROFILE": "missing"})
	if code != cli.ExitConfig || out != "" || strings.Contains(errout, "PRIVATE_SUBAGENT_TEXT") {
		t.Fatal("valid input did not reach profile boundary", code, out, errout)
	}
}

func TestSubagentCLIScopedWireCompactReceiptsAndUnknownOutput(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "subagent-cli")
	worker := f.worker(t, project, "Builder")
	var mutations, reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+f.admin || r.Header.Get("X-FC-Worker") != worker || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.URL.RawQuery != "" {
			t.Error("lost bound identity")
		}
		if r.Method == "GET" {
			reads.Add(1)
			_ = json.NewEncoder(w).Encode(client.SubagentReport{ID: subagentCLIRunID, ProjectID: project, PullNumber: 7, Revision: 17, Run: client.ReviewRunSummary{ID: subagentCLIRunID, PullNumber: 7, Source: "worker-subagent", SourceRunID: "subagent-source", Channel: "subagent", HeadSHA: strings.Repeat("a", 40), Reviewer: "Sol", Scope: "Database invariants", Outcome: "completed", Attributable: true, Complete: true, StartedAt: "2026-10-25T01:00:00Z", FinishedAt: "2026-10-25T01:01:00Z", CoverageLimits: []string{}}, Findings: []client.ReviewFinding{}})
			return
		}
		mutations.Add(1)
		if strings.HasSuffix(r.URL.Path, "/updates") {
			var in client.SubagentUpdateRequest
			if r.Header.Get("Idempotency-Key") != "update-once" || json.NewDecoder(r.Body).Decode(&in) != nil || in.ExpectedRevision == nil || *in.ExpectedRevision != 17 || in.Fixes[0].ExpectedSHA == nil || *in.Fixes[0].ExpectedSHA != "" {
				t.Error("explicit update guard/empty SHA changed")
			}
			_ = json.NewEncoder(w).Encode(client.SubagentReceipt{ID: subagentCLIRunID, State: "updated", Revision: 21})
			return
		}
		var in client.SubagentRecordRequest
		if r.Header.Get("Idempotency-Key") != "record-once" || json.NewDecoder(r.Body).Decode(&in) != nil || in.Findings[0].Detail != "PRIVATE_SUBAGENT_TEXT" {
			t.Error("record request changed")
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.SubagentReceipt{ID: subagentCLIRunID, State: "recorded", Revision: 17})
	}))
	defer service.Close()
	profilePath := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	record := []string{"reviews", "subagent-record", "--file", importCLIFile(t, subagentCLIRecord()), "--key", "record-once"}
	for i := 0; i < 2; i++ {
		code, out, errout := mutationInvoke(t, profilePath, "builder", record...)
		var receipt map[string]any
		if code != 0 || errout != "" || json.Unmarshal([]byte(out), &receipt) != nil || len(receipt) != 3 || receipt["id"] != subagentCLIRunID || strings.Contains(out, "PRIVATE_SUBAGENT_TEXT") {
			t.Fatal("record expanded result or failed", code, out, errout)
		}
	}
	updateFile := importCLIFile(t, []byte(`{"expected_revision":17,"decisions":[{"finding":0,"disposition":"accepted","reason":"Verified"}],"fixes":[{"finding":0,"expected_sha":"","fix_sha":"ffffffffffffffffffffffffffffffffffffffff","proof_note":"Focused proof"}],"finalize":true}`))
	code, out, errout := mutationInvoke(t, profilePath, "builder", "reviews", "subagent-update", subagentCLIRunID, "--file", updateFile, "--key", "update-once")
	if code != 0 || errout != "" || !strings.Contains(out, `"revision":21`) {
		t.Fatal(code, out, errout)
	}
	code, out, errout = mutationInvoke(t, profilePath, "builder", "reviews", "subagent-read", subagentCLIRunID)
	if code != 0 || errout != "" || !strings.Contains(out, `"channel":"subagent"`) || mutations.Load() != 3 || reads.Load() != 1 {
		t.Fatal("extra requests or broken read", code, out, errout)
	}
	var stderr bytes.Buffer
	args := append([]string{"--profiles-file", profilePath, "--profile", "builder"}, record...)
	code = cli.RunContext(t.Context(), args, nil, failedMutationOutput{}, &stderr)
	if code != cli.ExitConfig || !strings.Contains(stderr.String(), `"outcome_unknown":true`) || !strings.Contains(stderr.String(), "same key") || strings.Contains(stderr.String(), "PRIVATE_SUBAGENT_TEXT") {
		t.Fatal("lost exact recovery on stdout failure", code, &stderr)
	}
}
