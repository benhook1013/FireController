package reviewexec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
)

func TestQueueSortingHTTPPreservesConfigurationAndRetries(t *testing.T) {
	f := newAttemptFixture(t)
	f.e.queueSortingWrites = true
	f.configure(t, attemptProject, "test-capability", 0, false)
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	path := "/api/v1/projects/" + attemptProject + "/reviews/queue-sorting"
	get := f.request("GET", path, f.reader, "", "")
	var view queueSortingView
	if get.Code != 200 || json.Unmarshal(get.Body.Bytes(), &view) != nil || view.Mode != "auto" || view.Revision != 1 {
		t.Fatal(get.Code, get.Body.String())
	}
	body := `{"expected_revision":1,"mode":"manual"}`
	for _, token := range []string{f.reader, f.other} {
		if denied := f.request("POST", path, token, "denied-sort", body); denied.Code != 403 {
			t.Fatal(denied.Code, denied.Body.String())
		}
	}
	first := f.request("POST", path, f.writer, "sort-manual", body)
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body.String())
	}
	retry := f.request("POST", path, f.writer, "sort-manual", body)
	if retry.Code != 200 || retry.Body.String() != first.Body.String() {
		t.Fatal("retry", retry.Code, retry.Body.String())
	}
	c, err := f.e.ReadExecutionConfiguration(t.Context(), f.scope(t, attemptProject, f.writer, false))
	if err != nil || c.QueueSortMode != "manual" || !slices.Equal(c.Queue, []int64{2, 4}) || c.Mode != "independent" || c.Paused {
		t.Fatal(c, err)
	}
	stale := f.request("POST", path, f.admin, "stale-sort", `{"expected_revision":1,"mode":"auto"}`)
	if stale.Code != 409 {
		t.Fatal("stale", stale.Code, stale.Body.String())
	}
	if w := f.request("POST", path, f.writer, "sort-auto", `{"expected_revision":2,"mode":"auto"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	c, err = f.e.ReadExecutionConfiguration(t.Context(), f.scope(t, attemptProject, f.writer, false))
	if err != nil || c.Revision != 3 || c.QueueSortMode != "auto" || c.CapabilityID != "test-capability" || !slices.Equal(c.Queue, []int64{2, 4}) {
		t.Fatal(c, err)
	}
	for i, mode := range []string{`null`, `"bogus"`, `true`} {
		w := f.request("POST", path, f.writer, fmt.Sprintf("invalid-sort-%d", i), `{"expected_revision":3,"mode":`+mode+`}`)
		if w.Code != 400 {
			t.Fatal("expected invalid mode rejection", mode, w.Code, w.Body.String())
		}
	}
}

func TestQueueSortingHTTPReportsCompatibilityGateWithoutWriting(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	path := "/api/v1/projects/" + attemptProject + "/reviews/queue-sorting"
	w := f.request("GET", path, f.reader, "", "")
	var got queueSortingView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Mode != "auto" || got.Writable || got.Reason != "compatible_backup_and_rollback_readers_required" {
		t.Fatal(w.Code, w.Body.String())
	}
	rejected := f.request("POST", path, f.admin, "disabled-sorting-http", `{"expected_revision":1,"mode":"auto"}`)
	if rejected.Code != 409 {
		t.Fatal(rejected.Code, rejected.Body.String())
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM review_execution_config WHERE project_id=?`, attemptProject).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestQueueSortingHTTPUnconfiguredIsReadOnlyState(t *testing.T) {
	f := newAttemptFixture(t)
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	path := "/api/v1/projects/" + attemptProject + "/reviews/queue-sorting"
	response := f.request("GET", path, f.reader, "", "")
	var got queueSortingView
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &got) != nil || got.Revision != 0 || got.Writable || got.Reason != "reviews_unconfigured" {
		t.Fatal(response.Code, response.Body.String())
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM review_execution_config WHERE project_id=?`, attemptProject).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
