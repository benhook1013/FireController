package client

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestMissingSourceAssessmentDiagnosticRequiresExactConflictAndHidesServerText(t *testing.T) {
	c := &Client{token: "private-token-sentinel", credentialFile: "/private/credential"}
	for _, tc := range []struct {
		code     string
		status   int
		specific bool
	}{
		{"reviews_source_unavailable", http.StatusConflict, true},
		{"reviews_source_unavailable", http.StatusUnauthorized, false},
		{"reviews_source_unavailable", http.StatusForbidden, false},
		{"reviews_source_unavailable", http.StatusNotFound, false},
		{"reviews_source_unavailable", http.StatusInternalServerError, false},
		{"reviews_source_unavailable_other", http.StatusConflict, false},
	} {
		t.Run(tc.code+http.StatusText(tc.status), func(t *testing.T) {
			data, _ := json.Marshal(map[string]any{"error": map[string]any{"code": tc.code, "message": "PRIVATE_ARBITRARY_SERVER_MESSAGE", "details": map[string]any{"private": "PRIVATE_SERVER_DETAIL"}}, "request_id": "request-source-missing"})
			e, ok := c.serviceError(data, tc.status)
			if !ok || e.Code != tc.code || e.Status != tc.status || e.RequestID != "request-source-missing" || e.OutcomeUnknown || e.Details != nil || strings.Contains(e.Message+e.Action, "PRIVATE") {
				t.Fatal("source diagnostic changed refusal facts or leaked server text", e, ok)
			}
			if tc.specific {
				for _, phrase := range []string{"already-pending original refresh", "review_source_refresh", "review_source_refresh_read", "Reassess after the refresh completes", "does not refresh GitHub or start a review"} {
					if !strings.Contains(e.Action, phrase) {
						t.Fatal("missing supported source-recovery step", e)
					}
				}
				if !strings.Contains(e.Message, "No completed stored source observation") || strings.Contains(e.Action, "Check the selected instance") {
					t.Fatal("source absence still masquerades as access failure", e)
				}
			} else if e.Message != "The service refused this scoped request." || e.Action != "Check the selected instance, project, worker and current access." {
				t.Fatal("source recovery advice weakened generic status safeguards", e)
			}
		})
	}
}

func TestMissingCIAssessmentDiagnosticRequiresExactConflictAndHidesServerText(t *testing.T) {
	c := &Client{token: "private-token-sentinel", credentialFile: "/private/credential"}
	for _, tc := range []struct {
		code     string
		status   int
		specific bool
	}{
		{"reviews_ci_unavailable", http.StatusConflict, true},
		{"reviews_ci_unavailable", http.StatusUnauthorized, false},
		{"reviews_ci_unavailable", http.StatusForbidden, false},
		{"reviews_ci_unavailable", http.StatusNotFound, false},
		{"reviews_ci_unavailable", http.StatusInternalServerError, false},
		{"reviews_ci_unavailable_other", http.StatusConflict, false},
	} {
		t.Run(tc.code+http.StatusText(tc.status), func(t *testing.T) {
			data, _ := json.Marshal(map[string]any{"error": map[string]any{"code": tc.code, "message": "PRIVATE_ARBITRARY_SERVER_MESSAGE", "details": map[string]any{"private": "PRIVATE_SERVER_DETAIL"}}, "request_id": "request-ci-missing"})
			e, ok := c.serviceError(data, tc.status)
			if !ok || e.Code != tc.code || e.Status != tc.status || e.RequestID != "request-ci-missing" || e.OutcomeUnknown || e.Details != nil || strings.Contains(e.Message+e.Action, "PRIVATE") {
				t.Fatal("CI diagnostic changed refusal facts or leaked server text", e, ok)
			}
			if tc.specific {
				if !strings.Contains(e.Message, "No completed retained source observation with CI") || strings.Contains(e.Action, "current access") {
					t.Fatal("missing CI still masquerades as access failure", e)
				}
				for _, phrase := range []string{"wait_for_ci", "reviews ci-wait", "required-check details", "final merge branch", "without a separate source refresh", "retained selected-base CI", "already-pending CI-enabled refresh", "review_source_refresh_read", "include_ci:true", "reviews source-refresh --ci", "after it completes", "did not contact GitHub or start a review"} {
					if !strings.Contains(e.Action, phrase) {
						t.Fatal("missing CI recovery/routing boundary", phrase, e)
					}
				}
			} else if e.Message != "The service refused this scoped request." || e.Action != "Check the selected instance, project, worker and current access." {
				t.Fatal("CI diagnostic weakened status safeguards", e)
			}
		})
	}
}
