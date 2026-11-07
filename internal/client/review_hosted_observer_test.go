package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewHostedObserverFullAndCompactStrictWire(t *testing.T) {
	type testCase struct {
		name, observer, channel, state string
		noAttempt, valid               bool
	}
	cases := []testCase{
		{name: "legacy absent", valid: true},
		{name: "unknown", observer: `{"state":"unknown"}`, valid: true},
		{name: "checking", observer: `{"state":"checking","last_check_at":"2026-01-01T00:00:00Z","last_source_at":"2026-01-01T00:00:01Z","reason":"publication_not_proven"}`, valid: true},
		{name: "waiting", observer: `{"state":"waiting","last_check_at":"2026-01-01T00:00:00.123456789Z","last_source_at":"2026-01-01T00:00:00Z","next_check_at":"2026-01-01T00:00:15Z"}`, valid: true},
		{name: "retrying", observer: `{"state":"retrying","error":"provider_unavailable","reason":"attribution_window_closed","next_check_at":"2026-01-01T00:00:15Z"}`, valid: true},
		{name: "stopped", observer: `{"state":"stopped","error":"observer_unavailable","last_check_at":"2026-01-01T00:00:00Z"}`, valid: true},
		{name: "stopped without error", observer: `{"state":"stopped"}`, valid: true},
		{name: "posting original", observer: `{"state":"checking"}`, state: "posting", valid: true},
		{name: "active original", observer: `{"state":"waiting"}`, state: "active", valid: true},
		{name: "null observer", observer: `null`},
		{name: "missing state", observer: `{}`},
		{name: "unknown state", observer: `{"state":"running"}`},
		{name: "null state", observer: `{"state":null}`},
		{name: "state alias", observer: `{"State":"waiting"}`},
		{name: "duplicate state", observer: `{"state":"waiting","state":"checking"}`},
		{name: "private nested field", observer: `{"state":"waiting","raw_body":"PRIVATE_SENTINEL"}`},
		{name: "private nested object", observer: `{"state":"waiting","capture":{"path":"PRIVATE_SENTINEL"}}`},
		{name: "malformed time", observer: `{"state":"checking","last_check_at":"PRIVATE_SENTINEL"}`},
		{name: "offset time", observer: `{"state":"checking","last_check_at":"2026-01-01T01:00:00+01:00"}`},
		{name: "noncanonical time", observer: `{"state":"checking","last_check_at":"2026-01-01T00:00:00.000Z"}`},
		{name: "zero time", observer: `{"state":"checking","last_check_at":"0001-01-01T00:00:00Z"}`},
		{name: "bad source time", observer: `{"state":"checking","last_check_at":"2026-01-01T00:00:00Z","last_source_at":"not-time"}`},
		{name: "bad next time", observer: `{"state":"checking","next_check_at":"not-time"}`},
		{name: "source without check", observer: `{"state":"checking","last_source_at":"2026-01-01T00:00:00Z"}`},
		{name: "private reason", observer: `{"state":"stopped","reason":"/private/PRIVATE_SENTINEL"}`},
		{name: "private error", observer: `{"state":"retrying","error":"PRIVATE_SENTINEL\nraw response"}`},
		{name: "credential error", observer: `{"state":"retrying","error":"` + testToken + `"}`},
		{name: "oversized reason", observer: `{"state":"stopped","reason":"` + strings.Repeat("x", 129) + `"}`},
		{name: "oversized error", observer: `{"state":"retrying","error":"` + strings.Repeat("x", 129) + `"}`},
		{name: "retrying without error", observer: `{"state":"retrying"}`},
		{name: "waiting with error", observer: `{"state":"waiting","error":"provider_unavailable"}`},
		{name: "CLI original", observer: `{"state":"checking"}`, channel: "cli"},
		{name: "completed original", observer: `{"state":"stopped"}`, state: "completed"},
		{name: "no original attempt", observer: `{"state":"unknown"}`, noAttempt: true},
	}
	for _, field := range []string{"last_check_at", "last_source_at", "next_check_at", "reason", "error"} {
		cases = append(cases, testCase{name: "null " + field, observer: `{"state":"checking","` + field + `":null}`})
		value := "2026-01-01T00:00:00Z"
		if field == "reason" || field == "error" {
			value = "unknown_fact"
		}
		cases = append(cases, testCase{name: "unknown with " + field, observer: `{"state":"unknown","` + field + `":"` + value + `"}`})
	}
	for _, compact := range []bool{false, true} {
		path := "/api/v1/projects/project-one/reviews/operations/" + executionOperationID
		name := "full"
		if compact {
			path += "/status"
			name = "compact"
		}
		t.Run(name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					operation := sampleReviewOperation()
					operation.Channel, operation.State, operation.Reason = "hosted", "uncertain", "observation_pending"
					operation.AttemptID = "review_attempt_" + strings.Repeat("b", 32)
					if tc.channel != "" {
						operation.Channel = tc.channel
					}
					if tc.state != "" {
						operation.State = tc.state
					}
					if tc.noAttempt {
						operation.AttemptID = ""
					}
					var base any = operation
					if compact {
						v := compactStatusFixture()
						v.Channel, v.State, v.Reason, v.AttemptID = operation.Channel, operation.State, operation.Reason, operation.AttemptID
						v.Stage, v.NextAction = "uncertain", "contact_controller_owner"
						switch v.State {
						case "posting", "active":
							v.Stage, v.NextAction = "provider_wait", "wait"
						case "completed":
							v.Stage = "unknown"
						}
						base = v
					}
					data, _ := json.Marshal(base)
					var fields map[string]json.RawMessage
					_ = json.Unmarshal(data, &fields)
					if tc.observer != "" {
						fields["observer"] = json.RawMessage(tc.observer)
					}
					data, err := json.Marshal(fields)
					// json.Marshal checks syntax but intentionally preserves duplicate
					// keys inside RawMessage for the real wire decoder to refuse.
					if err != nil {
						t.Fatal(err)
					}
					var reads atomic.Int64
					s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/api/v1/instance" {
							if r.Header.Get("X-FC-Hosted-Observer") != "" {
								t.Error("observer negotiation leaked to identity read")
							}
							handshake(w)
							return
						}
						reads.Add(1)
						if r.Method != http.MethodGet || r.URL.Path != path || r.URL.RawQuery != "" || r.Header.Get("X-FC-Hosted-Observer") != "1" || r.Header.Get("Prefer") != "wait=2" || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("Idempotency-Key") != "" {
							t.Error("observer negotiation changed read routing or binding")
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(data)
					}))
					defer s.Close()
					c := newClient(t, s)
					var got *client.ReviewHostedObserverStatus
					var output []byte
					if compact {
						v, e := c.WaitReviewOperationStatus(t.Context(), executionOperationID, 2)
						got, err = v.Observer, e
						output, _ = json.Marshal(v)
					} else {
						v, e := c.WaitReviewOperation(t.Context(), executionOperationID, 2)
						got, err = v.Observer, e
						output, _ = json.Marshal(v)
					}
					if reads.Load() != 1 {
						t.Fatal("observer read retried")
					}
					if tc.valid {
						if err != nil {
							t.Fatal("valid original observer refused", err)
						}
						var want *client.ReviewHostedObserverStatus
						if tc.observer != "" {
							_ = json.Unmarshal([]byte(tc.observer), &want)
						}
						if !reflect.DeepEqual(got, want) {
							t.Fatal("observer metadata changed", got, want)
						}
					} else {
						checkError(t, err, "invalid_response", "PRIVATE_SENTINEL")
						if got != nil || strings.Contains(string(output), "PRIVATE_SENTINEL") {
							t.Fatal("refused observer escaped typed result")
						}
					}
				})
			}
		})
	}
}

func TestReviewHostedObserverHeaderDoesNotAuthorizeOtherRequests(t *testing.T) {
	var reads, posts atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-FC-Hosted-Observer") != "" {
			t.Error("observer negotiation leaked to another route or mutation")
		}
		switch r.URL.Path {
		case "/api/v1/instance":
			handshake(w)
		case "/api/v1/projects/project-one/reviews/execution/configuration":
			reads.Add(1)
			write(w, http.StatusOK, sampleExecutionConfiguration())
		case "/api/v1/projects/project-one/reviews/requests":
			posts.Add(1)
			write(w, http.StatusAccepted, client.ReviewRequestReceipt{ID: executionOperationID, State: "queued", Revision: 1})
		default:
			t.Error("unexpected request")
		}
	}))
	defer s.Close()
	c := newClient(t, s)
	if _, err := c.ReviewExecution(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RequestReview(t.Context(), "explicit-request-key", operationWaitRequest(), 0); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 1 || posts.Load() != 1 {
		t.Fatal("ordinary requests changed or retried")
	}
}
