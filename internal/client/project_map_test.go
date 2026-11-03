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
	"github.com/benhook1013/FireController/internal/projectmap"
)

func mapEditable() client.ProjectMapEditable {
	return client.ProjectMapEditable{Kind: "workstream", Title: "Build map", Description: "  Literal <script>text</script>\n", State: "RECONSIDER AFTER PROOF", Where: "Current work", Next: "Next milestone", Phases: []client.ProjectMapPhase{{ID: "proof_1", Name: "Proof", State: "Deferred"}}, Links: []client.ProjectMapLink{{Label: "Source", URL: "https://example.com/source#section"}}}
}

func mapEntry() client.ProjectMapEntry {
	e := mapEditable()
	return client.ProjectMapEntry{ID: "entry-one", ProjectID: "project-one", Revision: 2, Kind: e.Kind, Title: e.Title, Description: e.Description, State: e.State, Where: e.Where, Next: e.Next, Trigger: e.Trigger, Phases: e.Phases, Links: e.Links, Archived: e.Archived, CreatedAt: "2026-10-16T01:00:00.100000000Z", UpdatedAt: "2026-10-16T00:00:00.000000000Z"}
}

func TestProjectMapClientExactBoundPathsRequestsAndSinglePages(t *testing.T) {
	var calls atomic.Int64
	entry := mapEntry()
	summary := client.ProjectMapSummary{ID: entry.ID, Kind: entry.Kind, Title: entry.Title, State: entry.State, Revision: entry.Revision}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("map call lost selected profile identity")
		}
		base := "/api/v1/projects/project-one"
		switch r.Method + " " + r.URL.Path {
		case "POST " + base + "/map/entries":
			var got client.ProjectMapEditable
			if json.NewDecoder(r.Body).Decode(&got) != nil || !reflect.DeepEqual(got, mapEditable()) || r.Header.Get("Idempotency-Key") != "create-key" || r.Header.Get("Content-Type") != "application/json" {
				t.Error("create changed editable body or mutation key")
			}
			write(w, 201, client.ProjectMapReceipt{ID: entry.ID, Revision: 1, Outcome: "created"})
		case "PUT " + base + "/map/entries/entry-one":
			var got client.ReplaceProjectMapEntryRequest
			if json.NewDecoder(r.Body).Decode(&got) != nil || got.ExpectedRevision != 1 || !reflect.DeepEqual(got.Entry, mapEditable()) || r.Header.Get("Idempotency-Key") != "replace-key" {
				t.Error("replace lost independent entry guard or body")
			}
			write(w, 200, client.ProjectMapReceipt{ID: entry.ID, Revision: 2, Outcome: "updated"})
		case "GET " + base + "/map/entries":
			if r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("kind") != "workstream" || r.URL.Query().Get("archived") != "false" || r.URL.Query().Get("cursor") != "" {
				t.Error("list changed filters or auto-followed continuation")
			}
			write(w, 200, client.Page[client.ProjectMapSummary]{Items: []client.ProjectMapSummary{summary}, Cursor: "continue-one", HasMore: true})
		case "GET " + base + "/map/entries/entry-one":
			write(w, 200, entry)
		case "GET " + base + "/map/entries/entry-one/history":
			if r.URL.Query().Get("limit") != "25" {
				t.Error("history did not use bounded default")
			}
			write(w, 200, client.Page[client.ProjectMapHistorySummary]{Items: []client.ProjectMapHistorySummary{{Revision: 2, ActorID: "principal-one", CreatedAt: entry.UpdatedAt}}})
		case "GET " + base + "/map/entries/entry-one/history/2":
			write(w, 200, client.ProjectMapRevision{Revision: 2, ActorID: "principal-one", CreatedAt: entry.UpdatedAt, State: entry})
		case "GET " + base + "/jobs/job-one/workstream":
			write(w, 200, client.JobWorkstream{JobID: "job-one"})
		case "PUT " + base + "/jobs/job-one/workstream":
			var got client.ReplaceJobWorkstreamRequest
			if json.NewDecoder(r.Body).Decode(&got) != nil || got.ExpectedRevision != 0 || got.WorkstreamID != entry.ID || r.Header.Get("Idempotency-Key") != "association-key" {
				t.Error("association lost initial zero guard")
			}
			write(w, 200, client.JobWorkstreamReceipt{JobID: "job-one", Revision: 1, Outcome: "updated"})
		case "GET " + base + "/jobs/job-one/workstream/history":
			if r.URL.Query().Get("limit") != "25" {
				t.Error("association history did not use bounded default")
			}
			write(w, 200, client.Page[client.JobWorkstreamHistoryEntry]{Items: []client.JobWorkstreamHistoryEntry{{Revision: 1, WorkstreamID: entry.ID, WorkstreamTitle: "Captured old title", JobRevision: 7, ActorID: "principal-one", CreatedAt: entry.UpdatedAt}}})
		default:
			t.Error("unexpected map call", r.Method, r.URL.Path)
			write(w, 500, map[string]string{})
		}
	}))
	defer s.Close()
	c := newClient(t, s)
	if _, err := c.CreateProjectMapEntry(t.Context(), "create-key", mapEditable()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReplaceProjectMapEntry(t.Context(), "replace-key", entry.ID, client.ReplaceProjectMapEntryRequest{ExpectedRevision: 1, Entry: mapEditable()}); err != nil {
		t.Fatal(err)
	}
	page, err := c.ProjectMapEntries(t.Context(), client.PageOptions{Limit: 1}, client.ProjectMapFilter{Kind: "workstream"})
	if err != nil || len(page.Items) != 1 || !page.HasMore || page.Cursor != "continue-one" {
		t.Fatal(page, err)
	}
	current, err := c.ProjectMapEntry(t.Context(), entry.ID)
	if err != nil || !reflect.DeepEqual(current, entry) {
		t.Fatal(current, err)
	}
	history, err := c.ProjectMapHistory(t.Context(), entry.ID, client.PageOptions{})
	if err != nil || len(history.Items) != 1 {
		t.Fatal(history, err)
	}
	old, err := c.ProjectMapRevision(t.Context(), entry.ID, 2)
	if err != nil || old.State.Title != entry.Title {
		t.Fatal(old, err)
	}
	association, err := c.JobWorkstream(t.Context(), "job-one")
	if err != nil || association.Revision != 0 || association.WorkstreamID != "" {
		t.Fatal(association, err)
	}
	if _, err = c.ReplaceJobWorkstream(t.Context(), "association-key", "job-one", client.ReplaceJobWorkstreamRequest{WorkstreamID: entry.ID}); err != nil {
		t.Fatal(err)
	}
	refs, err := c.JobWorkstreamHistory(t.Context(), "job-one", client.PageOptions{})
	if err != nil || refs.Items[0].WorkstreamTitle != "Captured old title" || refs.Items[0].JobRevision != 7 || calls.Load() != 9 {
		t.Fatal(refs, err, calls.Load())
	}
}

