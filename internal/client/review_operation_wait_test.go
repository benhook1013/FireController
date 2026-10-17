package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func operationWaitRequest() client.ReviewRequest {
	return client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}
}

func TestReviewOperationWaitRequestReturnsFlatTerminalOrOriginalReceipt(t *testing.T) {
	for _, state := range []string{"completed", "failed", "cancelled", "rate_limited", "uncertain", "held", "queued"} {
		t.Run(state, func(t *testing.T) {
			var posts atomic.Int64
			operation := sampleReviewOperation()
			operation.WorkerID = "worker-one"
			operation.State, operation.Revision = state, 7
			receipt := client.ReviewRequestReceipt{ID: executionOperationID, State: "queued", Revision: 1}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				posts.Add(1)
				if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/requests" || r.URL.RawQuery != "" || r.Header.Get("Prefer") != "wait=900" || r.Header.Get("Idempotency-Key") != "same-durable-key" {
					t.Error("wait changed request routing or action identity")
				}
				var body client.ReviewRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body != operationWaitRequest() {
					t.Error("wait changed action body", err)
				}
				w.Header().Set("Content-Type", "application/json")
				if state == "queued" {
					w.WriteHeader(202)
					_ = json.NewEncoder(w).Encode(receipt)
				} else {
					_ = json.NewEncoder(w).Encode(operation)
				}
			}))
			defer srv.Close()
			c := newClient(t, srv)
			defer c.Close()
			got, err := c.RequestReview(t.Context(), "same-durable-key", operationWaitRequest(), 900)
			if err != nil || posts.Load() != 1 {
				t.Fatal("request did not return once", got, err)
			}
			var want any = operation
			if state == "queued" {
				want = receipt
				if got.Receipt == nil || *got.Receipt != receipt || got.Operation != nil {
					t.Fatal("fallback changed original receipt", got)
				}
			} else if got.Operation == nil || *got.Operation != operation || got.Receipt != nil {
				t.Fatal("terminal result lost facts", got)
			}
			actualJSON, err := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if err != nil || string(actualJSON) != string(wantJSON) {
				t.Fatal("union did not remain flat", string(actualJSON), err)
			}
		})
	}
	for _, invalid := range []client.ReviewRequestResult{{}, {Receipt: &client.ReviewRequestReceipt{}, Operation: &client.ReviewOperation{}}} {
		if _, err := json.Marshal(invalid); err == nil {
			t.Fatal("invalid union serialized")
		}
	}
}

func TestReviewOperationWaitRequestRejectsWrongTerminalFacts(t *testing.T) {
	for name, mutate := range map[string]func(*client.ReviewOperation){
		"queued":              func(v *client.ReviewOperation) { v.State = "queued" },
		"preparing":           func(v *client.ReviewOperation) { v.State = "preparing" },
		"posting":             func(v *client.ReviewOperation) { v.State = "posting" },
		"active":              func(v *client.ReviewOperation) { v.State = "active" },
		"foreign-project":     func(v *client.ReviewOperation) { v.ProjectID = "foreign-project" },
		"wrong-worker":        func(v *client.ReviewOperation) { v.WorkerID = "other-worker" },
		"wrong-target":        func(v *client.ReviewOperation) { v.PullNumber++ },
		"wrong-head":          func(v *client.ReviewOperation) { v.HeadSHA = strings.Repeat("b", 40) },
		"wrong-channel":       func(v *client.ReviewOperation) { v.Channel = "hosted" },
		"wrong-configuration": func(v *client.ReviewOperation) { v.ConfigurationRevision++ },
		"bad-operation":       func(v *client.ReviewOperation) { v.ID = "operation" },
	} {
		t.Run(name, func(t *testing.T) {
			operation := sampleReviewOperation()
			operation.WorkerID = "worker-one"
			operation.State = "completed"
			mutate(&operation)
			var posts atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				posts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(operation)
			}))
			defer srv.Close()
			c := newClient(t, srv)
			defer c.Close()
			_, err := c.RequestReview(t.Context(), "same-key", operationWaitRequest(), 2)
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Code != "invalid_response" || !failure.OutcomeUnknown || posts.Load() != 1 {
				t.Fatal("invalid accepted facts lost uncertainty or retried", err)
			}
		})
	}
}

