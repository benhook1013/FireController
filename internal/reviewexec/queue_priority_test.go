package reviewexec

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/benhook1013/FireController/internal/reviews"
)

func TestQueuePriorityHTTPAndFrontUseSameTopology(t *testing.T) {
	f := queueEnrollmentFixture(t)
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, attemptProject)
	f.e.queueSortingWrites = true
	c := enrollmentConfiguration(0, []int64{6, 4, 2}, []string{"hosted", "cli"})
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "priority-saved-order", c); err != nil {
		t.Fatal(err)
	}
	publishQueueTopology(t, f)
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	path := "/api/v1/projects/" + attemptProject + "/reviews/"
	readQueue := func(cursor string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest("GET", path+"queue?view=recent&limit=2&cursor="+cursor, nil)
		request.Header.Set("Authorization", "Bearer "+f.reader)
		request.Header.Set("X-FC-Queue-Topology", "v1")
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, request)
		return out
	}
	check := func(want []int64, direct []int64) reviews.QueueResponse {
		t.Helper()
		if got := frontRead(t, f); !slices.Equal(got.Queue, want) {
			t.Fatalf("scheduler order %v, want %v", got.Queue, want)
		}
		response := readQueue("")
		var page reviews.QueueResponse
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Items) != 2 || !page.HasMore {
			t.Fatal(response.Code, response.Body.String())
		}
		for i, pull := range page.Items {
			if pull.Number != want[i] || pull.Priority != slices.Contains(direct, pull.Number) {
				t.Fatal("browser order or flag disagreed", pull.Number, pull.Priority)
			}
		}
		return page
	}
	original := check([]int64{6, 2, 4}, nil)
	body := `{"expected_revision":0,"pull_numbers":[4],"priority":true}`
	post := func(token, key, body string, code int) string {
		t.Helper()
		got := f.request("POST", path+"queue-priority", token, key, body)
		if got.Code != code {
			t.Fatal(got.Code, got.Body.String())
		}
		return got.Body.String()
	}
	post(f.reader, "reader-priority", body, 403)
	post(f.other, "other-priority", body, 403)
	post(f.writer, "unknown-pr", `{"expected_revision":0,"pull_numbers":[7],"priority":true}`, 409)
	first := post(f.writer, "flag-child", body, 200)
	if post(f.writer, "flag-child", body, 200) != first {
		t.Fatal("original key returned different receipt")
	}
	post(f.writer, "stale-priority", body, 409)
	check([]int64{2, 4, 6}, []int64{4})
	if stale := readQueue(original.Cursor); stale.Code != 400 {
		t.Fatal("stale pagination cursor was accepted", stale.Code, stale.Body.String())
	}
	post(f.writer, "flag-both", `{"expected_revision":1,"pull_numbers":[2,4],"priority":true}`, 200)
	check([]int64{2, 4, 6}, []int64{2, 4})
	post(f.writer, "clear-child", `{"expected_revision":2,"pull_numbers":[4],"priority":false}`, 200)
	check([]int64{2, 4, 6}, []int64{2})
	got, err := f.e.ReadExecutionConfiguration(t.Context(), f.scope(t, attemptProject, f.writer, false))
	if err != nil || got.Revision != 1 || !slices.Equal(got.Queue, []int64{6, 4, 2}) {
		t.Fatal("priority altered saved execution configuration", got, err)
	}
	c.ExpectedRevision, c.QueueSortMode = new(int64(1)), "manual"
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "priority-manual-order", c); err != nil {
		t.Fatal(err)
	}
	check([]int64{6, 4, 2}, []int64{2})
	c.ExpectedRevision, c.QueueSortMode = new(int64(2)), "auto"
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "priority-return-auto", c); err != nil {
		t.Fatal(err)
	}
	post(f.writer, "clear-root", `{"expected_revision":3,"pull_numbers":[2],"priority":false}`, 200)
	check([]int64{6, 2, 4}, nil)
}
