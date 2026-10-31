package client_test

import (
	"encoding/json"
	"errors"
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func compactStatusFixture() client.ReviewOperationStatus {
	return client.ReviewOperationStatus{ID: executionOperationID, PullNumber: 2, Channel: "hosted", State: "uncertain", Revision: 4, Stage: "capture_blocked", Reason: "hosted_finished_inventory_unproven", AttemptID: "review_attempt_" + strings.Repeat("b", 32), UpdatedAt: "2026-01-01T00:00:00Z", NextAction: "contact_controller_owner"}
}
func TestReviewOperationStatusBoundWaitAndStrictResponse(t *testing.T) {
	for _, mode := range []string{"valid", "ready", "captured", "unknown", "wrong_id", "missing_reason", "null_attempt", "extra_private", "extra_metadata", "known_alias", "duplicate_future", "provider_running", "fake_evidence", "bad_evidence", "wrong_stage", "unsafe_reason", "oversize", "cooldown", "cooldown_unknown", "retry_null", "retry_bad", "retry_nonutc", "retry_foreign_state", "retry_cli"} {
		t.Run(mode, func(t *testing.T) {
			v := compactStatusFixture()
			data, _ := json.Marshal(v)
			var fields map[string]any
			_ = json.Unmarshal(data, &fields)
			if strings.HasPrefix(mode, "retry_") || strings.HasPrefix(mode, "cooldown") {
				fields["state"], fields["stage"], fields["reason"], fields["next_action"] = "held", "held", "hosted_allowance_cooldown", "read_diagnostics"
				if mode != "cooldown_unknown" {
					fields["retry_not_before"] = "2026-01-01T00:10:00Z"
				}
			}
			switch mode {
			case "retry_null":
				fields["retry_not_before"] = nil
			case "retry_bad":
				fields["retry_not_before"] = "PRIVATE_SENTINEL"
			case "retry_nonutc":
				fields["retry_not_before"] = "2026-01-01T01:10:00+01:00"
			case "retry_foreign_state":
				fields["state"], fields["stage"], fields["next_action"] = "active", "provider_wait", "wait"
			case "retry_cli":
				fields["channel"] = "cli"
			case "captured":
				fields["state"] = "active"
				fields["stage"] = "result_captured"
				fields["next_action"] = "wait"
				fields["evidence_id"] = "review_evidence_" + strings.Repeat("c", 32)
			case "ready":
				fields["state"] = "completed"
				fields["stage"] = "evidence_ready"
				fields["next_action"] = "read_evidence"
				fields["evidence_id"] = "review_evidence_" + strings.Repeat("c", 32)
			case "unknown":
				fields["state"] = "completed"
				fields["stage"] = "unknown"
			case "wrong_id":
				fields["id"] = "review_operation_" + strings.Repeat("c", 32)
			case "missing_reason":
				delete(fields, "reason")
			case "null_attempt":
				fields["attempt_id"] = nil
			case "extra_private":
				fields["capability_fingerprint"] = "PRIVATE_SENTINEL"
			case "extra_metadata":
				fields["future_metadata"] = map[string]any{"label": "PRIVATE_SENTINEL", "nested": []any{true, nil, 1}}
			case "known_alias":
				fields["ID"] = executionOperationID

			case "provider_running":
				fields["stage"] = "provider_running"
			case "fake_evidence":
				fields["state"] = "completed"
				fields["stage"] = "evidence_ready"
				fields["next_action"] = "read_evidence"
			case "bad_evidence":
				fields["evidence_id"] = "review_run_" + strings.Repeat("c", 32)
			case "wrong_stage":
				fields["state"] = "queued"
			case "unsafe_reason":
				fields["reason"] = "PRIVATE_SENTINEL\nbody"
			case "oversize":
				fields["reason"] = strings.Repeat("PRIVATE_SENTINEL", 4096)
			}
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/reviews/operations/"+executionOperationID+"/status" || r.URL.RawQuery != "" || r.Header.Get("Prefer") != "wait=900" || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("Idempotency-Key") != "" {
					t.Error("compact read changed binding or wait")
				}
				if mode == "duplicate_future" {
					data, _ := json.Marshal(fields)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(strings.TrimSuffix(string(data), "}") + `,"future_metadata":1,"future_metadata":2}`))
					return
				}
				write(w, 200, fields)
			}))
			defer s.Close()
			c := newClient(t, s)
			got, err := c.WaitReviewOperationStatus(t.Context(), executionOperationID, 900)
			if mode == "valid" || mode == "ready" || mode == "captured" || mode == "unknown" || mode == "cooldown" || mode == "cooldown_unknown" || mode == "extra_private" || mode == "extra_metadata" {
				if err != nil || got.ID != executionOperationID {
					t.Fatal(got, err)
				}
				out, _ := json.Marshal(got)
				if strings.Contains(string(out), "PRIVATE_SENTINEL") || strings.Contains(string(out), "future_metadata") || strings.Contains(string(out), "capability_fingerprint") {
					t.Fatal("unknown metadata escaped typed output")
				}
				if mode == "cooldown" && got.RetryNotBefore != "2026-01-01T00:10:00Z" {
					t.Fatal("original deadline changed", got)
				}
			} else {
				var failure *client.Error
				if !errors.As(err, &failure) || failure.Code != "invalid_response" || strings.Contains(failure.Message, "PRIVATE_SENTINEL") {
					t.Fatal("malformed status accepted or body escaped", got, err)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("read retried")
			}
		})
	}
}
func TestReviewOperationStatusInvalidArgumentsBeforeTransport(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		t.Error("invalid status request reached transport")
	}))
	defer s.Close()
	c := newClient(t, s)
	for _, in := range []struct {
		id      string
		seconds int
	}{{"bad", 0}, {executionOperationID, -1}, {executionOperationID, 901}} {
		if _, err := c.WaitReviewOperationStatus(t.Context(), in.id, in.seconds); err == nil {
			t.Fatal("invalid arguments accepted")
		}
	}
}
