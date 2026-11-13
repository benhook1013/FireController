package client_test

import (
	"encoding/json"
	"errors"
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCombinedWaitRetainsIndependentFactsAndRejectsMalformed(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, kind := range []string{"both", "source-only", "allowance-only", "legacy-none", "bad-source", "unknown-source", "unknown-allowance", "null-allowance", "wrong-channel", "bad-baseline", "unexpected-priority"} {
			t.Run(boolName(compact)+"/"+kind, func(t *testing.T) {
				op := sampleReviewOperation()
				op.State = "queued"
				op.AttemptID = ""
				op.Reason = ""
				op.Channel = "cli"
				allowance := map[string]any{"kind": "cli_exhausted", "observed_at": "2026-01-01T00:00:00Z", "remaining": 0, "limit": 8, "full_capacity_in_ms": 0, "rolling_window_ms": 3600000}
				source := map[string]any{"attempt_id": "review_attempt_" + strings.Repeat("c", 32), "observed_at": "2026-01-01T00:00:00Z", "retry_not_before": "2026-01-01T00:01:00Z", "sequence": 1}
				wait := map[string]any{"paused": false, "baseline_attempt_id": "review_attempt_" + strings.Repeat("b", 32), "retry_not_before": "2026-01-01T00:10:00Z", "cli_allowance": allowance, "source_retry": source}
				valid := true
				switch kind {
				case "source-only":
					delete(wait, "cli_allowance")
					delete(wait, "baseline_attempt_id")
					delete(wait, "retry_not_before")
				case "allowance-only":
					delete(wait, "source_retry")
				case "legacy-none":
					wait = nil
				case "bad-source":
					source["sequence"] = 4
					valid = false
				case "unknown-source":
					source["private"] = "secret"
					valid = false
				case "unknown-allowance":
					allowance["private"] = "secret"
					valid = false
				case "null-allowance":
					wait["cli_allowance"] = nil
					valid = false
				case "wrong-channel":
					op.Channel = "hosted"
					valid = false
				case "bad-baseline":
					wait["baseline_attempt_id"] = ""
					valid = false
				case "unexpected-priority":
					wait["queue_ahead_pull_number"] = 9
					valid = false
				}
				var raw []byte
				if compact {
					raw, _ = json.Marshal(client.ReviewOperationStatus{ID: op.ID, PullNumber: op.PullNumber, Channel: op.Channel, State: op.State, Revision: op.Revision, Stage: "queued", UpdatedAt: op.UpdatedAt, NextAction: "wait"})
				} else {
					raw, _ = json.Marshal(op)
				}
				var wire map[string]any
				_ = json.Unmarshal(raw, &wire)
				if wait != nil {
					wire["wait"] = wait
				}
				var calls atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/instance" {
						handshake(w)
						return
					}
					calls.Add(1)
					if r.Header.Get("X-FC-Operation-Wait") != "6" {
						t.Error("combined capability not requested")
					}
					write(w, 200, wire)
				}))
				defer srv.Close()
				c := newClient(t, srv)
				defer c.Close()
				var got any
				var err error
				if compact {
					got, err = c.ReviewOperationStatus(t.Context(), op.ID)
				} else {
					got, err = c.ReviewOperation(t.Context(), op.ID)
				}
				if !valid {
					checkError(t, err, "invalid_response")
				} else {
					if err != nil {
						t.Fatal(err)
					}
					b, _ := json.Marshal(got)
					var decoded map[string]any
					_ = json.Unmarshal(b, &decoded)
					expectedBytes, _ := json.Marshal(wait)
					var expected any
					_ = json.Unmarshal(expectedBytes, &expected)
					if !reflect.DeepEqual(decoded["wait"], expected) {
						t.Fatal("facts changed", string(b))
					}
				}
				if calls.Load() != 1 {
					t.Fatal("unexpected retry", calls.Load())
				}
			})
		}
	}
}

func TestRetainedOverrideMismatchPreservesUnknownSingleShot(t *testing.T) {
	for _, flag := range []string{"allow", "override"} {
		for _, requested := range []bool{false, true} {
			t.Run(flag+"/"+boolName(requested), func(t *testing.T) {
				op := sampleReviewOperation()
				op.WorkerID = "worker-one"
				op.State = "completed"
				req := client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: op.HeadSHA}
				if flag == "allow" {
					req.AllowOutOfOrder = requested
					op.AllowOutOfOrder = !requested
				} else {
					req.OverrideFileLimit = requested
					op.OverrideFileLimit = !requested
				}
				var posts atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/instance" {
						handshake(w)
						return
					}
					posts.Add(1)
					write(w, 200, op)
				}))
				defer srv.Close()
				c := newClient(t, srv)
				defer c.Close()
				_, err := c.RequestReview(t.Context(), "bound-flags", req, 0)
				checkError(t, err, "invalid_response")
				var failure *client.Error
				if !errors.As(err, &failure) || !failure.OutcomeUnknown || posts.Load() != 1 {
					t.Fatal("mismatch lost unknown single-shot semantics", err, posts.Load())
				}
			})
		}
	}
}
