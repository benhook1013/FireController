package client_test

import (
	"errors"
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQueueChangeRefusalHasActionableCause(t *testing.T) {
	for _, code := range []string{"review_queue_duplicate", "review_queue_anchor_missing", "review_queue_full", "review_target_retired", "review_not_configured", "reviews_unconfigured"} {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				write(w, 409, map[string]any{"error": map[string]any{"code": code, "message": "PRIVATE provider-injected prose"}, "request_id": "request-one"})
			}))
			defer server.Close()
			c := newClient(t, server)
			_, err := c.ChangeReviewQueue(t.Context(), "add", "one", client.ReviewQueueChange{ExpectedConfigurationRevision: 1, PullNumber: 3})
			var out *client.Error
			if !errors.As(err, &out) || out.Code != code || out.OutcomeUnknown || strings.Contains(out.Message, "PRIVATE") || strings.Contains(out.Action, "Check the selected instance") || out.Message == "The service refused this scoped request." {
				t.Fatal(out, err)
			}
		})
	}
}
