package jobs_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestJobRevisionReadsOnlySelectedHistoricalState(t *testing.T) {
	a, project, token, _, base := progressFixture(t)
	brief := strings.Repeat("Historical **π** context.\n", 1000)
	for i, body := range []map[string]any{
		{"expected_revision": 1, "brief": brief},
		{"expected_revision": 2, "brief": "Current instructions differ."},
	} {
		if r := a.call(http.MethodPatch, base, token, string(rune('a'+i)), body); r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	jobID := strings.TrimPrefix(base, "/api/v1/projects/"+project+"/jobs/")
	// Damage a different history entry. Exact recovery must not walk it, substitute
	// current state, or return the surrounding briefs.
	if _, err := a.store.DB().Exec(`UPDATE job_history SET state_json='invalid' WHERE project_id=? AND job_id=? AND revision=3`, project, jobID); err != nil {
		t.Fatal(err)
	}
	before := progressState(t, a)
	r := a.call(http.MethodGet, base+"/history/2", token, "", nil)
	var entry struct {
		Revision int64 `json:"revision"`
		State    struct {
			ID       string `json:"id"`
			Brief    string `json:"brief"`
			Revision int64  `json:"revision"`
		} `json:"state"`
	}
	decodeResponse(t, r, &entry)
	if r.Code != 200 || entry.Revision != 2 || entry.State.Revision != 2 || entry.State.ID != jobID || entry.State.Brief != brief || strings.Contains(r.Body.String(), "Current instructions differ.") {
		t.Fatal("exact full brief was not recovered", r.Code)
	}
	for path, status := range map[string]int{
		"/history/0": 400, "/history/-1": 400, "/history/01": 400,
		"/history/+1": 400, "/history/1.5": 400, "/history/9223372036854775808": 400,
		"/history/999": 404, "/history/2?limit=1": 400, "/history/2?cursor=other": 400,
		"/history/3": 500,
	} {
		if got := a.call(http.MethodGet, base+path, token, "", nil); got.Code != status {
			t.Fatalf("%s: got %d, want %d", path, got.Code, status)
		}
	}
	if got := a.call(http.MethodGet, base+"/history/2", "", "", nil); got.Code != 401 {
		t.Fatal("anonymous history exposed", got.Code)
	}
	other := a.createProject("other-revision-project", "example/other-revision-project")
	otherToken := a.grant(other, "other-reader", "read")
	// Both the principal and SQL lookup remain scoped to the requested project.
	if got := a.call(http.MethodGet, "/api/v1/projects/"+other+"/jobs/"+jobID+"/history/2", otherToken, "", nil); got.Code != 404 {
		t.Fatal("foreign historical state exposed", got.Code)
	}
	// Compare before creating the separate scope by excluding its setup writes.
	beforeForeign := progressState(t, a)
	_ = a.call(http.MethodGet, base+"/history/2", token, "", nil)
	if !reflect.DeepEqual(beforeForeign, progressState(t, a)) || before["job_history"] != beforeForeign["job_history"] {
		t.Fatal("exact read changed job history")
	}
}

func TestJobRevisionPreservesLegacyShapeAndRefusesDamagedSelectedState(t *testing.T) {
	for _, mode := range []string{"legacy", "wrong-job", "wrong-revision", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			a, project, token, _, base := progressFixture(t)
			id := strings.TrimPrefix(base, "/api/v1/projects/"+project+"/jobs/")
			var raw string
			if err := a.store.DB().QueryRow(`SELECT state_json FROM job_history WHERE project_id=? AND job_id=? AND revision=1`, project, id).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal([]byte(raw), &fields) != nil {
				t.Fatal("fixture state")
			}
			switch mode {
			case "legacy":
				delete(fields, "progress")
			case "wrong-job":
				fields["id"] = json.RawMessage(`"another-job"`)
			case "wrong-revision":
				fields["revision"] = json.RawMessage(`2`)
			}
			body, _ := json.Marshal(fields)
			if mode == "oversized" {
				body = append([]byte(strings.Repeat(" ", (1<<20)+1)), body...)
			}
			if _, err := a.store.DB().Exec(`UPDATE job_history SET state_json=? WHERE project_id=? AND job_id=? AND revision=1`, string(body), project, id); err != nil {
				t.Fatal(err)
			}
			r := a.call(http.MethodGet, base+"/history/1", token, "", nil)
			if mode == "legacy" {
				var entry struct {
					State map[string]json.RawMessage `json:"state"`
				}
				decodeResponse(t, r, &entry)
				if r.Code != 200 || entry.State["progress"] != nil {
					t.Fatal("legacy shape replaced", r.Code)
				}
			} else if r.Code != 500 {
				t.Fatal("damaged selected state accepted", r.Code)
			}
		})
	}
}
