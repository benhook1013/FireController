package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func allowanceSample(channel string) client.ReviewOperation {
	op := sampleReviewOperation()
	op.WorkerID = "worker-one"
	op.State = "held"
	op.AttemptID = "review_attempt_" + strings.Repeat("b", 32)
	op.ConfigurationRevision = 7
	if channel == "cli" {
		op.Reason = "allowance_exhausted"
		op.Allowance = &client.ReviewAllowanceObservation{Kind: "cli_exhausted", ObservedAt: "2026-01-01T00:00:00Z", Remaining: roundInt(0), Limit: roundInt(3), FullCapacityInMS: roundInt(0), RollingWindowMS: roundInt(86400000)}
	} else {
		op.Channel = "hosted"
		op.Reason = "hosted_allowance_cooldown"
		op.Allowance = &client.ReviewAllowanceObservation{Kind: "hosted_cooldown", ObservedAt: "2026-01-01T00:00:00Z", RetryNotBefore: "2026-01-01T00:10:00Z"}
	}
	return op
}

func TestReviewAllowanceHTTPAllResultSurfaces(t *testing.T) {
	for _, channel := range []string{"cli", "hosted", "legacy"} {
		for _, surface := range []string{"operation", "selection", "request", "start"} {
			t.Run(channel+"/"+surface, func(t *testing.T) {
				op := allowanceSample("cli")
				if channel == "hosted" {
					op = allowanceSample(channel)
				}
				if channel == "legacy" {
					op.Allowance = nil
				}
				raw, _ := json.Marshal(op)
				selection := selectionSample()
				selection.Selection.Channel = op.Channel
				selection.Operation = &op
				var calls atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/instance" {
						handshake(w)
						return
					}
					calls.Add(1)
					if r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("X-FC-Instance") != "instance-one" || r.URL.RawQuery != "" {
						t.Error("result lost bound scope")
					}
					w.Header().Set("Content-Type", "application/json")
					if surface == "selection" || surface == "start" {
						_ = json.NewEncoder(w).Encode(selection)
					} else {
						_, _ = w.Write(raw)
					}
				}))
				defer srv.Close()
				c := newClient(t, srv)
				var got client.ReviewOperation
				var err error
				switch surface {
				case "operation":
					got, err = c.WaitReviewOperation(t.Context(), op.ID, 2)
				case "selection":
					var value client.ReviewRunState
					value, err = c.WaitReviewSelection(t.Context(), runSelectionID, 2)
					if value.Operation != nil {
						got = *value.Operation
					}
				case "request":
					var value client.ReviewRequestResult
					value, err = c.RequestReview(t.Context(), "retained-only", client.ReviewRequest{ExpectedConfigurationRevision: 7, PullNumber: 2, Channel: op.Channel, HeadSHA: op.HeadSHA}, 2)
					if value.Operation != nil {
						got = *value.Operation
					}
				case "start":
					var value client.ReviewRunResult
					value, err = c.RunReview(t.Context(), "retained-selection", client.ReviewRunRequest{Channel: op.Channel, ExpectedPullNumber: roundInt(2)}, 2)
					if value.Result != nil && value.Result.Operation != nil {
						got = *value.Result.Operation
					}
				}
				if err != nil || !reflect.DeepEqual(got, op) || calls.Load() != 1 {
					t.Fatal("retained result changed, polled or rejected", err, calls.Load())
				}
				encoded, _ := json.Marshal(got)
				if string(encoded) != string(raw) {
					t.Fatal("optional allowance changed during output")
				}
				if channel == "cli" && (!strings.Contains(string(encoded), `"remaining":0`) || !strings.Contains(string(encoded), `"full_capacity_in_ms":0`)) {
					t.Fatal("known zero became omitted")
				}
				if channel == "legacy" && strings.Contains(string(encoded), `"allowance"`) {
					t.Fatal("legacy missing observation invented")
				}
			})
		}
	}
}