func TestProjectMapClientLocalValidationNoHTTP(t *testing.T) {
	c, calls := progressResponseClient(t, `{}`)
	for _, change := range []func(*client.ProjectMapEditable){
		func(e *client.ProjectMapEditable) { e.Kind = "unknown" },
		func(e *client.ProjectMapEditable) { e.Title = " " },
		func(e *client.ProjectMapEditable) { e.Description = string([]byte{0xff}) },
		func(e *client.ProjectMapEditable) { e.Trigger = "wrong kind" },
		func(e *client.ProjectMapEditable) { e.Kind = "return_point" },
		func(e *client.ProjectMapEditable) { e.State = "State\nsecond line" },
		func(e *client.ProjectMapEditable) { e.Phases[0].Name = "Proof\tstep" },
		func(e *client.ProjectMapEditable) { e.Phases = append(e.Phases, e.Phases[0]) },
		func(e *client.ProjectMapEditable) { e.Phases[0].ID = "Upper" },
		func(e *client.ProjectMapEditable) { e.Links[0].URL = "javascript:alert(1)" },
		func(e *client.ProjectMapEditable) { e.Links[0].URL = "https://user:secret@example.com" },
		func(e *client.ProjectMapEditable) { e.Links[0].URL = "//example.com/path" },
		func(e *client.ProjectMapEditable) { e.Links[0].URL = "https://example.com/unsafe space" },
		func(e *client.ProjectMapEditable) { e.Phases = make([]client.ProjectMapPhase, 65) },
		func(e *client.ProjectMapEditable) { e.Links = make([]client.ProjectMapLink, 13) },
		func(e *client.ProjectMapEditable) { e.Description = strings.Repeat("x", 8193) },
		func(e *client.ProjectMapEditable) {
			e.Description = strings.Repeat("<", 8192)
			e.Where = e.Description
			e.Next = e.Description
		},
	} {
		e := mapEditable()
		change(&e)
		_, err := c.CreateProjectMapEntry(t.Context(), "explicit", e)
		checkError(t, err, "validation_failed")
	}
	for _, revision := range []int64{0, -1, math.MaxInt64} {
		_, err := c.ReplaceProjectMapEntry(t.Context(), "explicit", "entry-one", client.ReplaceProjectMapEntryRequest{ExpectedRevision: revision, Entry: mapEditable()})
		checkError(t, err, "invalid_revision")
	}
	for _, request := range []client.ReplaceJobWorkstreamRequest{{ExpectedRevision: -1}, {ExpectedRevision: math.MaxInt64}} {
		_, err := c.ReplaceJobWorkstream(t.Context(), "explicit", "job-one", request)
		checkError(t, err, "invalid_revision")
	}
	for _, run := range []func() error{
		func() error { _, err := c.ProjectMapEntry(t.Context(), "../foreign"); return err },
		func() error { _, err := c.ProjectMapRevision(t.Context(), "entry-one", 0); return err },
		func() error {
			_, err := c.ProjectMapEntries(t.Context(), client.PageOptions{Limit: 101}, client.ProjectMapFilter{})
			return err
		},
		func() error {
			_, err := c.ProjectMapEntries(t.Context(), client.PageOptions{}, client.ProjectMapFilter{Kind: "all"})
			return err
		},
		func() error {
			_, err := c.ProjectMapHistory(t.Context(), "entry-one", client.PageOptions{Cursor: " padded"})
			return err
		},
		func() error { _, err := c.JobWorkstream(t.Context(), "bad/job"); return err },
		func() error {
			_, err := c.JobWorkstreamHistory(t.Context(), "job-one", client.PageOptions{Limit: -1})
			return err
		},
		func() error {
			_, err := c.ReplaceJobWorkstream(t.Context(), "explicit", "job-one", client.ReplaceJobWorkstreamRequest{WorkstreamID: "bad/id"})
			return err
		},
		func() error { _, err := c.CreateProjectMapEntry(t.Context(), "", mapEditable()); return err },
	} {
		if err := run(); err == nil {
			t.Fatal("invalid argument admitted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("local validation sent HTTP", calls.Load())
	}
}

func TestProjectMapClientNormalizesCopiedLabelsAndEmptyArrays(t *testing.T) {
	original := mapEditable()
	original.Title = "  Build map\n"
	original.Phases[0].Name = "  Proof\t"
	original.Links[0].Label = "  Source  "
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		var got client.ProjectMapEditable
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if got.Kind == "workstream" {
			if !reflect.DeepEqual(got, mapEditable()) {
				t.Error("labels or prose were not normalized as selected", got)
			}
		} else if got.Phases == nil || got.Links == nil || len(got.Phases) != 0 || len(got.Links) != 0 {
			t.Error("empty input collections did not become explicit empty arrays")
		}
		write(w, 201, client.ProjectMapReceipt{ID: "entry-one", Revision: 1, Outcome: "created"})
	}))
	defer s.Close()
	c := newClient(t, s)
	if _, err := c.CreateProjectMapEntry(t.Context(), "normalized", original); err != nil {
		t.Fatal(err)
	}
	if original.Title != "  Build map\n" || original.Phases[0].Name != "  Proof\t" || original.Links[0].Label != "  Source  " {
		t.Fatal("normalization mutated caller state")
	}
	if _, err := c.CreateProjectMapEntry(t.Context(), "empty", client.ProjectMapEditable{Kind: "return_point", Title: "Deferred", Trigger: "After prerequisite"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("normalization added a read", calls.Load())
	}
}

