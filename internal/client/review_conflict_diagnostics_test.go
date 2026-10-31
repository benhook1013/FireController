package client_test

import (
	"errors"
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewConflictNamesGuardAndFiltersPrivateMetadata(t *testing.T) {
	for _, mode := range []string{"fix_sha_conflict", "finding_not_accepted", "adjudication_revision_conflict", "unknown", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			details := map[string]any{"reason": mode, "field": "expected_sha", "current_revision": 0, "finding": 1, "current_fix_sha": "", "current_disposition": "accepted", "private_proof": "PRIVATE prose"}
			switch mode {
			case "finding_not_accepted":
				details["field"] = "disposition"
				details["current_disposition"] = "unadjudicated"
			case "adjudication_revision_conflict":
				details["field"] = "expected_revision"
			case "malformed":
				details["reason"] = "fix_sha_conflict"
				details["current_fix_sha"] = "PRIVATE bogus SHA"
			}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				write(w, 409, map[string]any{"error": map[string]any{"code": "review_conflict", "message": "PRIVATE server prose", "details": details}, "request_id": "request-original"})
			}))
			defer s.Close()
			c := newClient(t, s)
			_, err := c.RecordReviewFixes(t.Context(), "original-key", "review_evidence_"+strings.Repeat("c", 32), client.ReviewFixBatchRequest{ExpectedRevision: 2, Items: []client.ReviewFixBatchItem{{Finding: 1, ExpectedSHA: strings.Repeat("a", 40), FixSHA: strings.Repeat("a", 40), ProofNote: "Private proof"}}})
			var got *client.Error
			if !errors.As(err, &got) || got.Code != "review_conflict" || got.Status != 409 || got.RequestID != "request-original" || got.OutcomeUnknown {
				t.Fatal(got, err)
			}
			text := got.Message + got.Action
			if strings.Contains(text, "PRIVATE") || strings.Contains(text, "Check the selected instance") {
				t.Fatal("misleading or private diagnostic", got)
			}
			metadata, _ := got.Details.(map[string]any)
			if _, ok := metadata["private_proof"]; ok {
				t.Fatal("private metadata escaped")
			}
			if mode == "unknown" || mode == "malformed" {
				if metadata["reason"] != nil {
					t.Fatal("unvalidated guard escaped")
				}
			} else if metadata["reason"] != mode || metadata["current_revision"] != int64(0) {
				t.Fatal("guard dropped", got)
			}
			if mode == "fix_sha_conflict" && (!strings.Contains(text, "currently recorded fix_sha") || !strings.Contains(text, "empty string")) {
				t.Fatal("expected_sha meaning absent", got)
			}
		})
	}
}
