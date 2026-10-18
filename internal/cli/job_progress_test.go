package cli_test

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestJobProgressCommandsUseExplicitIndependentState(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "cli-progress")
	worker := f.worker(t, project, "Builder")
	job := f.call(t, "POST", "/api/v1/projects/"+project+"/jobs", f.admin, worker, map[string]any{"title": "Progress job", "brief": "Standing brief"})["id"].(string)
	service := httptest.NewServer(f.handler)
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	invoke := func(args ...string) string {
		t.Helper()
		code, out, err := mutationInvoke(t, path, "builder", args...)
		if code != cli.ExitSuccess || err != "" {
			t.Fatalf("progress CLI: %d %s %s", code, out, err)
		}
		return out
	}
	var checklist client.Checklist
	if json.Unmarshal([]byte(invoke("jobs", "checklist", job)), &checklist) != nil || checklist.Revision != 0 || checklist.Items == nil || checklist.JobRevision != 1 {
		t.Fatal("empty checklist not explicit")
	}
	update := []string{"jobs", "update", job, "--key", "update-once", "--expect-revision", "1", "--body", "  PRIVATE_PROGRESS\n", "--evidence", "proof:one", "--evidence=proof:two"}
	var receipt, replay client.UpdateReceipt
	out := invoke(update...)
	if strings.Contains(out, "PRIVATE_PROGRESS") || json.Unmarshal([]byte(out), &receipt) != nil || receipt.JobRevision != 1 || receipt.Sequence < 1 {
		t.Fatal("update receipt is invalid or echoes submitted body")
	}
	if json.Unmarshal([]byte(invoke(update...)), &replay) != nil || replay != receipt {
		t.Fatal("exact update replay changed")
	}
	var updates client.Page[client.Update]
	if json.Unmarshal([]byte(invoke("jobs", "updates", job, "--limit", "1")), &updates) != nil || len(updates.Items) != 1 || updates.HasMore || updates.Items[0].Body != "  PRIVATE_PROGRESS\n" || updates.Items[0].WorkerID != worker || strings.Join(updates.Items[0].Evidence, ",") != "proof:one,proof:two" {
		t.Fatal("explicit update history lost text, order or attribution")
	}
	items := `[{"id":"prove","text":"Run proof","done":false}]`
	input := filepath.Join(t.TempDir(), "items.json")
	if err := os.WriteFile(input, []byte(items), 0600); err != nil {
		t.Fatal(err)
	}
	set := []string{"jobs", "checklist-set", job, "--key", "plan-once", "--expect-revision", "1", "--expect-checklist-revision", "0", "--items-file", input}
	var setReceipt, setReplay client.ChecklistReceipt
	if json.Unmarshal([]byte(invoke(set...)), &setReceipt) != nil || setReceipt.Revision != 1 || setReceipt.Outcome != "updated" {
		t.Fatal("initial checklist replacement failed")
	}
	if json.Unmarshal([]byte(invoke(set...)), &setReplay) != nil || setReplay != setReceipt {
		t.Fatal("checklist retry added a revision")
	}
	if json.Unmarshal([]byte(invoke("jobs", "checklist", job)), &checklist) != nil || checklist.Revision != 1 || len(checklist.Items) != 1 || checklist.Items[0].Done || checklist.Items[0].ID != "prove" {
		t.Fatal("current checklist changed item identity/completion")
	}
	f.call(t, "PATCH", "/api/v1/projects/"+project+"/jobs/"+job, f.admin, worker, map[string]any{"expected_revision": 1, "status": "blocked"})
	code, out, err := mutationInvoke(t, path, "builder", "jobs", "checklist-set", job, "--key", "stale", "--expect-revision", "1", "--expect-checklist-revision", "1", "--items", items)
	if code != cli.ExitConflict || out != "" || !strings.Contains(err, "revision_conflict") {
		t.Fatal("stale brief guard did not surface")
	}
	completed := strings.Replace(items, "false", "true", 1)
	invoke("jobs", "checklist-set", job, "--key", "plan-done", "--expect-revision", "2", "--expect-checklist-revision", "1", "--items", completed)
	var history client.Page[client.ChecklistHistoryEntry]
	if json.Unmarshal([]byte(invoke("jobs", "checklist-history", job, "--limit", "1")), &history) != nil || len(history.Items) != 1 || !history.HasMore || history.Items[0].Revision != 2 || !history.Items[0].Items[0].Done || history.Items[0].JobRevision != 2 {
		t.Fatal("explicit checklist archive lost completion or captured brief revision")
	}
	if json.Unmarshal([]byte(invoke("jobs", "checklist-history", job, "--limit", "1", "--cursor", history.Cursor)), &history) != nil || len(history.Items) != 1 || history.HasMore || history.Items[0].Revision != 1 || history.Items[0].Items[0].Done {
		t.Fatal("checklist continuation lost old state")
	}
	var current client.Job
	if json.Unmarshal([]byte(invoke("jobs", "read", job)), &current) != nil || current.Brief != "Standing brief" || current.Revision != 2 || f.resolverHit.Load() != 1 {
		t.Fatal("progress changed standing brief/revision or performed provider work")
	}
	invoke("jobs", "update", job, "--key", "markdown-update", "--expect-revision", "2", "--body", "-- Heading\nText")
	invoke("jobs", "checkpoint", job, "--key", "markdown-checkpoint", "--expect-revision", "2", "--done", "-- Done", "--next", "-- Next")
	var checkpoint client.LatestCheckpointResult
	if json.Unmarshal([]byte(invoke("jobs", "checkpoint-latest", job)), &checkpoint) != nil || checkpoint.Checkpoint == nil || checkpoint.Checkpoint.Done != "-- Done" || checkpoint.Checkpoint.Next != "-- Next" {
		t.Fatal("dash-prefixed checkpoint text was not preserved")
	}
	if json.Unmarshal([]byte(invoke("jobs", "updates", job, "--limit", "1")), &updates) != nil || len(updates.Items) != 1 || updates.Items[0].Body != "-- Heading\nText" {
		t.Fatal("dash-prefixed narrative was not preserved")
	}
}

