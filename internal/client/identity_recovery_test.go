package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func TestIdentityPreflightRecoversOneTransientReadWithoutDispatchingWork(t *testing.T) {
	for _, status := range []int{502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/instance" || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Instance") != "instance-one" {
					t.Error("preflight changed binding or dispatched another operation")
				}
				if calls.Add(1) == 1 {
					http.Error(w, "synthetic gateway failure", status)
					return
				}
				handshake(w)
			}))
			defer s.Close()
			c, err := client.New(t.Context(), binding(t, s.URL))
			if err != nil || calls.Load() != 2 {
				t.Fatal("bounded identity recovery", err, calls.Load())
			}
			c.Close()
		})
	}
}

func TestIdentityPreflightDoesNotRetryPermanentFailures(t *testing.T) {
	for _, status := range []int{401, 403, 404, 409, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, "synthetic failure", status)
			}))
			defer s.Close()
			c, err := client.New(t.Context(), binding(t, s.URL))
			want := int64(1)
			if err == nil || c != nil || calls.Load() != want {
				t.Fatal("unexpected retry or accepted failure", err, calls.Load())
			}
		})
	}
}

func TestIdentityPreflightRecoveryRespectsCancellationAndInstancePin(t *testing.T) {
	for _, mode := range []string{"cancel", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					if mode == "cancel" {
						cancel()
					}
					http.Error(w, "synthetic gateway failure", 503)
					return
				}
				write(w, 200, map[string]string{"instance_id": "other-instance"})
			}))
			defer s.Close()
			start := time.Now()
			c, err := client.New(ctx, binding(t, s.URL))
			code, count := "canceled", int64(1)
			if mode == "replacement" {
				code, count = "instance_mismatch", 2
			}
			checkError(t, err, code)
			if c != nil || calls.Load() != count || time.Since(start) > time.Second {
				t.Fatal("cancellation/binding recovery boundary", calls.Load())
			}
		})
	}
}

func TestIdentityPreflightRecoversSeveralReadsThenSendsOriginalWriteOnce(t *testing.T) {
	var identities, mutations atomic.Int64
	title := "Retain the exact selected update"
	request := client.UpdateJobRequest{ExpectedRevision: 7, Title: &title}
	wantBody, _ := json.Marshal(request)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Instance") != "instance-one" {
			t.Error("retry changed credential or instance pin")
		}
		if r.URL.Path == "/api/v1/instance" {
			if r.Method != "GET" {
				t.Error("identity retry became a mutation")
			}
			if identities.Add(1) <= 3 {
				http.Error(w, "synthetic recreate gap", 503)
				return
			}
			handshake(w)
			return
		}
		mutations.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || r.Method != "PATCH" || r.URL.Path != "/api/v1/projects/project-one/jobs/job-one" || r.Header.Get("Idempotency-Key") != "original-update-key" || r.Header.Get("X-FC-Worker") != "worker-one" || !reflect.DeepEqual(body, wantBody) {
			t.Error("selected mutation changed original request/key", string(body), err)
		}
		write(w, 200, client.JobReceipt{ID: "job-one", Outcome: "updated", Revision: 8})
	}))
	defer s.Close()
	c, err := client.New(t.Context(), binding(t, s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if identities.Load() != 4 || mutations.Load() != 0 {
		t.Fatal("bootstrap dispatched selected work", identities.Load(), mutations.Load())
	}
	receipt, err := c.UpdateJob(t.Context(), "original-update-key", "job-one", request)
	if err != nil || receipt.Revision != 8 || mutations.Load() != 1 || identities.Load() != 4 {
		t.Fatal(receipt, err, mutations.Load(), identities.Load())
	}
}

func TestIdentityPreflightTemporaryFailureDoesNotBlameBindingOrSendOperation(t *testing.T) {
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/v1/instance" {
			t.Error("failed bootstrap sent selected operation")
		}
		http.Error(w, "synthetic unavailable", 503)
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	c, err := client.New(ctx, binding(t, s.URL))
	var failure *client.Error
	if c != nil || !errors.As(err, &failure) || failure.Code != "deadline_exceeded" || failure.OutcomeUnknown || calls.Load() != 1 {
		t.Fatal(err, calls.Load())
	}
	for _, phrase := range []string{"temporarily unavailable", "did not send the selected operation"} {
		if !strings.Contains(failure.Message, phrase) {
			t.Fatal(failure.Message)
		}
	}
	for _, phrase := range []string{"earlier uncertain write", "exact same request body, idempotency key and binding", "does not establish its outcome"} {
		if !strings.Contains(failure.Action, phrase) {
			t.Fatal(failure.Action)
		}
	}
	for _, word := range []string{"maintainer", "profile change", "credential change"} {
		if strings.Contains(failure.Action, word) {
			t.Fatal("transient failure required unrelated repair", failure.Action)
		}
	}
}

func TestIdentityPreflightDoesNotRepeatAnUnknownPost(t *testing.T) {
	var posts atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		posts.Add(1)
		if r.Method != "POST" || r.Header.Get("Idempotency-Key") != "original-message-key" {
			t.Error("unknown write changed method/key")
		}
		http.Error(w, "synthetic lost response after effect", 503)
	}))
	defer s.Close()
	c, err := client.New(t.Context(), binding(t, s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.SendMessage(t.Context(), "original-message-key", client.SendMessageRequest{RecipientWorkerID: "worker-two", Body: "One original message"})
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || posts.Load() != 1 || !strings.Contains(failure.Action, "exact same idempotency key") {
		t.Fatal("unknown POST repeated or lost original-key recovery", err, posts.Load())
	}
}
