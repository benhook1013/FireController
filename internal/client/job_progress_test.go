package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func progressUpdate() client.Update {
	return client.Update{ID: "update-one", ProjectID: "project-one", JobID: "job-one", JobRevision: 2,
		Sequence: 8, ActorID: "principal-one", WorkerID: "", Body: "  completed <literal>\n", Evidence: []string{"proof-one"}, CreatedAt: "2026-10-15T00:00:00Z"}
}

func progressChecklist() client.Checklist {
	return client.Checklist{JobID: "job-one", JobRevision: 2, Revision: 3, Items: []client.ChecklistItem{{ID: "plan_1", Text: "  preserve text\t", Done: false}}}
}

func progressJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func progressResponseClient(t *testing.T, body string) (*client.Client, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return newClient(t, s), calls
}

func TestJobProgressClientPublicAPIReplayHistoryAndLostCommittedResponse(t *testing.T) {
	f := newMutationFixture(t)
	var lost atomic.Bool
	lost.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/updates") && lost.CompareAndSwap(true, false) {
			recorded := httptest.NewRecorder()
			f.handler.ServeHTTP(recorded, r)
			if recorded.Code != 201 {
				t.Errorf("public update failed before response loss: %d %s", recorded.Code, recorded.Body.String())
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(recorded.Code)
			_, _ = io.WriteString(w, "{")
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	c := f.newClient(t, f.admin, f.sender, server.URL)
	job, err := c.CreateJob(t.Context(), "progress-job", client.CreateJobRequest{Title: "Progress without brief edits", Brief: "Standing instructions", WorkerID: f.sender})
	if err != nil {
		t.Fatal(err)
	}
	request := client.AppendUpdateRequest{ExpectedJobRevision: 1, Body: "  exact narrative <text>\n"}
	_, err = c.AppendJobUpdate(t.Context(), "lost-progress", job.ID, request)
	var unknown *client.Error
	if !errors.As(err, &unknown) || !unknown.OutcomeUnknown {
		t.Fatalf("lost committed append was not uncertain: %v", err)
	}
	first, err := c.AppendJobUpdate(t.Context(), "lost-progress", job.ID, request)
	if err != nil || first.JobRevision != 1 || first.Outcome != "created" {
		t.Fatal(first, err)
	}
	updates, err := c.JobUpdates(t.Context(), job.ID, client.PageOptions{})
	if err != nil || len(updates.Items) != 1 || updates.Items[0].ID != first.ID || updates.Items[0].Body != request.Body || updates.Items[0].Evidence == nil || updates.Items[0].ProjectID != f.project || updates.Items[0].WorkerID != f.sender {
		t.Fatal(updates, err)
	}
	empty, err := c.JobChecklist(t.Context(), job.ID)
	if err != nil || empty.Revision != 0 || empty.Items == nil || len(empty.Items) != 0 {
		t.Fatal(empty, err)
	}
	items := []client.ChecklistItem{{ID: "inspect", Text: "  inspect exact inputs\n"}, {ID: "verify_2", Text: "Verify result", Done: true}}
	set := client.ReplaceChecklistRequest{ExpectedJobRevision: 1, ExpectedRevision: 0, Items: items}
	changed, err := c.ReplaceJobChecklist(t.Context(), "first-plan", job.ID, set)
	if err != nil || changed.Revision != 1 || changed.Outcome != "updated" {
		t.Fatal(changed, err)
	}
	beforeNoop := f.eventCount(t)
	set.ExpectedRevision = 1
	unchanged, err := c.ReplaceJobChecklist(t.Context(), "same-plan", job.ID, set)
	if err != nil || unchanged.Revision != 1 || unchanged.Outcome != "unchanged" || f.eventCount(t) != beforeNoop {
		t.Fatal("identical plan appended an event", unchanged, err)
	}
	title := "New standing brief revision"
	_, err = c.EditJob(t.Context(), "progress-edit", job.ID, client.EditJobRequest{ExpectedRevision: 1, Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := c.AppendJobUpdate(t.Context(), "lost-progress", job.ID, request)
	if err != nil || replay != first {
		t.Fatal("original append changed after job edit", replay, err)
	}
	_, err = c.AppendJobUpdate(t.Context(), "stale-update", job.ID, request)
	checkError(t, err, "revision_conflict")
	set.ExpectedJobRevision = 2
	set.Items = []client.ChecklistItem{items[1], items[0]}
	second, err := c.ReplaceJobChecklist(t.Context(), "reorder-plan", job.ID, set)
	if err != nil || second.Revision != 2 || second.JobRevision != 2 {
		t.Fatal(second, err)
	}
	beforeRead := f.eventCount(t)
	current, err := c.JobChecklist(t.Context(), job.ID)
	if err != nil || current.JobRevision != 2 || current.Revision != 2 || !reflect.DeepEqual(current.Items, set.Items) {
		t.Fatal(current, err)
	}
	history, err := c.JobChecklistHistory(t.Context(), job.ID, client.PageOptions{Limit: 1})
	if err != nil || len(history.Items) != 1 || history.Items[0].Revision != 2 || history.Items[0].JobRevision != 2 || !history.HasMore {
		t.Fatal(history, err)
	}
	older, err := c.JobChecklistHistory(t.Context(), job.ID, client.PageOptions{Limit: 1, Cursor: history.Cursor})
	if err != nil || len(older.Items) != 1 || older.Items[0].Revision != 1 || older.Items[0].JobRevision != 1 || !reflect.DeepEqual(older.Items[0].Items, items) || older.HasMore {
		t.Fatal(older, err)
	}
	jobHistory, err := c.JobHistory(t.Context(), job.ID, client.PageOptions{})
	if err != nil || len(jobHistory.Items) != 2 || f.eventCount(t) != beforeRead || f.providerCalls.Load() != 1 {
		t.Fatal("progress changed brief history/provider/read effects", jobHistory, err)
	}
	var receipts int
	// One exact-key append retains one public update and one replay receipt.
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM receipts WHERE key='lost-progress'`).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatal(receipts, err)
	}
}

func TestJobProgressClientBoundRoutesAndExplicitSinglePages(t *testing.T) {
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("binding lost")
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/projects/project-one/jobs/job-one/updates":
			var in client.AppendUpdateRequest
			if json.NewDecoder(r.Body).Decode(&in) != nil || in.Evidence == nil || len(in.Evidence) != 0 || in.Body != "  body\n" || r.Header.Get("Idempotency-Key") != "update-key" {
				t.Error("update normalized text/key incorrectly")
			}
			write(w, 201, client.UpdateReceipt{ID: "update-one", Sequence: 8, JobRevision: 2, Outcome: "created"})
		case "GET /api/v1/projects/project-one/jobs/job-one/updates":
			if r.URL.Query().Get("limit") != "20" || len(r.URL.Query()) != 1 {
				t.Error("default page is not bounded20")
			}
			write(w, 200, client.Page[client.Update]{Items: []client.Update{progressUpdate()}, HasMore: true, Cursor: "explicit-next"})
		case "GET /api/v1/projects/project-one/jobs/job-one/checklist":
			write(w, 200, progressChecklist())
		case "PUT /api/v1/projects/project-one/jobs/job-one/checklist":
			var in client.ReplaceChecklistRequest
			if json.NewDecoder(r.Body).Decode(&in) != nil || in.ExpectedRevision != 0 || in.Items == nil || len(in.Items) != 0 || r.Header.Get("Idempotency-Key") != "empty-plan" {
				t.Error("explicit zero/empty plan lost")
			}
			write(w, 200, client.ChecklistReceipt{ID: "job-one", Revision: 0, JobRevision: 2, Outcome: "unchanged"})
		case "GET /api/v1/projects/project-one/jobs/job-one/checklist/history":
			if r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("cursor") != "chosen-cursor" {
				t.Error("history query changed")
			}
			write(w, 200, client.Page[client.ChecklistHistoryEntry]{Items: []client.ChecklistHistoryEntry{{Revision: 3, JobRevision: 2, ActorID: "actor-one", WorkerID: "", Items: progressChecklist().Items, CreatedAt: "2026-10-15T00:00:00Z"}}, Cursor: "next", HasMore: true})
		default:
			t.Error("unexpected request", r.Method, r.URL.Path)
		}
	}))
	defer s.Close()
	c := newClient(t, s)
	if _, err := c.AppendJobUpdate(t.Context(), "update-key", "job-one", client.AppendUpdateRequest{ExpectedJobRevision: 2, Body: "  body\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.JobUpdates(t.Context(), "job-one", client.PageOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.JobChecklist(t.Context(), "job-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReplaceJobChecklist(t.Context(), "empty-plan", "job-one", client.ReplaceChecklistRequest{ExpectedJobRevision: 2, Items: []client.ChecklistItem{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.JobChecklistHistory(t.Context(), "job-one", client.PageOptions{Limit: 1, Cursor: "chosen-cursor"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 5 {
		t.Fatal("automatic retry/fanout", calls.Load())
	}
}

func TestJobProgressClientStrictResponsesScopeOrderAndBudgets(t *testing.T) {
	update := progressJSON(t, client.Page[client.Update]{Items: []client.Update{progressUpdate()}})
	checklist := progressJSON(t, progressChecklist())
	history := progressJSON(t, client.Page[client.ChecklistHistoryEntry]{Items: []client.ChecklistHistoryEntry{{Revision: 3, JobRevision: 2, ActorID: "principal-one", WorkerID: "", Items: progressChecklist().Items, CreatedAt: "2026-10-15T00:00:00Z"}}})
	for _, test := range []struct{ name, route, body string }{
		{"update-project", "updates", strings.Replace(update, "project-one", "project-two", 1)},
		{"update-job", "updates", strings.Replace(update, "job-one", "job-two", 1)},
		{"update-null-evidence", "updates", strings.Replace(update, `["proof-one"]`, "null", 1)},
		{"update-missing-worker", "updates", strings.Replace(update, `"worker_id":"",`, "", 1)},
		{"update-null-body", "updates", strings.Replace(update, `"body":"  completed \u003cliteral\u003e\n"`, `"body":null`, 1)},
		{"update-unknown", "updates", strings.Replace(update, `"sequence":8`, `"sequence":8,"extra":1`, 1)},
		{"update-null-items", "updates", `{"items":null,"has_more":false}`},
		{"update-zero-time", "updates", strings.Replace(update, `2026-10-15T00:00:00Z`, `0001-01-01T00:00:00Z`, 1)},
		{"checklist-wrong-job", "checklist", strings.Replace(checklist, "job-one", "job-two", 1)},
		{"checklist-zero-with-items", "checklist", strings.Replace(checklist, `"revision":3`, `"revision":0`, 1)},
		{"checklist-null-items", "checklist", `{"job_id":"job-one","job_revision":2,"revision":0,"items":null}`},
		{"checklist-unknown", "checklist", strings.Replace(checklist, `"done":false`, `"done":false,"extra":1`, 1)},
		{"checklist-missing-false", "checklist", strings.Replace(checklist, `,"done":false`, "", 1)},
		{"checklist-null-false", "checklist", strings.Replace(checklist, `"done":false`, `"done":null`, 1)},
		{"checklist-alias", "checklist", strings.Replace(checklist, `"done":false`, `"Done":false`, 1)},
		{"checklist-duplicate", "checklist", strings.Replace(checklist, `"revision":3`, `"revision":3,"revision":3`, 1)},
		{"checklist-budget", "checklist", strings.Repeat(" ", 128<<10) + checklist},
		{"archive-budget", "updates", strings.Repeat(" ", 4<<20) + update},
		{"history-budget", "history", strings.Repeat(" ", 4<<20) + history},
		{"history-zero", "history", strings.Replace(history, `"revision":3`, `"revision":0`, 1)},
		{"history-unknown", "history", strings.Replace(history, `"actor_id":"principal-one"`, `"actor_id":"principal-one","project_id":"other"`, 1)},
		{"history-missing-false", "history", strings.Replace(history, `,"done":false`, "", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, calls := progressResponseClient(t, test.body)
			var err error
			switch test.route {
			case "updates":
				_, err = c.JobUpdates(t.Context(), "job-one", client.PageOptions{})
			case "checklist":
				_, err = c.JobChecklist(t.Context(), "job-one")
			case "history":
				_, err = c.JobChecklistHistory(t.Context(), "job-one", client.PageOptions{})
			}
			checkError(t, err, "invalid_response")
			if calls.Load() != 1 {
				t.Fatal("invalid response caused automatic followup")
			}
		})
	}
	for _, duplicateID := range []bool{false, true} {
		one, two := progressUpdate(), progressUpdate()
		two.Sequence = 7
		if !duplicateID {
			two.ID = "update-two"
			two.Sequence = 9
		}
		c, _ := progressResponseClient(t, progressJSON(t, client.Page[client.Update]{Items: []client.Update{one, two}}))
		_, err := c.JobUpdates(t.Context(), "job-one", client.PageOptions{})
		checkError(t, err, "invalid_response")
	}
	entry := client.ChecklistHistoryEntry{Revision: 2, JobRevision: 1, ActorID: "actor", Items: []client.ChecklistItem{}, CreatedAt: "2026-10-15T00:00:00Z"}
	c, _ := progressResponseClient(t, progressJSON(t, client.Page[client.ChecklistHistoryEntry]{Items: []client.ChecklistHistoryEntry{entry, entry}}))
	_, err := c.JobChecklistHistory(t.Context(), "job-one", client.PageOptions{})
	checkError(t, err, "invalid_response")
}

func TestJobProgressClientLocalValidationAndExplicitChecklistDecode(t *testing.T) {
	for _, raw := range []string{"null", "{}", `[{"id":"one","text":"task"}]`, `[{"id":"one","text":"task","done":null}]`, `[{"id":"one","text":"task","Done":false}]`, `[{"id":"one","text":"task","done":false,"done":false}]`, `[{"id":"one","text":"task","done":false,"other":1}]`, `[{"id":"one","text":"\ud800","done":false}]`, `[{"id":"é","text":"task","done":false}]`, `[{"id":"one","text":"task","done":false},{"id":"one","text":"again","done":true}]`, strings.Repeat(" ", 256<<10) + "[]"} {
		if _, err := client.DecodeJobChecklistItems([]byte(raw)); err == nil {
			t.Fatalf("malformed checklist accepted: %.80s", raw)
		}
	}
	for _, raw := range []string{"[]", `[{"id":"item_One-2","text":"  task\n","done":false}]`} {
		items, err := client.DecodeJobChecklistItems([]byte(raw))
		if err != nil || items == nil {
			t.Fatal("valid explicit array refused", err)
		}
	}
	base := client.AppendUpdateRequest{ExpectedJobRevision: math.MaxInt64, Body: strings.Repeat("a", 8<<10), Evidence: []string{strings.Repeat("e", 1024), strings.Repeat("e", 1024), strings.Repeat("e", 1024), strings.Repeat("e", 1024), strings.Repeat("e", 1024), strings.Repeat("e", 1024), strings.Repeat("e", 1024), strings.Repeat("e", 1024)}}
	if err := client.ValidateJobUpdateRequest(base); err != nil {
		t.Fatal("exact aggregate/max job revision refused", err)
	}
	for _, change := range []func(*client.AppendUpdateRequest){
		func(r *client.AppendUpdateRequest) { r.ExpectedJobRevision = 0 },
		func(r *client.AppendUpdateRequest) { r.Body = " " },
		func(r *client.AppendUpdateRequest) { r.Body = "bad\x7f" },
		func(r *client.AppendUpdateRequest) { r.Body = string([]byte{255}) },
		func(r *client.AppendUpdateRequest) { r.Body += "a" },
		func(r *client.AppendUpdateRequest) { r.Evidence = append(r.Evidence, "x") },
		func(r *client.AppendUpdateRequest) { r.Evidence = []string{" "} },
	} {
		r := base
		change(&r)
		if client.ValidateJobUpdateRequest(r) == nil {
			t.Fatal("invalid update locally accepted")
		}
		if _, err := (&client.Client{}).AppendJobUpdate(t.Context(), "key", "job-one", r); err == nil {
			t.Fatal("invalid update reached an unconfigured client")
		}
	}
	if err := client.ValidateJobChecklistRequest(client.ReplaceChecklistRequest{ExpectedJobRevision: math.MaxInt64, Items: []client.ChecklistItem{}}); err != nil {
		t.Fatal(err)
	}
	for _, request := range []client.ReplaceChecklistRequest{{ExpectedJobRevision: 1}, {ExpectedJobRevision: 0, Items: []client.ChecklistItem{}}, {ExpectedJobRevision: 1, ExpectedRevision: -1, Items: []client.ChecklistItem{}}, {ExpectedJobRevision: 1, ExpectedRevision: math.MaxInt64, Items: []client.ChecklistItem{}}, {ExpectedJobRevision: 1, Items: []client.ChecklistItem{{ID: "id:invalid", Text: "x"}}}, {ExpectedJobRevision: 1, Items: []client.ChecklistItem{{ID: "valid", Text: "bad\x01"}}}} {
		if _, err := (&client.Client{}).ReplaceJobChecklist(t.Context(), "key", "job-one", request); err == nil {
			t.Fatal("invalid plan reached unconfigured client")
		}
	}
	c := &client.Client{}
	if _, err := c.JobUpdates(t.Context(), "../bad", client.PageOptions{}); err == nil {
		t.Fatal("unsafe job accepted")
	}
	if _, err := c.JobChecklist(t.Context(), "bad/id"); err == nil {
		t.Fatal("unsafe job accepted")
	}
	if _, err := c.JobChecklistHistory(t.Context(), "job-one", client.PageOptions{Limit: 101}); err == nil {
		t.Fatal("unbounded page accepted")
	}
}

func TestJobProgressClientChecklistAggregateAndEscapedReadBoundaries(t *testing.T) {
	items := make([]client.ChecklistItem, 16)
	for i := range items {
		items[i] = client.ChecklistItem{ID: string(rune('a' + i)), Text: strings.Repeat("&", 1023)}
	}
	request := client.ReplaceChecklistRequest{ExpectedJobRevision: 1, Items: items}
	if err := client.ValidateJobChecklistRequest(request); err != nil {
		t.Fatal("exact16KiB plan refused", err)
	}
	body := progressJSON(t, client.Checklist{JobID: "job-one", JobRevision: 1, Revision: math.MaxInt64, Items: items})
	if len(body) < 90<<10 || len(body) >= 128<<10 {
		t.Fatal("escaped fixture does not exercise the current response bound")
	}
	c, _ := progressResponseClient(t, body)
	got, err := c.JobChecklist(t.Context(), "job-one")
	if err != nil || got.Revision != math.MaxInt64 || !reflect.DeepEqual(got.Items, items) {
		t.Fatal("bounded escaped plan was truncated or writable guard applied to read", err)
	}
	items[0].Text += "&"
	if client.ValidateJobChecklistRequest(request) == nil {
		t.Fatal("one-byte aggregate overflow accepted")
	}
	many := make([]client.ChecklistItem, 101)
	for i := range many {
		many[i] = client.ChecklistItem{ID: string(rune('a'+i/26)) + string(rune('a'+i%26)), Text: "task"}
	}
	request.Items = many[:100]
	if err := client.ValidateJobChecklistRequest(request); err != nil {
		t.Fatal("100 distinct items refused", err)
	}
	request.Items = many
	if client.ValidateJobChecklistRequest(request) == nil {
		t.Fatal("101 item plan accepted")
	}
}

func TestJobProgressClientInvalidUpdateReceiptRetainsUncertainty(t *testing.T) {
	for _, body := range []string{
		`{"id":"update-one","sequence":0,"job_revision":2,"outcome":"created"}`,
		`{"id":"update-one","sequence":1,"job_revision":3,"outcome":"created"}`,
		`{"id":"update-one","sequence":1,"job_revision":2,"outcome":"updated"}`,
		`{"id":"update-one","sequence":1,"job_revision":2,"outcome":"created","body":"unrequested"}`,
	} {
		var writes atomic.Int64
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			writes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, body)
		}))
		c := newClient(t, s)
		_, err := c.AppendJobUpdate(t.Context(), "retain-key", "job-one", client.AppendUpdateRequest{ExpectedJobRevision: 2, Body: "literal update"})
		var unknown *client.Error
		if !errors.As(err, &unknown) || !unknown.OutcomeUnknown || writes.Load() != 1 {
			t.Fatal("invalid successful append receipt was treated as known refusal or retried", err)
		}
		c.Close()
		s.Close()
	}
}

func TestJobProgressClientDispatchedReceiptAndCancellationAreUnknown(t *testing.T) {
	for _, body := range []string{`{"id":"job-two","revision":1,"job_revision":2,"outcome":"updated"}`, `{"id":"job-one","revision":0,"job_revision":2,"outcome":"updated"}`, `{"id":"job-one","revision":1,"job_revision":2,"outcome":"created"}`, `{"id":"job-one","revision":1,"job_revision":2,"outcome":"updated","extra":1}`} {
		c, calls := progressResponseClient(t, body)
		_, err := c.ReplaceJobChecklist(t.Context(), "explicit-key", "job-one", client.ReplaceChecklistRequest{ExpectedJobRevision: 2, Items: []client.ChecklistItem{}})
		var failure *client.Error
		if !errors.As(err, &failure) || !failure.OutcomeUnknown || calls.Load() != 1 {
			t.Fatal("dispatched invalid receipt not unknown", err)
		}
	}
	started, release := make(chan struct{}), make(chan struct{})
	var writes atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		writes.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-release
	}))
	defer s.Close()
	defer close(release)
	c := newClient(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.AppendJobUpdate(ctx, "cancel-key", "job-one", client.AppendUpdateRequest{ExpectedJobRevision: 1, Body: "current observation"})
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("update did not dispatch")
	}
	cancel()
	select {
	case err := <-done:
		var failure *client.Error
		if !errors.As(err, &failure) || failure.Code != "canceled" || !failure.OutcomeUnknown || writes.Load() != 1 {
			t.Fatal(err, writes.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation was not prompt")
	}
}
