package reviewexec

import (
	"context"
	"database/sql"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"strings"
	"testing"
	"time"
)

func TestPartialFindingsNoticeUsesOriginalCaptureWithoutCompletedCredit(t *testing.T) {
	for _, state := range []string{"failed", "cancelled", "rate_limited"} {
		t.Run(state, func(t *testing.T) {
			f, a := completionNoticeStartedFixture(t, "cli")
			anchor := attemptAnchor(a.ProjectID, 2)
			var err error
			a, err = f.e.Admit(t.Context(), a.ProjectID, a.ID, a.Revision, anchor, "")
			if err != nil {
				t.Fatal(err)
			}
			digest := strings.Repeat("2", 64)
			capture := ReviewEvidenceInput{
				CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024,
				StartedAt:      time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano),
				FinishedAt:     time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
				ExpectedAnchor: anchor,
				Review: reviewprovider.ParsedReview{Outcome: "failed", Complete: false, Findings: []reviewprovider.ParsedFinding{
					{Title: "Retained finding", Detail: "Exact partial finding needs a decision.", Severity: "medium"},
				}},
			}
			evidence, err := f.e.RecordReviewEvidence(t.Context(), a.ProjectID, a.ID, capture)
			if err != nil {
				t.Fatal(err)
			}
			if completionNoticeCount(t, f) != 0 {
				t.Fatal("capture alone notified")
			}
			_, err = f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, state, "fixture_partial", "")
			if err != nil {
				t.Fatal(err)
			}
			if completionNoticeCount(t, f) != 1 {
				t.Fatal("expected one actionable notice, not duplicate generic stop")
			}
			var body string
			if err := f.store.DB().QueryRow("SELECT body FROM inbox_messages").Scan(&body); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{evidence.ID, a.OperationID, "Retained findings: 1", "not a completed review round", "need adjudication"} {
				if !strings.Contains(body, want) {
					t.Fatalf("missing %q: %s", want, body)
				}
			}
			if strings.Contains(body, "results ready") {
				t.Fatal("partial claimed completion")
			}
			if n := attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions WHERE attempt_id=? AND state='completed'`, a.ID); n != 0 {
				t.Fatal("partial got completed transition")
			}
			err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				o, err := readOperation(ctx, tx, a.ProjectID, a.OperationID)
				if err != nil {
					return err
				}
				_, err = f.e.publishPartialFindingsNotice(ctx, tx, o, a.Provider, state)
				return err
			})
			if err != nil || completionNoticeCount(t, f) != 1 {
				t.Fatal("dedup failed", err)
			}
		})
	}
}
