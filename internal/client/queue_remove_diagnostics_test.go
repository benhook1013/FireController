package client_test

import (
	"errors"
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQueueRemovalPendingDiagnosticRetainsOnlyVerifiedOperation(t *testing.T) {
	for _, id := range []string{executionOperationID, "review_operation_short", "private provider narrative"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			write(w, 409, map[string]any{"request_id": "request-one", "error": map[string]any{"code": "review_queue_operation_pending", "message": "PRIVATE", "details": map[string]any{"operation_id": id, "private": "SECRET"}}})
		}))
		c := newClient(t, server)
		_, err := c.ChangeReviewQueue(t.Context(), "remove", "remove-one", client.ReviewQueueChange{ExpectedConfigurationRevision: 1, PullNumber: 2, Reason: "defunct"})
		var e *client.Error
		if !errors.As(err, &e) || e.Code != "review_queue_operation_pending" || e.OutcomeUnknown {
			t.Fatal(err)
		}
		if id == executionOperationID {
			d, ok := e.Details.(map[string]any)
			if !ok || len(d) != 1 || d["operation_id"] != id {
				t.Fatal(e)
			}
		} else if e.Details != nil {
			t.Fatal("unverified operation escaped", e)
		}
		server.Close()
	}
}