func TestJobProgressRejectsInvalidRequestsBeforeProfiles(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(broken, []byte(`[{"id":"a","text":"Missing completion"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	base := []string{"jobs", "checklist-set", "job-one", "--key", "explicit", "--expect-revision", "1", "--expect-checklist-revision", "0"}
	cases := [][]string{
		{"jobs", "update", "job-one", "--key", "k", "--expect-revision", "1", "--body", " "},
		{"jobs", "update", "job-one", "--key", "k", "--expect-revision", "0", "--body", "body"},
		{"jobs", "update", "job-one", "--key", "k", "--expect-revision", "1", "--body", "body", "--body", "duplicate"},
		{"jobs", "update", "job-one", "--key", "k", "--expect-revision", "1", "--body", "body", "--evidence", ""},
		{"jobs", "checklist", "job-one", "--limit", "1"},
		{"jobs", "updates", "job-one", "--limit", "101"},
		append(append([]string{}, base...), "--items-file", broken),
		append(append([]string{}, base...), "--items-file", filepath.Dir(broken)),
		append(append([]string{}, base...), "--items", "null"),
		append(append([]string{}, base...), "--items", "[]", "--items-file", broken),
		append(append([]string{}, base...), "--items", `[{"id":"a","text":"x","done":null}]`),
		append(append([]string{}, base...), "--items", `[{"id":"a","text":"x","done":false,"done":true}]`),
		append(append([]string{}, base...), "--items", `[{"id":"a","text":"x","done":false},{"id":"a","text":"y","done":true}]`),
	}
	for _, args := range cases {
		code, out, err := run(append([]string{"--profile", "absent", "--profiles-file", "/unavailable/profiles.json"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_arguments") {
			t.Fatalf("validation precedence for %q: %d %s %s", args[:2], code, out, err)
		}
	}
}
