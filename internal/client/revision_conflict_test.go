package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestGuardedRevisionConflictsExplainRecovery(t *testing.T) {
	for _, code := range []string{
		"revision_conflict", "checklist_revision_conflict", "job_workstream_revision_conflict",
		"note_revision_conflict", "primary_job_revision_conflict", "project_presentation_revision_conflict",
		"publication_revision_conflict", "review_attempt_revision_conflict", "review_round_revision_conflict",
		"summary_revision_conflict", "worker_pause_revision_conflict", "idempotency_conflict", "forbidden", "future_revision_conflict",
	} {
		t.Run(code, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				calls.Add(1)
				write(w, 409, map[string]any{"error": map[string]any{"code": code, "message": "private provider prose", "details": map[string]any{"current_revision": 7, "unexpected": "private detail"}}, "request_id": "request-one"})
			}))
			defer server.Close()
			c := newClient(t, server)
			defer c.Close()
			_, err := c.Worker(context.Background())
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Code != code || failure.Status != 409 || failure.OutcomeUnknown || failure.RequestID != "request-one" || calls.Load() != 1 {
				t.Fatalf("refusal classification or no-retry changed: %#v calls=%d", failure, calls.Load())
			}
			details, ok := failure.Details.(map[string]any)
			if !ok || len(details) != 1 || details["current_revision"] != int64(7) || strings.Contains(failure.Message, "private") || strings.Contains(failure.Action, "private") {
				t.Fatal("safe conflict detail lost or provider text exposed")
			}
			guarded := code != "idempotency_conflict" && code != "forbidden" && code != "future_revision_conflict"
			if guarded {
				for _, required := range []string{"Read the current record", "preserve concurrent edits", "current revision", "new idempotency key"} {
					if !strings.Contains(failure.Action, required) {
						t.Fatal("guarded refusal lost useful edit recovery")
					}
				}
				if strings.Contains(failure.Action, "current access") {
					t.Fatal("guarded conflict suggests authentication repair")
				}
			} else if !strings.Contains(failure.Action, "current access") {
				t.Fatal("unrelated or unknown refusal acquired guarded-write recovery")
			}
		})
	}
}
