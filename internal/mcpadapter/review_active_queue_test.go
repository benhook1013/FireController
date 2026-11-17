package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewActiveQueueMCPCompactViewAndContinuation(t *testing.T) {
	f := mutationFixture(t)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	reviews.Register(mux, f.store)
	zero := int64(0)
	observation := reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 1, Source: "active fixture", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	for n := int64(1); n <= 7; n++ {
		state := "open"
		if n <= 2 {
			state = "merged"
		}
		observation.Queue = append(observation.Queue, reviews.PullInput{Number: n, Title: fmt.Sprintf("Pull %d", n), State: state, CIState: "unknown", Mergeability: "unknown"})
	}
	body, _ := json.Marshal(observation)
	request := httptest.NewRequest("POST", "/api/v1/projects/"+f.project+"/reviews/observations", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+f.admin)
	request.Header.Set("Idempotency-Key", "active-mcp-seed")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != 201 {
		t.Fatal(recorder.Code, recorder.Body.String())
	}
	var activeReads, writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/reviews/queue/active") {
			activeReads.Add(1)
		} else if strings.Contains(r.URL.Path, "/reviews/") && !strings.HasSuffix(r.URL.Path, "/reviews/queue") {
			t.Errorf("unexpected queue fanout: %s", r.URL.Path)
		}
		mux.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := connect(t, f.adapter(t, service.URL, f.sender), "")
	var first, page, tail client.ActiveQueueResponse
	decodeResult(t, call(t, session, "review_queue", map[string]any{"active": true}), &first)
	if len(first.Items) != 5 || first.Items[0].Number != 3 || first.Items[0].Position != 2 || first.HasMore {
		t.Fatal(first)
	}
	decodeResult(t, call(t, session, "review_queue", map[string]any{"active": true, "limit": 2}), &page)
	if len(page.Items) != 2 || !page.HasMore {
		t.Fatal(page)
	}
	decodeResult(t, call(t, session, "review_queue", map[string]any{"active": true, "cursor": page.Cursor}), &tail)
	if tail.SnapshotID != page.SnapshotID || len(tail.Items) != 3 || tail.Items[0].Number != 5 || tail.Items[2].Number != 7 || tail.HasMore {
		t.Fatal(tail)
	}
	var full client.ReviewQueue
	decodeResult(t, call(t, session, "review_queue", map[string]any{"active": false}), &full)
	if full.Items[0].State != "merged" || full.Items[0].Number != 1 {
		t.Fatal("explicit false changed full queue", full)
	}
	if activeReads.Load() != 3 || writes.Load() != 0 || f.providers.Load() != 1 {
		t.Fatal("active queue performed extra work", activeReads.Load(), writes.Load())
	}
}

func TestReviewActiveQueueMCPRejectsTypesBeforeCredential(t *testing.T) {
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer service.Close()
	binding := localContext()
	binding.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"alpha": binding}, "missing-active-queue-credential"), "alpha"), "")
	for _, raw := range []string{`{"active":null}`, `{"active":1}`, `{"active":"true"}`, `{"active":true,"active":false}`, `{"active":true,"limit":null}`, `{"active":true,"limit":0}`, `{"active":true,"limit":101}`, `{"active":true,"limit":1.5}`, `{"active":true,"cursor":""}`, `{"active":true,"cursor":null}`, `{"active":true,"snapshot":"unknown"}`} {
		errorResult(t, call(t, session, "review_queue", json.RawMessage(raw)), "invalid_arguments")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid selection contacted service")
	}
}