func TestReviewAllowanceHTTPRejectsMalformedOrUnboundFacts(t *testing.T) {
	type change struct {
		name, channel string
		apply         func(map[string]any, map[string]any)
	}
	cases := []change{
		{"unknown-kind", "cli", func(_, a map[string]any) { a["kind"] = "available" }},
		{"private-field", "cli", func(_, a map[string]any) { a["account"] = "PRIVATE_SENTINEL" }},
		{"mixed-empty-retry", "cli", func(_, a map[string]any) { a["retry_not_before"] = "" }},
		{"wrong-state", "cli", func(o, _ map[string]any) { o["state"] = "active" }},
		{"wrong-reason", "cli", func(o, _ map[string]any) { o["reason"] = "other_hold" }},
		{"wrong-channel", "cli", func(o, _ map[string]any) { o["channel"] = "hosted" }},
		{"missing-attempt", "cli", func(o, _ map[string]any) { o["attempt_id"] = "" }},
		{"foreign-project", "cli", func(o, _ map[string]any) { o["project_id"] = "other-project" }},
		{"null-observation", "cli", func(o, _ map[string]any) { o["allowance"] = nil }},
		{"null-remaining", "cli", func(_, a map[string]any) { a["remaining"] = nil }},
		{"missing-limit", "cli", func(_, a map[string]any) { delete(a, "limit") }},
		{"missing-zero-estimate", "cli", func(_, a map[string]any) { delete(a, "full_capacity_in_ms") }},
		{"missing-window", "cli", func(_, a map[string]any) { delete(a, "rolling_window_ms") }},
		{"positive-remaining", "cli", func(_, a map[string]any) { a["remaining"] = 1 }},
		{"negative-remaining", "cli", func(_, a map[string]any) { a["remaining"] = -1 }},
		{"zero-limit", "cli", func(_, a map[string]any) { a["limit"] = 0 }},
		{"large-limit", "cli", func(_, a map[string]any) { a["limit"] = 10001 }},
		{"negative-estimate", "cli", func(_, a map[string]any) { a["full_capacity_in_ms"] = -1 }},
		{"large-estimate", "cli", func(_, a map[string]any) { a["full_capacity_in_ms"] = 86400001 }},
		{"fractional-estimate", "cli", func(_, a map[string]any) { a["full_capacity_in_ms"] = 0.5 }},
		{"zero-window", "cli", func(_, a map[string]any) { a["rolling_window_ms"] = 0 }},
		{"large-window", "cli", func(_, a map[string]any) { a["rolling_window_ms"] = 86400001 }},
		{"nonutc-observed", "cli", func(_, a map[string]any) { a["observed_at"] = "2026-01-01T00:00:00+00:00" }},
		{"noncanonical-observed", "cli", func(_, a map[string]any) { a["observed_at"] = "2026-01-01T00:00:00.000Z" }},
		{"zero-observed", "cli", func(_, a map[string]any) { a["observed_at"] = "0001-01-01T00:00:00Z" }},
		{"hosted-wrong-reason", "hosted", func(o, _ map[string]any) { o["reason"] = "allowance_exhausted" }},
		{"hosted-cli-field", "hosted", func(_, a map[string]any) { a["remaining"] = 0 }},
		{"hosted-null-cli-field", "hosted", func(_, a map[string]any) { a["limit"] = nil }},
		{"hosted-missing-retry", "hosted", func(_, a map[string]any) { delete(a, "retry_not_before") }},
		{"hosted-invalid-retry", "hosted", func(_, a map[string]any) { a["retry_not_before"] = "PRIVATE_SENTINEL" }},
		{"hosted-nonutc-retry", "hosted", func(_, a map[string]any) { a["retry_not_before"] = "2026-01-01T01:10:00+01:00" }},
	}
	for _, tc := range cases {
		for _, surface := range []string{"operation", "selection", "request", "start"} {
			t.Run(tc.name+"/"+surface, func(t *testing.T) {
				op := allowanceSample(tc.channel)
				raw, _ := json.Marshal(op)
				var object map[string]any
				_ = json.Unmarshal(raw, &object)
				tc.apply(object, object["allowance"].(map[string]any))
				raw, _ = json.Marshal(object)
				if surface == "selection" || surface == "start" {
					selection := selectionSample()
					selection.Selection.Channel = op.Channel
					selection.Operation = nil
					wrapper, _ := json.Marshal(selection)
					var state map[string]any
					_ = json.Unmarshal(wrapper, &state)
					state["operation"] = object
					raw, _ = json.Marshal(state)
				}
				var calls atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/instance" {
						handshake(w)
						return
					}
					calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(raw)
				}))
				defer srv.Close()
				c := newClient(t, srv)
				var err error
				switch surface {
				case "operation":
					_, err = c.ReviewOperation(t.Context(), op.ID)
				case "selection":
					_, err = c.ReviewSelection(t.Context(), runSelectionID)
				case "request":
					_, err = c.RequestReview(t.Context(), "once", client.ReviewRequest{ExpectedConfigurationRevision: 7, PullNumber: 2, Channel: op.Channel, HeadSHA: op.HeadSHA}, 2)
				case "start":
					_, err = c.RunReview(t.Context(), "once", client.ReviewRunRequest{Channel: op.Channel, ExpectedPullNumber: roundInt(2)}, 2)
				}
				var failure *client.Error
				if !errors.As(err, &failure) || failure.Code != "invalid_response" || failure.OutcomeUnknown != (surface == "request" || surface == "start") || calls.Load() != 1 {
					t.Fatal("malformed allowance accepted/retried or lost uncertainty", err, calls.Load())
				}
				if strings.Contains(failure.Error(), "PRIVATE_SENTINEL") {
					t.Fatal("private wire data escaped")
				}
			})
		}
	}
}
