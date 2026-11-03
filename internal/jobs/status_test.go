package jobs_test

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

func TestJobStatusBoundedAuthorizedReadAndConservation(t *testing.T) {
	a, project, token, worker, base := progressFixture(t)
	initial := a.call("GET", base+"/status", token, "", nil)
	progressOK(t, initial, 200)
	var empty client.JobStatus
	decodeResponse(t, initial, &empty)
	if empty.Revision != 1 || empty.Status != "active" || empty.WorkerID != "" || empty.ProgressSummary != "" || empty.Next != "" || empty.Blocker != "" || empty.EvidenceCount != 0 || empty.ProgressTruncated || empty.NextTruncated || empty.BlockerTruncated {
		t.Fatal("initial status omitted or invented current progress", empty)
	}
	refs := make([]string, 10)
	for i := range refs {
		refs[i] = fmt.Sprintf("private-evidence-%d-", i) + strings.Repeat("e", 480)
	}
	progressOK(t, a.call("PATCH", base, token, "status-large", map[string]any{"expected_revision": 1, "title": strings.Repeat("<", 240), "brief": strings.Repeat("PRIVATE-BRIEF", 2500), "worker_id": worker, "status": "blocked", "progress": map[string]any{"summary": strings.Repeat("<", 5000), "next": strings.Repeat("<", 3000), "blocker": strings.Repeat("<", 1000), "evidence": refs}}), 200)
	readOnly := a.grant(project, "status-reader", "read")
	other := a.createProject("status-other", "example/status-other")
	foreign := a.grant(other, "status-foreign", "read")
	before := contextState(t, a)
	response := a.call("GET", base+"/status", token, "", nil)
	progressOK(t, response, 200)
	var status client.JobStatus
	decodeResponse(t, response, &status)
	if status.ID != base[strings.LastIndex(base, "/")+1:] || status.Title != strings.Repeat("<", 240) || status.WorkerID != worker || status.Status != "blocked" || status.Revision != 2 || status.UpdatedAt == "" || status.ProgressSummary != strings.Repeat("<", 1024) || status.Next != strings.Repeat("<", 512) || status.Blocker != strings.Repeat("<", 512) || !status.ProgressTruncated || !status.NextTruncated || !status.BlockerTruncated || status.EvidenceCount != 10 {
		t.Fatal("inaccurate compact status", status)
	}
	if response.Body.Len() >= 16<<10 {
		t.Fatal("escaped response exceeds compact budget", response.Body.Len())
	}
	for _, hidden := range []string{"PRIVATE-BRIEF", "private-evidence", `"brief"`, `"evidence"`, `"notes"`, `"checklist"`, `"history"`, `"actor_id"`} {
		if strings.Contains(response.Body.String(), hidden) {
			t.Fatal("status included unselected context", hidden)
		}
	}
	progressOK(t, a.call("GET", base+"/status", readOnly, "", nil), 200)
	progressOK(t, a.call("GET", base+"/status", foreign, "", nil), 403)
	progressOK(t, a.call("GET", base+"/status", "", "", nil), 401)
	progressOK(t, a.call("GET", base+"/status?limit=1", token, "", nil), 400)
	progressOK(t, a.call("GET", "/api/v1/projects/"+project+"/jobs/missing/status", token, "", nil), 404)
	// Grants/project setup above deliberately adds no job-domain state.
	if !reflect.DeepEqual(before, contextState(t, a)) {
		t.Fatal("status reads modified job state")
	}
	full := readWorking(t, a, base, token)
	if len(full.Brief) != 32500 || len(full.Progress.Summary) != 5000 || !reflect.DeepEqual(full.Progress.Evidence, refs) {
		t.Fatal("compact read changed full job")
	}
	progressOK(t, a.call("PATCH", base, token, "status-unicode", map[string]any{"expected_revision": 2, "progress": map[string]any{"summary": strings.Repeat("a", 1023) + "雪", "next": strings.Repeat("雪", 171), "blocker": ""}}), 200)
	response = a.call("GET", base+"/status", token, "", nil)
	progressOK(t, response, 200)
	decodeResponse(t, response, &status)
	if len(status.ProgressSummary) != 1023 || len(status.Next) != 510 || !utf8.ValidString(status.ProgressSummary+status.Next) || !status.ProgressTruncated || !status.NextTruncated || status.BlockerTruncated {
		t.Fatal("split UTF-8 prefix or inaccurate truncation", status)
	}
	// A later brief-only revision legitimately retains the previous progress.
	progressOK(t, a.call("PATCH", base, token, "status-private", map[string]any{"expected_revision": 3, "brief": "Changed private definition"}), 200)
	response = a.call("GET", base+"/status", token, "", nil)
	progressOK(t, response, 200)
	decodeResponse(t, response, &status)
	if status.Revision != 4 || len(status.ProgressSummary) != 1023 {
		t.Fatal("metadata edit invalidated current progress")
	}
}

