package client_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewRoundRefusalsExplainExistingWorkWithoutRetryOrAccessAdvice(t *testing.T) {
	cases := map[string][]string{
		"review_round_capacity": {"remaining allocation", "review_operations_list", "pull_number and channel", "reviews operations --pr NUMBER --channel CHANNEL", "review_rounds_read", "Do not submit duplicate reviews"},
		"review_round_closed":   {"explicitly closed", "review_rounds_read", "authorized deliberate grant"},
		"review_round_complete": {"configured discovery stop", "maximum usage from taper", "review_operations_list", "changed commits do not reset"},
		"review_round_changed":  {"different round allocation", "original review operation", "Preserve the original identity"},
	}
	for code, required := range cases {
		t.Run(code, func(t *testing.T) {
			var calls atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				calls.Add(1)
				if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/requests" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("X-FC-Instance") != "instance-one" {
					t.Error("refusal test lost original binding")
				}
				write(w, http.StatusConflict, map[string]any{"request_id": "request-round-refused", "error": map[string]any{"code": code, "message": "PRIVATE_PROVIDER_MESSAGE", "details": map[string]any{"private": "PRIVATE_STATE", "operation_id": "PRIVATE_UNVERIFIED_ID"}}})
			}))
			defer service.Close()
			c := newClient(t, service)
			result, err := c.RequestReview(t.Context(), "planned-hosted-review", client.ReviewRequest{ExpectedConfigurationRevision: 43, PullNumber: 3374, Channel: "hosted", HeadSHA: strings.Repeat("a", 40)}, 0)
			var refusal *client.Error
			if !errors.As(err, &refusal) || refusal.Code != code || refusal.Status != 409 || refusal.RequestID != "request-round-refused" || refusal.OutcomeUnknown || refusal.Details != nil || result.Operation != nil || result.Receipt != nil {
				t.Fatal("known local refusal changed its outcome", result, err)
			}
			text := refusal.Message + " " + refusal.Action
			for _, part := range required {
				if !strings.Contains(text, part) {
					t.Fatal("missing specific recovery", part, text)
				}
			}
			for _, bad := range []string{"PRIVATE_", "Check the selected instance", "current access"} {
				if strings.Contains(text, bad) {
					t.Fatal("refusal leaked data or diagnosed access failure", text)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("refusal retried or performed another operation", calls.Load())
			}
		})
	}
}

func TestReviewRoundRefusalDoesNotReplaceUnknownMutationRecovery(t *testing.T) {
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		write(w, http.StatusServiceUnavailable, map[string]any{"request_id": "request-unknown-round", "error": map[string]any{"code": "review_round_capacity", "message": "PRIVATE_PROVIDER_MESSAGE"}})
	}))
	defer service.Close()
	c := newClient(t, service)
	_, err := c.RequestReview(t.Context(), "original-intent", client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: strings.Repeat("a", 40)}, 0)
	var refusal *client.Error
	if !errors.As(err, &refusal) || !refusal.OutcomeUnknown || refusal.RequestID != "request-unknown-round" || strings.Contains(refusal.Action, "review_operations_list") || !strings.Contains(refusal.Action, "key") || calls.Load() != 1 {
		t.Fatal("specific round guidance replaced unknown-write recovery", err, calls.Load())
	}
}
