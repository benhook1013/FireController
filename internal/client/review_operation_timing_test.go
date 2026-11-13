package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompactOperationTimingPreservesDistinctClocksAndRejectsInvalidKnownValues(t *testing.T) {
	for _, mode := range []string{"valid", "reversed_clocks", "missing_recorded", "no_evidence", "invalid_time", "null_time"} {
		t.Run(mode, func(t *testing.T) {
			fields := map[string]any{"id": executionOperationID, "pull_number": 2, "channel": "hosted", "state": "completed", "revision": 7, "stage": "evidence_ready", "reason": "hosted_evidence_recovered", "attempt_id": "review_attempt_" + strings.Repeat("b", 32), "evidence_id": "review_evidence_" + strings.Repeat("c", 32), "review_finished_at": "2026-01-01T00:00:00Z", "evidence_recorded_at": "2026-01-01T00:04:24Z", "updated_at": "2026-01-01T00:04:25Z", "next_action": "read_evidence"}
			switch mode {
			case "reversed_clocks":
				fields["evidence_recorded_at"] = "2025-12-31T23:59:59Z"
			case "missing_recorded":
				delete(fields, "evidence_recorded_at")
			case "no_evidence":
				delete(fields, "evidence_id")
			case "invalid_time":
				fields["review_finished_at"] = "PRIVATE bogus time"
			case "null_time":
				fields["review_finished_at"] = nil
			}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				write(w, 200, fields)
			}))
			defer s.Close()
			c := newClient(t, s)
			got, err := c.ReviewOperationStatus(t.Context(), executionOperationID)
			if mode == "valid" || mode == "reversed_clocks" {
				if err != nil || got.ReviewFinishedAt == "" || got.EvidenceRecordedAt != fields["evidence_recorded_at"] {
					t.Fatal(got, err)
				}
			} else {
				checkError(t, err, "invalid_response")
			}
		})
	}
}
