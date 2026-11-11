package client_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewSelectionBusyDetailsAreStrictAndNeverAcceptance(t *testing.T) {
	valid := func(state string) map[string]any {
		return map[string]any{"selection_id": runSelectionID, "channel": "cli", "state": state}
	}
	for _, tc := range []struct {
		name    string
		details any
		keep    bool
	}{
		{"queued", valid("queued"), true}, {"selecting", valid("selecting"), true},
		{"reservation-no-details", nil, false},
		{"partial", map[string]any{"selection_id": runSelectionID, "channel": "cli"}, false},
		{"wrong-id-kind", map[string]any{"selection_id": executionOperationID, "channel": "cli", "state": "queued"}, false},
		{"uppercase-id", map[string]any{"selection_id": strings.ToUpper(runSelectionID), "channel": "cli", "state": "queued"}, false},
		{"numeric-id", map[string]any{"selection_id": 4, "channel": "cli", "state": "queued"}, false},
		{"invalid-channel", map[string]any{"selection_id": runSelectionID, "channel": "other", "state": "queued"}, false},
		{"invalid-state", valid("active"), false}, {"terminal-state", valid("selected"), false},
		{"extra-field", map[string]any{"selection_id": runSelectionID, "channel": "cli", "state": "queued", "actor_id": "untrusted-actor"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/reviews/selections") || r.Header.Get("Idempotency-Key") != "refused-start" {
					t.Error("busy request changed or triggered followup work")
				}
				posts.Add(1)
				failure := map[string]any{"code": "review_busy", "message": "Existing review work."}
				if tc.details != nil {
					failure["details"] = tc.details
				}
				write(w, 409, map[string]any{"error": failure, "request_id": "request-busy"})
			}))
			defer srv.Close()
			c := newClient(t, srv)
			value, err := c.RunReview(t.Context(), "refused-start", selectionRequest(), 0)
			var failure *client.Error
			if value.Receipt != nil || value.Result != nil || !errors.As(err, &failure) || failure.Code != "review_busy" || failure.Status != 409 || failure.OutcomeUnknown || posts.Load() != 1 || !strings.Contains(failure.Message, "not accepted") {
				t.Fatal("busy was interpreted as acceptance or lost recovery", value, err)
			}
			if tc.keep {
				if !reflect.DeepEqual(failure.Details, tc.details) || !strings.Contains(failure.Message, "selection") || !strings.Contains(failure.Action, "--selection-id "+runSelectionID) || !strings.Contains(failure.Action, "review_selection_read") {
					t.Fatal("valid pending selection handle was dropped", failure)
				}
			} else if failure.Details != nil || strings.Contains(failure.Action, runSelectionID) || strings.Contains(failure.Action, "untrusted-actor") || strings.Contains(failure.Message, "selection") || strings.Contains(failure.Action, "review_selection_read") || strings.Contains(failure.Action, "run-read") || !strings.Contains(failure.Action, "reviews operations") {
				t.Fatal("missing or malformed details fabricated a selection blocker", failure)
			}
		})
	}
}
