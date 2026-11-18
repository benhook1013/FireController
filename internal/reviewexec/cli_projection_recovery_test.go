package reviewexec

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
	"strings"
	"testing"
)

func invalidProjectionFixture(t *testing.T) (*attemptFixture, Attempt, ReviewEvidence, ReviewEvidenceInput) {
	t.Helper()
	f, a, in := evidenceFixture(t, "cli", true)
	if err := EnsureAdjudicationSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCheckpointSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	complete := evidenceClone(in)
	complete.Review.Findings[0].Title = strings.Repeat("title", 123)
	in.Review = reviewprovider.ParsedReview{Outcome: "uncertain", Findings: []reviewprovider.ParsedFinding{}, ReviewedFiles: []string{}}
	rec, err := f.e.RecordReviewEvidence(t.Context(), a.ProjectID, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "uncertain", "capture_invalid", "")
	if err != nil {
		t.Fatal(err)
	}
	old, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, a.ProjectID, f.reader), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, a, old, complete
}
func TestRecoverInvalidCLIProjectionArchivesAndPreservesSource(t *testing.T) {
	f, a, old, in := invalidProjectionFixture(t)
	rec, err := f.e.RecoverInvalidCLIProjection(t.Context(), a.ProjectID, a.ID, a.Revision, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, a.ProjectID, f.reader), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != old.ID || got.Recovery == nil || !got.Capture.Review.Complete || got.Capture.Review.Findings[0].Title != in.Review.Findings[0].Title || got.RecordedAt != old.RecordedAt {
		t.Fatal(got)
	}
	var archived string
	if err = f.store.DB().QueryRow(`SELECT original_body_json FROM review_cli_evidence_recovery WHERE evidence_id=?`, old.ID).Scan(&archived); err != nil || archived != encoded(old) {
		t.Fatal(archived, err)
	}
	before := evidenceState(t, f, a)
	if _, err = f.e.RecoverInvalidCLIProjection(t.Context(), a.ProjectID, a.ID, a.Revision, in); err != nil || before != evidenceState(t, f, a) {
		t.Fatal("replay mutated", err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 1 {
		t.Fatal("reparse released slot before ordinary completion")
	}
}
func TestRecoverInvalidCLIProjectionRefusesChangesAndDecisions(t *testing.T) {
	for _, mode := range []string{"digest", "bytes", "time", "anchor", "stale", "incomplete", "decision", "audit", "capability", "configuration", "reason", "reservation"} {
		t.Run(mode, func(t *testing.T) {
			f, a, old, in := invalidProjectionFixture(t)
			revision := a.Revision
			switch mode {
			case "digest":
				in.CaptureSHA256 = strings.Repeat("2", 64)
				in.CaptureID = "review_capture_" + in.CaptureSHA256[:32]
			case "bytes":
				in.CaptureBytes++
			case "time":
				in.StartedAt = in.FinishedAt
			case "anchor":
				in.ExpectedAnchor.BaseSHA = strings.Repeat("f", 40)
			case "stale":
				revision--
			case "incomplete":
				in.Review.Complete = false
				in.Review.Outcome = "uncertain"
			case "decision":
				_, err := f.store.DB().Exec(`INSERT INTO review_adjudication_state(evidence_id,project_id,revision,body_json) VALUES(?,?,1,'{}')`, old.ID, a.ProjectID)
				if err != nil {
					t.Fatal(err)
				}
			case "capability":
				cap := f.e.capabilities["test-capability"]
				cap.Fingerprint = strings.Repeat("e", 64)
				f.e.capabilities["test-capability"] = cap
			case "configuration":
				_, err := f.store.DB().Exec(`UPDATE review_execution_config SET body_json='{}'`)
				if err != nil {
					t.Fatal(err)
				}
			case "reason":
				_, err := f.store.DB().Exec(`UPDATE review_operations SET reason='execution_interrupted' WHERE id=?`, a.OperationID)
				if err != nil {
					t.Fatal(err)
				}
			case "reservation":
				_, err := f.store.DB().Exec(`DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID)
				if err != nil {
					t.Fatal(err)
				}
			case "audit":
				_, err := f.store.DB().Exec(`CREATE TRIGGER reject_recovery_event BEFORE INSERT ON events WHEN NEW.event_type='review.cli_capture_reparsed' BEGIN SELECT RAISE(ABORT,'test'); END`)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := evidenceState(t, f, a)
			_, err := f.e.RecoverInvalidCLIProjection(t.Context(), a.ProjectID, a.ID, revision, in)
			if err == nil || before != evidenceState(t, f, a) {
				b, _ := json.Marshal(in)
				t.Fatal("unsafe recovery", string(b), err)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_cli_evidence_recovery`) != 0 {
				t.Fatal("partial archive")
			}
		})
	}
}
