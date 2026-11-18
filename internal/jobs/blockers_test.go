package jobs_test

import (
	"net/http"
	"testing"
)

func TestJobBlockersBlockAndReleaseOnCompletion(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("deps", "example/deps")
	base := "/api/v1/projects/" + project + "/jobs"
	create := func(title string) string {
		r := a.call(http.MethodPost, base, a.admin, "create-"+title, map[string]any{"title": title})
		if r.Code != http.StatusOK && r.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, r.Code, r.Body.String())
		}
		var out receipt
		decodeResponse(t, r, &out)
		return out.ID
	}
	read := func(id string) (jobValue, []string) {
		r := a.call(http.MethodGet, base+"/"+id, a.admin, "", nil)
		var out struct {
			jobValue
			BlockedBy []string `json:"blocked_by"`
		}
		decodeResponse(t, r, &out)
		return out.jobValue, out.BlockedBy
	}
	edit := func(key, id string, body map[string]any) *testResponse {
		current, _ := read(id)
		body["expected_revision"] = current.Revision
		r := a.call(http.MethodPatch, base+"/"+id, a.admin, key, body)
		return &testResponse{r.Code, r.Body.String()}
	}
	blocker, other, dependent := create("blocker"), create("other"), create("dependent")

	if r := edit("block", dependent, map[string]any{"blocked_by": []string{blocker, other}}); r.code != http.StatusOK {
		t.Fatalf("set blockers: %d %s", r.code, r.body)
	}
	if got, ids := read(dependent); got.Status != "blocked" || len(ids) != 2 {
		t.Fatalf("dependent not blocked: %+v %v", got, ids)
	}
	for _, c := range []struct {
		key string
		id  string
		ids []string
	}{{"self", dependent, []string{dependent}}, {"cycle", blocker, []string{dependent}}, {"missing", dependent, []string{"job_missing"}}, {"dup", dependent, []string{other, other}}} {
		if r := edit(c.key, c.id, map[string]any{"blocked_by": c.ids}); r.code != http.StatusBadRequest {
			t.Fatalf("%s accepted: %d %s", c.key, r.code, r.body)
		}
	}
	// Completing one blocker leaves the job blocked; completing the last releases it.
	if r := edit("done-1", blocker, map[string]any{"status": "completed"}); r.code != http.StatusOK {
		t.Fatalf("complete blocker: %d %s", r.code, r.body)
	}
	if got, _ := read(dependent); got.Status != "blocked" {
		t.Fatalf("released with an open blocker: %+v", got)
	}
	if r := edit("done-2", other, map[string]any{"status": "completed"}); r.code != http.StatusOK {
		t.Fatalf("complete other: %d %s", r.code, r.body)
	}
	if got, _ := read(dependent); got.Status != "active" {
		t.Fatalf("not released after all blockers completed: %+v", got)
	}
	// Clearing the list is an ordinary edit.
	if r := edit("clear", dependent, map[string]any{"blocked_by": []string{}}); r.code != http.StatusOK {
		t.Fatalf("clear: %d %s", r.code, r.body)
	}
	if _, ids := read(dependent); len(ids) != 0 {
		t.Fatalf("blockers not cleared: %v", ids)
	}
}

type testResponse struct {
	code int
	body string
}