func TestReviewOperationWaitReadRemainsPureAndBounded(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		index := calls.Add(1)
		want := "wait=900"
		if index == 1 {
			want = "wait=0"
		}
		if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/reviews/operations/"+executionOperationID || r.URL.RawQuery != "" || r.Header.Get("Prefer") != want || r.Header.Get("Idempotency-Key") != "" {
			t.Error("waiting read changed action or routing")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sampleReviewOperation())
	}))
	defer srv.Close()
	c := newClient(t, srv)
	defer c.Close()
	if got, err := c.ReviewOperation(t.Context(), executionOperationID); err != nil || got != sampleReviewOperation() {
		t.Fatal("immediate read changed", got, err)
	}
	if got, err := c.WaitReviewOperation(t.Context(), executionOperationID, 900); err != nil || got != sampleReviewOperation() {
		t.Fatal("wait expiration must return pending facts", got, err)
	}
	for _, seconds := range []int{-1, 901} {
		if client.ValidateReviewOperationWait(seconds) == nil {
			t.Fatal("unbounded wait admitted")
		}
		var unloaded *client.Client
		_, err := unloaded.RequestReview(t.Context(), "key", operationWaitRequest(), seconds)
		checkError(t, err, "validation_failed")
		_, err = unloaded.WaitReviewOperation(t.Context(), executionOperationID, seconds)
		checkError(t, err, "validation_failed")
	}
	if calls.Load() != 2 || client.ValidateReviewOperationWait(0) != nil || client.ValidateReviewOperationWait(900) != nil || client.ValidateReviewSourceWait(61) == nil {
		t.Fatal("review wait widened source limits or added I/O")
	}
	if client.ReviewOperationBudget(0) != 5*time.Second || client.ReviewOperationBudget(2) != 5*time.Second || client.ReviewOperationBudget(900) != 905*time.Second || client.SourceRefreshBudget(60) != 65*time.Second {
		t.Fatal("request budgets changed")
	}
}

func TestReviewOperationWaitCancellationDetachesSingleRequest(t *testing.T) {
	for _, mutation := range []bool{false, true} {
		for _, received := range []bool{false, true} {
			t.Run(map[bool]string{false: "read", true: "request"}[mutation]+map[bool]string{false: "-unsent", true: "-received"}[received], func(t *testing.T) {
				started, release := make(chan struct{}, 1), make(chan struct{})
				var calls atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/instance" {
						handshake(w)
						return
					}
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					started <- struct{}{}
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}))
				defer srv.Close()
				defer close(release)
				c := newClient(t, srv)
				defer c.Close()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if !received {
					cancel()
				}
				done := make(chan error, 1)
				go func() {
					if mutation {
						_, err := c.RequestReview(ctx, "same-key", operationWaitRequest(), 900)
						done <- err
					} else {
						_, err := c.WaitReviewOperation(ctx, executionOperationID, 900)
						done <- err
					}
				}()
				if received {
					select {
					case <-started:
					case <-time.After(2 * time.Second):
						t.Fatal("request did not arrive")
					}
					cancel()
				}
				select {
				case err := <-done:
					var failure *client.Error
					if !errors.As(err, &failure) || failure.Code != "canceled" || failure.OutcomeUnknown != (mutation && received) {
						t.Fatal("cancellation lost known-unsent/read/post-accept distinction", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("wait ignored caller cancellation")
				}
				want := int64(0)
				if received {
					want = 1
				}
				if calls.Load() != want {
					t.Fatal("wait retried or sent a canceled request")
				}
			})
		}
	}
}

func TestReviewOperationWaitPostAcceptFailureIsUnknownOnlyForMutation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"forbidden","message":"access revoked","details":{"outcome_unknown":true}},"request_id":"wait-request"}`)
	}))
	defer srv.Close()
	c := newClient(t, srv)
	defer c.Close()
	_, requestErr := c.RequestReview(t.Context(), "same-key", operationWaitRequest(), 2)
	_, readErr := c.WaitReviewOperation(t.Context(), executionOperationID, 2)
	for mutation, err := range map[bool]error{true: requestErr, false: readErr} {
		var failure *client.Error
		if !errors.As(err, &failure) || failure.Code != "forbidden" || failure.OutcomeUnknown != mutation {
			t.Fatal("post-accept authorization uncertainty leaked into read or disappeared", err)
		}
	}
}

func TestReviewOperationWaitOutlivesOrdinaryTransportWithoutChangingOtherCalls(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.Method == "POST" {
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			operation := sampleReviewOperation()
			operation.WorkerID = "worker-one"
			operation.State = "completed"
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(operation)
			return
		}
		if r.Header.Get("Prefer") != "wait=0" {
			t.Error("long wait mutated immediate request preference")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sampleReviewOperation())
	}))
	defer srv.Close()
	c := newClient(t, srv)
	defer c.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.RequestReview(ctx, "same-key", operationWaitRequest(), 10); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("wait never reached handler")
	}
	if _, err := c.ReviewOperation(ctx, executionOperationID); err != nil {
		t.Fatal("concurrent immediate read blocked", err)
	}
	// A real response after the ordinary five-second header/client timeout
	// proves both owned wait timers were extended, without inspecting internals.
	select {
	case err := <-done:
		t.Fatal("long wait ended under ordinary budget", err)
	case <-time.After(5100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("extended wait failed", err)
		}
	case <-ctx.Done():
		t.Fatal("wait never collected terminal result")
	}
}
