package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
)

func TestResultWaitReconnectKeepsOriginalReadAcrossRestart(t *testing.T) {
	for _, kind := range []string{"operation", "compact", "selection", "ci"} {
		t.Run(kind, func(t *testing.T) {
			var preferences []int
			var bodies []string
			op := sampleReviewOperation()
			op.State = "completed"
			op.Revision = 3
			op.WorkerID = "worker-one"
			op.AttemptID = "review_attempt_" + strings.Repeat("b", 32)
			op.EvidenceID = "review_evidence_" + strings.Repeat("c", 32)
			selection := selectionSample()
			selection.Operation = &op
			selection.Selection.ConfigurationRevision = op.ConfigurationRevision
			ci := ciWatchSample()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				if r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Idempotency-Key") != "" {
					t.Error("reattached wait lost original authority or became mutation")
				}
				n, err := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Prefer"), "wait="))
				if err != nil {
					t.Error(err)
				}
				preferences = append(preferences, n)
				var body any
				if kind == "ci" {
					if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/reviews/ci/watch") {
						t.Error("CI pins retargeted")
					}
					if json.NewDecoder(r.Body).Decode(&body) != nil {
						t.Error("CI body missing")
					}
					raw, _ := json.Marshal(body)
					bodies = append(bodies, string(raw))
				} else if r.Method != "GET" {
					t.Error("operation/selection was resubmitted")
				}
				if len(preferences) == 1 {
					w.Header().Set("X-Request-ID", "request-restart")
					core.WriteError(w, &core.Error{Code: "overloaded", Status: 503, Message: "shutdown", Details: map[string]any{"reason": "result_wait_lifetime"}})
					return
				}
				if len(preferences) == 2 {
					w.WriteHeader(502)
					_, _ = w.Write([]byte("gateway restarting"))
					return
				}
				switch kind {
				case "operation":
					write(w, 200, op)
				case "compact":
					write(w, 200, client.ReviewOperationStatus{ID: op.ID, PullNumber: 2, Channel: "cli", State: "completed", Revision: 3, Stage: "evidence_ready", AttemptID: op.AttemptID, EvidenceID: op.EvidenceID, UpdatedAt: op.UpdatedAt, NextAction: "read_evidence"})
				case "selection":
					write(w, 200, selection)
				case "ci":
					write(w, 200, ci)
				}
			}))
			defer server.Close()
			c := newClient(t, server)
			var err error
			switch kind {
			case "operation":
				var got client.ReviewOperation
				got, err = c.WaitReviewOperation(t.Context(), op.ID, 3)
				if got.ID != op.ID || got.State != "completed" || got.EvidenceID != op.EvidenceID {
					t.Error(got)
				}
			case "compact":
				var got client.ReviewOperationStatus
				got, err = c.WaitReviewOperationStatus(t.Context(), op.ID, 3)
				if got.ID != op.ID || got.State != "completed" || got.EvidenceID != op.EvidenceID {
					t.Error(got)
				}
			case "selection":
				var got client.ReviewRunState
				got, err = c.WaitReviewSelection(t.Context(), runSelectionID, 3)
				if got.Selection.ID != runSelectionID || got.Operation == nil || got.Operation.ID != op.ID {
					t.Error(got)
				}
			case "ci":
				var got client.CIWatchResult
				got, err = c.WatchCI(t.Context(), ciWatchPins(), 3)
				if !reflect.DeepEqual(got, ci) {
					t.Error(got)
				}
			}
			if err != nil || len(preferences) != 3 || preferences[0] != 3 || preferences[1] >= 3 || preferences[2] > preferences[1] {
				t.Fatal("wait restarted its deadline or lost completion", preferences, err)
			}
			for _, body := range bodies {
				if body != bodies[0] {
					t.Fatal("CI original pins changed", bodies)
				}
			}
		})
	}
}

func TestResultWaitReconnectExpiryUsesOneFinalReadAndRetainsIdentity(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(strconv.FormatBool(success), func(t *testing.T) {
			var preferences []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				preferences = append(preferences, r.Header.Get("Prefer"))
				if len(preferences) == 1 {
					w.Header().Set("X-Request-ID", "request-original-shutdown")
					core.WriteError(w, &core.Error{Code: "overloaded", Status: 503, Message: "shutdown", Details: map[string]any{"reason": "result_wait_lifetime"}})
					return
				}
				if success {
					write(w, 200, client.ReviewOperationStatus{ID: executionOperationID, PullNumber: 2, Channel: "hosted", State: "active", Revision: 3, Stage: "provider_wait", AttemptID: "review_attempt_" + strings.Repeat("b", 32), UpdatedAt: "2026-01-01T00:00:00Z", NextAction: "wait"})
					return
				}
				w.WriteHeader(504)
				_, _ = w.Write([]byte("still restarting"))
			}))
			defer server.Close()
			c := newClient(t, server)
			started := time.Now()
			got, err := c.WaitReviewOperationStatus(t.Context(), executionOperationID, 1)
			if time.Since(started) < 900*time.Millisecond || time.Since(started) > 3*time.Second || !reflect.DeepEqual(preferences, []string{"wait=1", "wait=0"}) {
				t.Fatal("retry budget reset or repeated final reads", preferences, time.Since(started))
			}
			if success {
				if err != nil || got.ID != executionOperationID || got.State != "active" {
					t.Fatal("fresh pending read became false completion/error", got, err)
				}
				return
			}
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Code != "overloaded" || failure.RequestID != "request-original-shutdown" || failure.Details.(map[string]any)["operation_id"] != executionOperationID || failure.Details.(map[string]any)["reason"] != "result_wait_lifetime" {
				t.Fatal("exhaustion lost original targeted shutdown", failure)
			}
		})
	}
}

func TestReviewWatchReconnectsWhenGatewayMissesShutdownSignal(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(strconv.FormatBool(disconnect), func(t *testing.T) {
			var reads atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				n := reads.Add(1)
				if r.Method != "GET" || r.URL.Query().Get("after") != "original-event" || r.URL.Query().Get("channel") != "cli" || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Worker") != "worker-one" {
					t.Error("reattached read changed binding/cursor")
				}
				if n == 1 {
					if disconnect {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = connection.Close()
						return
					}
					w.WriteHeader(503)
					_, _ = w.Write([]byte("PRIVATE_RESTART_BODY"))
					return
				}
				if r.Header.Get("Prefer") == "wait=3" {
					t.Error("reconnection reset original wait")
				}
				write(w, 200, client.ReviewWatchResult{ProjectID: "project-one", Changes: []client.ReviewWatchChange{}, EventCursor: "resumed", TimedOut: true})
			}))
			defer service.Close()
			got, err := newClient(t, service).WatchReviews(t.Context(), client.ReviewWatchOptions{Channel: "cli", Limit: 20, After: "original-event"}, 3)
			if err != nil || got.EventCursor != "resumed" || !got.TimedOut || reads.Load() != 2 {
				t.Fatal(got, err, reads.Load())
			}
		})
	}
}
