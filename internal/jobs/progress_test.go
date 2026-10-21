package jobs_test

import (
	"net/http/httptest"
	"testing"
)

type progressItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Done bool   `json:"done"`
}
type progressChecklist struct {
	JobID       string         `json:"job_id"`
	JobRevision int64          `json:"job_revision"`
	Revision    int64          `json:"revision"`
	Items       []progressItem `json:"items"`
}

func progressFixture(t *testing.T) (*testAPI, string, string, string, string) {
	t.Helper()
	a := setupAPI(t)
	p := a.createProject("progress", "example/progress")
	token := a.grant(p, "progress-client", "read", "write")
	worker := a.createWorker(p, "builder")
	job := createCheckpointTestJob(t, a, p, token)
	return a, p, token, worker, "/api/v1/projects/" + p + "/jobs/" + job.ID
}
func progressState(t *testing.T, a *testAPI) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"jobs", "job_history", "job_checkpoints", "job_updates", "job_checklists", "job_checklist_history", "events", "receipts"} {
		var n int
		if err := a.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}
func progressReplace(job, revision int64, items []progressItem) map[string]any {
	return map[string]any{"expected_job_revision": job, "expected_revision": revision, "items": items}
}
func progressOK(t *testing.T, r *httptest.ResponseRecorder, status int) {
	t.Helper()
	if r.Code != status {
		t.Fatalf("status%d want%d: %s", r.Code, status, r.Body.String())
	}
}