func TestJobStatusCoherentDuringAtomicUpdates(t *testing.T) {
	a, _, token, _, base := progressFixture(t)
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Go(func() {
		defer close(done)
		for revision := int64(1); revision <= 20; revision++ {
			marker := fmt.Sprintf("state-%d", revision+1)
			r := a.call("PATCH", base, token, marker, map[string]any{"expected_revision": revision, "title": marker, "progress": map[string]any{"summary": marker, "next": marker, "blocker": marker}})
			if r.Code != 200 {
				t.Errorf("atomic update failed: %d", r.Code)
				return
			}
		}
	})
	reads := 0
	for {
		r := a.call("GET", base+"/status", token, "", nil)
		progressOK(t, r, 200)
		var out client.JobStatus
		if json.Unmarshal(r.Body.Bytes(), &out) != nil {
			t.Fatal("invalid status JSON")
		}
		if out.Revision > 1 {
			marker := fmt.Sprintf("state-%d", out.Revision)
			if out.Title != marker || out.ProgressSummary != marker || out.Next != marker || out.Blocker != marker {
				t.Fatal("mixed job/progress snapshot", out)
			}
		}
		reads++
		select {
		case <-done:
			wg.Wait()
			if reads == 0 {
				t.Fatal("no reads")
			}
			return
		default:
		}
	}
}

func TestJobStatusRefusesCorruptCurrentProgress(t *testing.T) {
	for _, query := range []string{`DELETE FROM job_progress`, `UPDATE job_progress SET job_revision=99`, `UPDATE job_progress SET evidence_json='null'`, `UPDATE job_progress SET summary=zeroblob(1000000)`} {
		t.Run(query, func(t *testing.T) {
			a, _, token, _, base := progressFixture(t)
			if _, err := a.store.DB().Exec(query); err != nil {
				t.Fatal(err)
			}
			r := a.call("GET", base+"/status", token, "", nil)
			progressOK(t, r, 500)
			if !strings.Contains(r.Body.String(), "storage_unavailable") {
				t.Fatal("unsafe corruption response")
			}
		})
	}
}

func TestJobStatusWorstEscapedEnvelope(t *testing.T) {
	// '<' has the largest JSON expansion per permitted ASCII byte. Give both
	// identifiers their maximum length as well as every text preview and title.
	out := client.JobStatus{ID: strings.Repeat("<", 128), WorkerID: strings.Repeat("<", 128), Title: strings.Repeat("<", 240), Status: "completed", Revision: math.MaxInt64, UpdatedAt: strings.Repeat("0", 64), ProgressSummary: strings.Repeat("<", 1024), Next: strings.Repeat("<", 512), Blocker: strings.Repeat("<", 512), EvidenceCount: 10}
	body, err := json.Marshal(out)
	if err != nil || len(body)+1 >= 16<<10 {
		t.Fatal("maximum escaped envelope exceeds budget", len(body)+1, err)
	}
	t.Logf("maximum escaped envelope: %d bytes including newline", len(body)+1)
}
