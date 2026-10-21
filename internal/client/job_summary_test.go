package client_test

import (
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestJobSummaryClientIndependentStateAndLostCommittedReplay(t *testing.T) {
	f := newMutationFixture(t)
	var corrupt atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/summary") && corrupt.CompareAndSwap(true, false) {
			recorded := httptest.NewRecorder()
			f.handler.ServeHTTP(recorded, r)
			if recorded.Code != 200 {
				t.Errorf("summary failed before response loss: %s", recorded.Body.String())
			}
			w.WriteHeader(200)
			_, _ = io.WriteString(w, "{")
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	c := f.newClient(t, f.admin, f.sender, server.URL)
	job, err := c.CreateJob(t.Context(), "summary-job", client.CreateJobRequest{Title: "Private title", Brief: "Private standing instructions"})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := c.JobSummary(t.Context(), job.ID)
	if err != nil || empty.Revision != 0 || empty.Title != "" || empty.Body != "" {
		t.Fatal(empty, err)
	}
	request := client.SetJobSummaryRequest{ExpectedJobRevision: 1, Title: "Authored title", Body: "**Authored** Markdown 雪"}
	corrupt.Store(true)
	_, err = c.SetJobSummary(t.Context(), "summary-once", job.ID, request)
	var unknown *client.Error
	if !errors.As(err, &unknown) || !unknown.OutcomeUnknown {
		t.Fatal("committed lost response not uncertain", err)
	}
	first, err := c.SetJobSummary(t.Context(), "summary-once", job.ID, request)
	if err != nil || first.Revision != 1 || first.Outcome != "updated" {
		t.Fatal(first, err)
	}
	retained, err := c.JobSummaryRevision(t.Context(), job.ID, 1)
	if err != nil || retained.Title != request.Title || retained.Body != request.Body || retained.JobRevision != 1 || retained.WorkerID != f.sender {
		t.Fatal(retained, err)
	}
	private := "Private brief changed"
	_, err = c.UpdateJob(t.Context(), "private-update", job.ID, client.UpdateJobRequest{ExpectedRevision: 1, Brief: &private})
	if err != nil {
		t.Fatal(err)
	}
	state, err := c.JobSummary(t.Context(), job.ID)
	if err != nil || state.JobRevision != 2 || state.Revision != 1 || state.Title != request.Title || state.Body != request.Body {
		t.Fatal(state, err)
	}
	clear, err := c.SetJobSummary(t.Context(), "summary-clear", job.ID, client.SetJobSummaryRequest{ExpectedJobRevision: 2, ExpectedRevision: 1})
	if err != nil || clear.Revision != 2 {
		t.Fatal(clear, err)
	}
	replay, err := c.SetJobSummary(t.Context(), "summary-once", job.ID, request)
	if err != nil || replay != first {
		t.Fatal("original replay after private edit and clear", replay, err)
	}
	page, err := c.JobSummaryHistory(t.Context(), job.ID, client.PageOptions{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].Revision != 2 || !page.HasMore {
		t.Fatal(page, err)
	}
	page, err = c.JobSummaryHistory(t.Context(), job.ID, client.PageOptions{Limit: 1, Cursor: page.Cursor})
	if err != nil || len(page.Items) != 1 || page.Items[0] != retained || page.HasMore {
		t.Fatal(page, err)
	}
}

func TestJobSummaryClientRejectsBeforeIOAndRefusesForgedResponses(t *testing.T) {
	c, calls := progressResponseClient(t, `{}`)
	for _, r := range []client.SetJobSummaryRequest{
		{ExpectedJobRevision: 0, Title: "Title", Body: "Body"}, {ExpectedJobRevision: 1, ExpectedRevision: -1, Title: "Title", Body: "Body"}, {ExpectedJobRevision: 1, ExpectedRevision: math.MaxInt64, Title: "Title", Body: "Body"}, {ExpectedJobRevision: 1, Title: "", Body: "Body"}, {ExpectedJobRevision: 1, Title: "Title", Body: strings.Repeat("雪", 667)}, {ExpectedJobRevision: 1, Title: strings.Repeat("雪", 81), Body: "Body"}, {ExpectedJobRevision: 1, Title: "Title\nBad", Body: "Body"},
	} {
		if _, err := c.SetJobSummary(t.Context(), "key", "job-one", r); err == nil {
			t.Fatal("accepted invalid summary", r)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid summary contacted service")
	}
	valid := client.JobShareableSummary{JobID: "job-one", JobRevision: 2, Revision: 1, Title: "Title", Body: "Body"}
	for _, change := range []func(*client.JobShareableSummary){func(v *client.JobShareableSummary) { v.JobID = "other" }, func(v *client.JobShareableSummary) { v.JobRevision = 0 }, func(v *client.JobShareableSummary) { v.Revision = 0 }, func(v *client.JobShareableSummary) { v.Title = "" }, func(v *client.JobShareableSummary) { v.Body = strings.Repeat("x", 2001) }} {
		bad := valid
		change(&bad)
		remote, _ := progressResponseClient(t, progressJSON(t, bad))
		if _, err := remote.JobSummary(t.Context(), "job-one"); err == nil {
			t.Fatal("accepted forged summary", bad)
		}
	}
	remote, _ := progressResponseClient(t, `{"id":"job-one","job_revision":1,"revision":9,"outcome":"updated"}`)
	_, err := remote.SetJobSummary(t.Context(), "key", "job-one", client.SetJobSummaryRequest{ExpectedJobRevision: 1, Title: "Title", Body: "Body"})
	var unknown *client.Error
	if !errors.As(err, &unknown) || !unknown.OutcomeUnknown {
		t.Fatal("bad receipt not uncertain", err)
	}
	remote, _ = progressResponseClient(t, `{"items":[{"job_id":"other","job_revision":1,"revision":1,"title":"Title","body":"Body","actor_id":"actor-one","worker_id":"","created_at":"2026-10-20T00:00:00Z"}],"has_more":false}`)
	if _, err := remote.JobSummaryHistory(t.Context(), "job-one", client.PageOptions{}); err == nil {
		t.Fatal("accepted foreign history")
	}
}
