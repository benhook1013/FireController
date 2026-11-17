package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
)

func TestReviewWatchClientStrictReplayAndNoAutomaticPagination(t *testing.T) {
	var calls atomic.Int64
	base := client.ReviewWatchResult{ProjectID: "project-one", Changes: []client.ReviewWatchChange{{Sequence: 3, EventType: "review.operation_updated", Revision: 1, Operation: operationListClientItem()}}, EventCursor: "signed-next", HasMore: true}
	body, _ := json.Marshal(base)
	for name, raw := range map[string]string{"valid": string(body), "foreign": strings.Replace(string(body), "project-one", "other", 1), "wrong-channel": strings.Replace(string(body), `"channel":"cli"`, `"channel":"hosted"`, 1), "unknown": strings.TrimSuffix(string(body), "}") + `,"private":"PRIVATE_SENTINEL"}`, "future-revision": strings.Replace(string(body), `"revision":1,"operation"`, `"revision":2,"operation"`, 1), "null-changes": strings.Replace(string(body), `"changes":[`+string(mustJSON(t, base.Changes[0]))+`]`, `"changes":null`, 1)} {
		t.Run(name, func(t *testing.T) {
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				calls.Add(1)
				if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/reviews/watch") || r.Header.Get("Prefer") != "wait=0" || r.URL.Query().Get("after") != "signed-original" {
					t.Error("watch binding/wait lost")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(raw))
			}))
			defer service.Close()
			c := newClient(t, service)
			v, err := c.WatchReviews(t.Context(), client.ReviewWatchOptions{Channel: "cli", Limit: 1, After: "signed-original"}, 0)
			if name == "valid" {
				if err != nil || !v.HasMore || len(v.Changes) != 1 {
					t.Fatal(v, err)
				}
			} else {
				checkError(t, err, "invalid_response", "PRIVATE_SENTINEL")
			}
		})
	}
	if calls.Load() != 6 {
		t.Fatal("implicit continuation or retry", calls.Load())
	}
}
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestReviewWatchClientKnownShutdownReconnectAndReleaseMismatch(t *testing.T) {
	var reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.URL.Query().Get("after") != "original-event" || r.URL.Query().Get("channel") != "cli" {
			t.Error("watch rearmed with different filters/cursor")
		}
		if reads.Add(1) == 1 {
			core.WriteError(w, &core.Error{Code: "overloaded", Status: 503, Message: "shutdown", Details: map[string]string{"reason": "result_wait_lifetime"}})
			return
		}
		write(w, 200, client.ReviewWatchResult{ProjectID: "project-one", Changes: []client.ReviewWatchChange{}, EventCursor: "next-event", TimedOut: true})
	}))
	defer service.Close()
	c := newClient(t, service)
	v, err := c.WatchReviews(t.Context(), client.ReviewWatchOptions{Channel: "cli", Limit: 20, After: "original-event"}, 3)
	if err != nil || !v.TimedOut || reads.Load() != 2 {
		t.Fatal(v, err, reads.Load())
	}
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		http.NotFound(w, r)
	}))
	defer missing.Close()
	_, err = newClient(t, missing).WatchReviews(t.Context(), client.ReviewWatchOptions{Limit: 20}, 0)
	var failure *client.Error
	if !errors.As(err, &failure) || !strings.Contains(failure.Action, "matching deployed server") || strings.Contains(failure.Action, "credential") {
		t.Fatal(err)
	}
}

func TestReviewWatchMissingRouteDiagnosticDoesNotMaskStructuredRefusals(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
	}{{"not_found", 404}, {"forbidden", 403}, {"unauthorized", 401}} {
		t.Run(tc.code, func(t *testing.T) {
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				core.WriteError(w, core.Fail(tc.code, "PRIVATE_RAW_MESSAGE", tc.status))
			}))
			defer service.Close()
			_, err := newClient(t, service).WatchReviews(t.Context(), client.ReviewWatchOptions{Limit: 20}, 0)
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Code != tc.code || strings.Contains(failure.Action, "matching deployed") || strings.Contains(failure.Message, "PRIVATE_RAW_MESSAGE") {
				t.Fatal("structured refusal remapped", err)
			}
		})
	}
}

func TestReviewWatchClientRejectsMalformedSnapshotPagination(t *testing.T) {
	item := operationListClientItem()
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		write(w, 200, client.ReviewWatchResult{ProjectID: "project-one", Snapshot: &client.ReviewOperationsPage{ProjectID: "project-one", Items: []client.ReviewOperationSummary{item}, HasMore: true, Cursor: " invalid-page"}, Changes: []client.ReviewWatchChange{}, EventCursor: "event-cursor"})
	}))
	defer service.Close()
	_, err := newClient(t, service).WatchReviews(t.Context(), client.ReviewWatchOptions{Limit: 1}, 0)
	checkError(t, err, "invalid_response")
}
