package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestCheckpointCommandsPreserveReceiptsTextAndExplicitHistory(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "cli-checkpoints")
	worker := f.worker(t, project, "Builder")
	job := f.call(t, "POST", "/api/v1/projects/"+project+"/jobs", f.admin, worker, map[string]any{"title": "Checkpoint job", "brief": "Original brief"})["id"].(string)
	var corrupt atomic.Bool
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/checkpoints") {
			writes.Add(1)
			if corrupt.CompareAndSwap(true, false) {
				recorder := httptest.NewRecorder()
				f.handler.ServeHTTP(recorder, r)
				if recorder.Code != 201 {
					t.Error("checkpoint did not commit before response damage")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{"id":"incomplete"}`))
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	invoke := func(args ...string) string {
		t.Helper()
		code, out, err := mutationInvoke(t, path, "builder", args...)
		if code != 0 || err != "" {
			t.Fatalf("checkpoint CLI: %d %s %s", code, out, err)
		}
		return out
	}
	var latest client.LatestCheckpointResult
	if json.Unmarshal([]byte(invoke("jobs", "checkpoint-latest", job)), &latest) != nil || latest.Checkpoint != nil || latest.JobRevision != 1 {
		t.Fatal("existing job without checkpoint not distinguished")
	}
	args := []string{"jobs", "checkpoint", job, "--key", "checkpoint-once", "--expect-revision", "1", "--done", "  PRIVATE_DONE\n", "--next", "next\tstep", "--blocker", "", "--evidence", "proof-two", "--evidence=proof-one"}
	var receipt, replay client.CheckpointReceipt
	out := invoke(args...)
	if json.Unmarshal([]byte(out), &receipt) != nil || receipt.Sequence < 1 || receipt.JobRevision != 1 || receipt.Outcome != "created" || strings.Contains(out, "PRIVATE_DONE") || strings.Contains(out, "evidence") {
		t.Fatal("checkpoint receipt not compact")
	}
	if json.Unmarshal([]byte(invoke(args...)), &replay) != nil || replay != receipt {
		t.Fatal("checkpoint exact replay changed")
	}
	if json.Unmarshal([]byte(invoke("jobs", "checkpoint-latest", job)), &latest) != nil || latest.Checkpoint == nil || latest.Checkpoint.Done != "  PRIVATE_DONE\n" || latest.Checkpoint.WorkerID != worker || strings.Join(latest.Checkpoint.Evidence, ",") != "proof-two,proof-one" {
		t.Fatal("checkpoint text/order/attribution changed")
	}
	corrupt.Store(true)
	uncertain := []string{"jobs", "checkpoint", job, "--key", "checkpoint-uncertain", "--expect-revision", "1", "--done", "second", "--next", "third"}
	code, stdout, stderr := mutationInvoke(t, path, "builder", uncertain...)
	if code != cli.ExitService || stdout != "" || !strings.Contains(stderr, `"outcome_unknown":true`) {
		t.Fatalf("uncertain checkpoint outcome lost: %d %s %s", code, stdout, stderr)
	}
	_ = invoke(uncertain...)
	if writes.Load() != 4 {
		t.Fatalf("automatic write retry: %d", writes.Load())
	}
	var first, second client.Page[client.Checkpoint]
	if json.Unmarshal([]byte(invoke("jobs", "checkpoints", job, "--limit", "1")), &first) != nil || len(first.Items) != 1 || !first.HasMore || first.Cursor == "" {
		t.Fatal("archive first page incomplete")
	}
	if json.Unmarshal([]byte(invoke("jobs", "checkpoints", job, "--limit", "1", "--cursor", first.Cursor)), &second) != nil || len(second.Items) != 1 || second.HasMore || first.Items[0].Sequence <= second.Items[0].Sequence || second.Items[0].ID != receipt.ID {
		t.Fatal("archive descending cursor order changed")
	}
	f.call(t, "PATCH", "/api/v1/projects/"+project+"/jobs/"+job, f.admin, worker, map[string]any{"expected_revision": 1, "status": "blocked"})
	if json.Unmarshal([]byte(invoke("jobs", "checkpoint-latest", job)), &latest) != nil || latest.JobRevision != 2 || latest.Checkpoint.JobRevision != 1 {
		t.Fatal("older checkpoint was not returned faithfully")
	}
	code, stdout, stderr = mutationInvoke(t, path, "builder", "jobs", "checkpoint", job, "--key", "stale", "--expect-revision", "1", "--done", "done", "--next", "next")
	if code != cli.ExitConflict || stdout != "" || !strings.Contains(stderr, "revision_conflict") {
		t.Fatal("stale revision did not surface conflict")
	}
	if f.resolverHit.Load() != 1 {
		t.Fatal("checkpoints performed provider work")
	}
}

func TestCheckpointArgumentsRejectBeforeProfileOrCredentialAccess(t *testing.T) {
	base := []string{"jobs", "checkpoint", "job-one", "--key", "explicit", "--expect-revision", "1", "--done", "done", "--next", "next"}
	cases := [][]string{
		{"jobs", "checkpoint", "job-one"}, append(append([]string{}, base...), "--key", "other"), append(append([]string{}, base...), "--done", "other"), append(append([]string{}, base...), "--unknown", "x"),
		{"jobs", "checkpoint", "job-one", "--key", "explicit", "--expect-revision", "0", "--done", "done", "--next", "next"},
		{"jobs", "checkpoint", "job-one", "--key", "explicit", "--expect-revision", "9223372036854775808", "--done", "done", "--next", "next"},
		{"jobs", "checkpoint", "job-one", "--key", "explicit", "--expect-revision", "1", "--done", "bad\x00", "--next", "next"},
		{"jobs", "checkpoint", "job-one", "--key", "explicit", "--expect-revision", "1", "--done", strings.Repeat("x", 8192), "--next", strings.Repeat("x", 8192), "--blocker", "x"},
		{"jobs", "checkpoint-latest", "job-one", "--limit", "1"}, {"jobs", "checkpoints", "job-one", "--limit", "101"}, {"jobs", "checkpoints", "job-one", "--cursor", "a", "--cursor", "b"}, {"jobs", "checkpoint-latest", "../other"},
	}
	tooMany := append([]string{}, base...)
	for i := 0; i < 11; i++ {
		tooMany = append(tooMany, "--evidence", strconv.Itoa(i))
	}
	cases = append(cases, tooMany)
	for i, args := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			code, out, err := run(append([]string{"--profile", "absent", "--profiles-file", "/unavailable/profiles.json"}, args...), nil)
			if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_arguments") {
				t.Fatalf("validation precedence: %d %s %s", code, out, err)
			}
		})
	}
}