func TestProjectMapClientDecodeEditableDefaultsAndRejectsMalformedInput(t *testing.T) {
	for _, raw := range []string{`{"kind":"workstream","title":"  Minimal  "}`, `{"kind":"workstream","title":"  Minimal  ","phases":null,"links":null}`, `{"kind":"workstream","title":"  Minimal  ","phases":[{"id":"proof","name":" Proof "}]}`} {
		entry, err := client.DecodeProjectMapEditable([]byte(raw))
		if err != nil || entry.Title != "Minimal" || entry.Phases == nil || entry.Links == nil || entry.State != "" || entry.Archived {
			t.Fatal("optional fields did not default safely", entry, err)
		}
	}
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"kind":"workstream"}`, `{"title":"Missing kind"}`,
		`{"kind":"workstream","title":"A","description":null}`,
		`{"kind":"workstream","title":"A","archived":null}`,
		`{"kind":"workstream","title":"A","title":"B"}`,
		`{"kind":"workstream","title":"A","Title":"B"}`,
		`{"kind":"workstream","title":"\ud800"}`,
		`{"kind":"workstream","title":"A","unknown":false}`,
		`{"kind":"workstream","title":"A","phases":[{"id":"p","name":"Proof","state":null}]}`,
		`{"kind":"workstream","title":"A","phases":[{"id":"p","name":"Proof","archived":null}]}`,
		`{"kind":"workstream","title":"A","phases":[{"id":"p"}]}`,
		`{"kind":"workstream","title":"A","links":[{"label":"Source"}]}`,
		`{"kind":"workstream","title":"A","links":[{"label":"Source","url":null}]}`,
	} {
		_, err := client.DecodeProjectMapEditable([]byte(raw))
		checkError(t, err, "validation_failed")
	}
}

func TestProjectMapClientStrictReadShapesAndCapturedIdentities(t *testing.T) {
	good := progressJSON(t, mapEntry())
	for _, raw := range []string{
		strings.Replace(good, `"project_id":"project-one"`, `"project_id":"foreign"`, 1),
		strings.Replace(good, `"archived":false`, `"archived":null`, 1),
		strings.Replace(good, `"archived":false,`, ``, 1),
		strings.Replace(good, `"revision":2`, `"revision":2.0`, 1),
		strings.Replace(good, `"kind":"workstream"`, `"kind":"workstream","kind":"return_point"`, 1),
		strings.Replace(good, `"description":`, `"Description":`, 1),
		strings.Replace(good, `"links":[{"label":"Source","url":"https://example.com/source#section"}]`, `"links":null`, 1),
		strings.Replace(good, `"id":"proof_1"`, `"id":"../bad"`, 1),
	} {
		c, _ := progressResponseClient(t, raw)
		_, err := c.ProjectMapEntry(t.Context(), "entry-one")
		checkError(t, err, "invalid_response")
	}
	for _, raw := range []string{`{"items":[],"has_more":null}`, `{"items":[]}`, `{"items":null,"has_more":false}`, `{"items":[],"has_more":false,"cursor":null}`, `{"items":[],"has_more":true,"cursor":"next"}`} {
		c, _ := progressResponseClient(t, raw)
		_, err := c.ProjectMapEntries(t.Context(), client.PageOptions{}, client.ProjectMapFilter{})
		checkError(t, err, "invalid_response")
	}
	for _, ref := range []client.JobWorkstream{{JobID: "foreign"}, {JobID: "job-one", WorkstreamID: "entry-one", Revision: 0}, {JobID: "job-one", WorkstreamTitle: "No identity", Revision: 1}} {
		c, _ := progressResponseClient(t, progressJSON(t, ref))
		_, err := c.JobWorkstream(t.Context(), "job-one")
		checkError(t, err, "invalid_response")
	}
	revision := client.ProjectMapRevision{Revision: 1, ActorID: "principal-one", CreatedAt: mapEntry().UpdatedAt, State: mapEntry()}
	c, _ := progressResponseClient(t, progressJSON(t, revision))
	_, err := c.ProjectMapRevision(t.Context(), "entry-one", 1)
	checkError(t, err, "invalid_response")
	history := []client.ProjectMapHistorySummary{{Revision: 2, ActorID: "principal-one", CreatedAt: mapEntry().UpdatedAt}, {Revision: 2, ActorID: "principal-one", CreatedAt: mapEntry().UpdatedAt}}
	c, _ = progressResponseClient(t, progressJSON(t, client.Page[client.ProjectMapHistorySummary]{Items: history}))
	_, err = c.ProjectMapHistory(t.Context(), "entry-one", client.PageOptions{})
	checkError(t, err, "invalid_response")
}

func TestProjectMapClientCancellationAndKnownConflictVersusUnknownWrites(t *testing.T) {
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		switch r.Header.Get("Idempotency-Key") {
		case "known-conflict":
			write(w, 409, map[string]any{"error": map[string]any{"code": "revision_conflict", "message": "Stale revision.", "details": map[string]any{"current_revision": 2}}, "request_id": "request-one"})
		case "malformed-receipt":
			write(w, 200, client.JobWorkstreamReceipt{JobID: "foreign", Revision: 1, Outcome: "updated"})
		default:
			<-r.Context().Done()
		}
	}))
	defer s.Close()
	c := newClient(t, s)
	_, err := c.ReplaceProjectMapEntry(t.Context(), "known-conflict", "entry-one", client.ReplaceProjectMapEntryRequest{ExpectedRevision: 1, Entry: mapEditable()})
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "revision_conflict" || failure.OutcomeUnknown || failure.Status != 409 {
		t.Fatal("known refusal became uncertainty", err)
	}
	_, err = c.ReplaceJobWorkstream(t.Context(), "malformed-receipt", "job-one", client.ReplaceJobWorkstreamRequest{WorkstreamID: "entry-one"})
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || failure.Code != "invalid_response" {
		t.Fatal("malformed dispatched receipt lost uncertainty", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	_, err = c.ProjectMapEntry(ctx, "entry-one")
	if !errors.As(err, &failure) || failure.Code != "deadline_exceeded" || failure.OutcomeUnknown {
		t.Fatal("read deadline semantics", err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	_, err = c.CreateProjectMapEntry(ctx, "uncertain", mapEditable())
	if !errors.As(err, &failure) || failure.Code != "deadline_exceeded" || !failure.OutcomeUnknown || calls.Load() != 4 {
		t.Fatal("mutation deadline retried/lost uncertainty", err, calls.Load())
	}
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	_, err = c.CreateProjectMapEntry(ctx, "canceled-before-dispatch", mapEditable())
	if !errors.As(err, &failure) || failure.Code != "canceled" || failure.OutcomeUnknown || calls.Load() != 4 {
		t.Fatal("pre-canceled mutation dispatched or became uncertain", err, calls.Load())
	}
}

func TestProjectMapClientPublicAPICommittedReplayHistoryAndIndependentJobReference(t *testing.T) {
	f := newMutationFixture(t)
	if err := projectmap.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	projectmap.Register(mux, f.store)
	var lose atomic.Bool
	var posts atomic.Int64
	lose.Store(true)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/map/entries") {
			posts.Add(1)
			if lose.CompareAndSwap(true, false) {
				recorder := httptest.NewRecorder()
				mux.ServeHTTP(recorder, r)
				if recorder.Code != 201 {
					t.Error("controlled response loss did not follow commit", recorder.Code, recorder.Body.String())
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(recorder.Code)
				_, _ = io.WriteString(w, "{")
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
	defer s.Close()
	c := f.newClient(t, f.admin, f.sender, s.URL)
	job, err := c.CreateJob(t.Context(), "map-job", client.CreateJobRequest{Title: "Independent job", Brief: "Original brief", WorkerID: f.sender})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateProjectMapEntry(t.Context(), "original-map", mapEditable())
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || posts.Load() != 1 {
		t.Fatal("lost committed response was retried or reported known", err, posts.Load())
	}
	original, err := c.CreateProjectMapEntry(t.Context(), "original-map", mapEditable())
	if err != nil || original.Revision != 1 {
		t.Fatal(original, err)
	}
	replacement := mapEditable()
	replacement.Phases[0].Name = "Renamed proof"
	replacement.Phases[0].Archived = true
	updated, err := c.ReplaceProjectMapEntry(t.Context(), "map-update", original.ID, client.ReplaceProjectMapEntryRequest{ExpectedRevision: 1, Entry: replacement})
	if err != nil || updated.Revision != 2 {
		t.Fatal(updated, err)
	}
	events := f.eventCount(t)
	noop, err := c.ReplaceProjectMapEntry(t.Context(), "map-noop", original.ID, client.ReplaceProjectMapEntryRequest{ExpectedRevision: 2, Entry: replacement})
	if err != nil || noop.Outcome != "unchanged" || noop.Revision != 2 || f.eventCount(t) != events {
		t.Fatal("no-op added effects", noop, err)
	}
	_, err = c.ReplaceProjectMapEntry(t.Context(), "map-stale", original.ID, client.ReplaceProjectMapEntryRequest{ExpectedRevision: 1, Entry: replacement})
	checkError(t, err, "revision_conflict")
	replay, err := c.CreateProjectMapEntry(t.Context(), "original-map", mapEditable())
	if err != nil || replay != original || f.eventCount(t) != events {
		t.Fatal("exact original key replay changed later state", replay, err)
	}
	old, err := c.ProjectMapRevision(t.Context(), original.ID, 1)
	if err != nil || old.State.Phases[0].Name != "Proof" || old.State.Phases[0].Archived {
		t.Fatal("history lost captured editorial state", old, err)
	}
	current, err := c.ProjectMapEntry(t.Context(), original.ID)
	if err != nil || current.Phases[0].Name != "Renamed proof" || current.Revision != 2 {
		t.Fatal(current, err)
	}
	history, err := c.ProjectMapHistory(t.Context(), original.ID, client.PageOptions{Limit: 1})
	if err != nil || len(history.Items) != 1 || history.Items[0].Revision != 2 || !history.HasMore {
		t.Fatal(history, err)
	}
	next, err := c.ProjectMapHistory(t.Context(), original.ID, client.PageOptions{Limit: 1, Cursor: history.Cursor})
	if err != nil || len(next.Items) != 1 || next.Items[0].Revision != 1 || next.HasMore {
		t.Fatal(next, err)
	}
	list, err := c.ProjectMapEntries(t.Context(), client.PageOptions{}, client.ProjectMapFilter{})
	if err != nil || len(list.Items) != 1 || list.Items[0].ID != original.ID || f.eventCount(t) != events {
		t.Fatal(list, err)
	}
	initial, err := c.JobWorkstream(t.Context(), job.ID)
	if err != nil || initial.Revision != 0 {
		t.Fatal(initial, err)
	}
	_, err = c.ReplaceJobWorkstream(t.Context(), "association-stale-zero", job.ID, client.ReplaceJobWorkstreamRequest{ExpectedRevision: 1, WorkstreamID: original.ID})
	checkError(t, err, "job_workstream_revision_conflict")
	if !errors.As(err, &failure) {
		t.Fatal(err)
	}
	if details, ok := failure.Details.(map[string]any); !ok || details["current_revision"] != int64(0) {
		t.Fatal("initial association conflict lost zero revision", failure.Details)
	}
	selected, err := c.ReplaceJobWorkstream(t.Context(), "association-original", job.ID, client.ReplaceJobWorkstreamRequest{WorkstreamID: original.ID})
	if err != nil || selected.Revision != 1 {
		t.Fatal(selected, err)
	}
	replacement.Title = "Later name"
	replacement.Archived = true
	if _, err = c.ReplaceProjectMapEntry(t.Context(), "map-archive", original.ID, client.ReplaceProjectMapEntryRequest{ExpectedRevision: 2, Entry: replacement}); err != nil {
		t.Fatal(err)
	}
	ref, err := c.JobWorkstream(t.Context(), job.ID)
	if err != nil || ref.WorkstreamTitle != "Build map" || ref.WorkstreamID != original.ID {
		t.Fatal("rename/archive rewrote association meaning", ref, err)
	}
	cleared, err := c.ReplaceJobWorkstream(t.Context(), "association-clear", job.ID, client.ReplaceJobWorkstreamRequest{ExpectedRevision: 1})
	if err != nil || cleared.Revision != 2 {
		t.Fatal(cleared, err)
	}
	if replay, err := c.ReplaceJobWorkstream(t.Context(), "association-original", job.ID, client.ReplaceJobWorkstreamRequest{WorkstreamID: original.ID}); err != nil || replay != selected {
		t.Fatal("association original replay lost", replay, err)
	}
	refs, err := c.JobWorkstreamHistory(t.Context(), job.ID, client.PageOptions{})
	if err != nil || len(refs.Items) != 2 || refs.Items[0].WorkstreamID != "" || refs.Items[1].WorkstreamTitle != "Build map" || refs.Items[1].JobRevision != 1 {
		t.Fatal(refs, err)
	}
	events = f.eventCount(t)
	work, err := c.Job(t.Context(), job.ID)
	if err != nil || work.Revision != 1 || work.Brief != "Original brief" || f.eventCount(t) != events || f.providerCalls.Load() != 1 {
		t.Fatal("map changed job/provider state", work, err)
	}
}

func TestProjectMapEditorialClientBoundedPagesAndStrictResponses(t *testing.T) {
	first := mapEntry()
	first.ID = "entry-a"
	first.Position = 3
	second := first
	second.ID = "entry-b"
	second.Position = 5
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.URL.Path != "/api/v1/projects/project-one/map/editorial" || r.URL.Query().Get("limit") != "5" {
			t.Error("editorial default/path", r.URL.Path, r.URL.RawQuery)
		}
		switch r.URL.Query().Get("cursor") {
		case "":
			write(w, 200, client.Page[client.ProjectMapEntry]{Items: []client.ProjectMapEntry{first}, HasMore: true, Cursor: "second-page"})
		case "second-page":
			write(w, 200, client.Page[client.ProjectMapEntry]{Items: []client.ProjectMapEntry{second}})
		default:
			t.Error("unexpected continuation")
		}
	}))
	defer s.Close()
	c := newClient(t, s)
	page, err := c.ProjectMapEditorial(t.Context(), client.PageOptions{})
	if err != nil || len(page.Items) != 1 || !page.HasMore || calls.Load() != 1 {
		t.Fatal(page, err, calls.Load())
	}
	next, err := c.ProjectMapEditorial(t.Context(), client.PageOptions{Cursor: page.Cursor})
	if err != nil || len(next.Items) != 1 || next.Items[0].Position != 5 || next.HasMore || calls.Load() != 2 {
		t.Fatal(next, err, calls.Load())
	}
	for name, mutate := range map[string]func(*client.Page[client.ProjectMapEntry]){
		"foreign":          func(p *client.Page[client.ProjectMapEntry]) { p.Items[0].ProjectID = "other" },
		"archived":         func(p *client.Page[client.ProjectMapEntry]) { p.Items[0].Archived = true },
		"position":         func(p *client.Page[client.ProjectMapEntry]) { p.Items[0].Position = 1000001 },
		"reverse-position": func(p *client.Page[client.ProjectMapEntry]) { p.Items[0].Position = 6 },
		"reverse-id":       func(p *client.Page[client.ProjectMapEntry]) { p.Items[0].Position = 5; p.Items[0].ID = "entry-z" },
		"duplicate":        func(p *client.Page[client.ProjectMapEntry]) { p.Items[1] = p.Items[0] },
		"null-items":       func(p *client.Page[client.ProjectMapEntry]) { p.Items = nil },
	} {
		t.Run(name, func(t *testing.T) {
			p := client.Page[client.ProjectMapEntry]{Items: []client.ProjectMapEntry{first, second}}
			mutate(&p)
			raw, _ := json.Marshal(p)
			malformed, hits := progressResponseClient(t, string(raw))
			_, err := malformed.ProjectMapEditorial(t.Context(), client.PageOptions{})
			checkError(t, err, "invalid_response")
			if hits.Load() != 1 {
				t.Fatal("unexpected request/retry count", hits.Load())
			}
		})
	}
	local, hits := progressResponseClient(t, `{}`)
	for _, options := range []client.PageOptions{{Limit: -1}, {Limit: 11}, {Cursor: " padded"}} {
		_, err := local.ProjectMapEditorial(t.Context(), options)
		checkError(t, err, "validation_failed")
	}
	if hits.Load() != 0 {
		t.Fatal("invalid editorial input reached HTTP")
	}
}

func TestProjectMapSectionClientActualAPIAndPositionHistory(t *testing.T) {
	f := newMutationFixture(t)
	if err := projectmap.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	projectmap.Register(mux, f.store)
	s := httptest.NewServer(mux)
	defer s.Close()
	c := f.newClient(t, f.admin, f.sender, s.URL)
	content := client.ProjectMapEditable{Kind: "section", Title: "Narrative", Description: "# Heading\n\nLiteral <script>body</script>", Position: 9}
	created, err := c.CreateProjectMapEntry(t.Context(), "section-create", content)
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.ProjectMapEditorial(t.Context(), client.PageOptions{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != created.ID || page.Items[0].Description != content.Description || page.Items[0].Position != 9 {
		t.Fatal(page, err)
	}
	content.Position = 1
	changed, err := c.ReplaceProjectMapEntry(t.Context(), "section-position", created.ID, client.ReplaceProjectMapEntryRequest{ExpectedRevision: 1, Entry: content})
	if err != nil || changed.Revision != 2 {
		t.Fatal(changed, err)
	}
	old, err := c.ProjectMapRevision(t.Context(), created.ID, 1)
	if err != nil || old.State.Position != 9 {
		t.Fatal(old, err)
	}
	current, err := c.ProjectMapEntry(t.Context(), created.ID)
	if err != nil || current.Position != 1 {
		t.Fatal(current, err)
	}
	list, err := c.ProjectMapEntries(t.Context(), client.PageOptions{}, client.ProjectMapFilter{Kind: "section"})
	if err != nil || len(list.Items) != 1 {
		t.Fatal(list, err)
	}
	for _, raw := range []string{`{"kind":"section","title":"x","position":null}`, `{"kind":"section","title":"x","position":-1}`, `{"kind":"section","title":"x","where":"wrong"}`} {
		if _, err := client.DecodeProjectMapEditable([]byte(raw)); err == nil {
			t.Fatal("invalid section accepted")
		}
	}
}
