package client_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/projectmap"
)

func TestProjectMapPartialClientActualAPILostReceiptReplayAndPreservation(t *testing.T) {
	f := newMutationFixture(t)
	if err := projectmap.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	projectmap.Register(mux, f.store)
	var lose atomic.Bool
	lose.Store(true)
	var reads, writes atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/map/entries/") {
			if r.Method == "GET" {
				reads.Add(1)
			}
			if r.Method == "PATCH" {
				writes.Add(1)
				if lose.CompareAndSwap(true, false) {
					recorder := httptest.NewRecorder()
					mux.ServeHTTP(recorder, r)
					if recorder.Code != 200 {
						t.Error("write did not commit", recorder.Body.String())
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(recorder.Code)
					_, _ = io.WriteString(w, "{")
					return
				}
			}
		}
		mux.ServeHTTP(w, r)
	}))
	defer s.Close()
	c := f.newClient(t, f.admin, f.sender, s.URL)
	original, err := c.CreateProjectMapEntry(t.Context(), "create", mapEditable())
	if err != nil {
		t.Fatal(err)
	}
	changes, err := client.DecodeProjectMapChanges([]byte(`{"title":"  Renamed  ","links":[],"description":"","position":0}`))
	if err != nil {
		t.Fatal(err)
	}
	request := client.UpdateProjectMapEntryRequest{ExpectedRevision: 1, Changes: changes}
	_, err = c.UpdateProjectMapEntry(t.Context(), "patch-once", original.ID, request)
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || writes.Load() != 1 || reads.Load() != 0 {
		t.Fatalf("hidden read/retry or lost uncertainty %v writes=%d reads=%d", err, writes.Load(), reads.Load())
	}
	events := f.eventCount(t)
	replay, err := c.UpdateProjectMapEntry(t.Context(), "patch-once", original.ID, request)
	if err != nil || replay.Revision != 2 || replay.Outcome != "updated" || f.eventCount(t) != events || reads.Load() != 0 {
		t.Fatal(replay, err)
	}
	current, err := c.ProjectMapEntry(t.Context(), original.ID)
	if err != nil || current.Title != "Renamed" || current.Description != "" || len(current.Links) != 0 || !reflect.DeepEqual(current.Phases, mapEditable().Phases) || current.Where != mapEditable().Where {
		t.Fatal(current, err)
	}
	_, err = c.UpdateProjectMapEntry(t.Context(), "stale", original.ID, request)
	if !errors.As(err, &failure) || failure.OutcomeUnknown || failure.Code != "revision_conflict" {
		t.Fatal(err)
	}
	request.ExpectedRevision = 2
	unchanged, err := c.UpdateProjectMapEntry(t.Context(), "noop", original.ID, request)
	if err != nil || unchanged.Revision != 2 || unchanged.Outcome != "unchanged" || f.eventCount(t) != events {
		t.Fatal(unchanged, err)
	}
	captured, err := c.ProjectMapRevision(t.Context(), original.ID, 1)
	if err != nil || captured.State.Title != mapEditable().Title || captured.State.Description == "" || len(captured.State.Links) != 1 {
		t.Fatal(captured, err)
	}
}

func TestProjectMapPartialClientStrictLocalInputAndExactBody(t *testing.T) {
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Method != "PATCH" || r.URL.Path != "/api/v1/projects/project-one/map/entries/entry-one" || r.Header.Get("Idempotency-Key") != "update" {
			t.Error("wrong mutation binding", r.Method, r.URL.Path)
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 2 || string(body["expected_revision"]) != "2" || string(body["changes"]) != `{"state":"complete"}` {
			t.Error("unexpected body", body)
		}
		write(w, 200, client.ProjectMapReceipt{ID: "entry-one", Revision: 3, Outcome: "updated"})
	}))
	defer s.Close()
	c := newClient(t, s)
	for _, raw := range []string{`{}`, `null`, `{"kind":"workstream"}`, `{"state":null}`, `{"state":"a","state":"b"}`, `{"State":"x"}`, `{"phases":null}`, `{"links":null}`, `{"phases":[{"id":"first","name":"First","state":null}]}`, `{"title":" "}`, `{"position":-1}`, `{"links":[{"label":"x","url":"javascript:bad"}]}`} {
		if _, err := client.DecodeProjectMapChanges([]byte(raw)); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	for _, rev := range []int64{0, -1} {
		state := "complete"
		_, err := c.UpdateProjectMapEntry(t.Context(), "update", "entry-one", client.UpdateProjectMapEntryRequest{ExpectedRevision: rev, Changes: client.ProjectMapChanges{State: &state}})
		if err == nil {
			t.Fatal("invalid guard admitted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("local invalid input dispatched")
	}
	changes, err := client.DecodeProjectMapChanges([]byte(`{"state":"complete"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.UpdateProjectMapEntry(t.Context(), "update", "entry-one", client.UpdateProjectMapEntryRequest{ExpectedRevision: 2, Changes: changes}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("update expanded into extra calls")
	}
}
