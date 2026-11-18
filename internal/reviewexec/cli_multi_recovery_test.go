package reviewexec

import (
	"testing"
	"time"
)

func TestEndedPartialCLIAllowsLaterCompleteRecovery(t *testing.T) {
	f, a, evidence := uncertainCapturedCLI(t, 0, "uncertain")
	if err := f.e.RecordCLIProcessExit(t.Context(), a.ProjectID, a.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	second := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "later-complete-cli", "cli", 4, 1))
	anchor := attemptAnchor(otherAttemptProject, 4)
	anchor.ParentSHA = anchor.BaseSHA
	var err error
	second, err = f.e.Admit(t.Context(), second.ProjectID, second.ID, second.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	input := evidence.Capture
	input.ExpectedAnchor = anchor
	input.Review.Outcome = "completed"
	input.Review.Complete = true
	if _, err = f.e.RecordReviewEvidence(t.Context(), second.ProjectID, second.ID, input); err != nil {
		t.Fatal(err)
	}
	second, err = f.e.Advance(t.Context(), second.ProjectID, second.ID, second.Revision, "uncertain", "capture_publication_pending", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.e.RecoverRetainedCLI(t.Context(), "test-capability"); err != nil {
		t.Fatal(err)
	}
	op, err := f.e.ReadOperation(t.Context(), evidenceReadScope(t, f, second.ProjectID, f.other), second.OperationID)
	if err != nil || op.State != "completed" {
		t.Fatal("later complete capture starved", op, err)
	}
	op, err = f.e.ReadOperation(t.Context(), evidenceReadScope(t, f, a.ProjectID, f.reader), a.OperationID)
	if err != nil || op.State != "uncertain" {
		t.Fatal("partial original unexpectedly settled", op, err)
	}
}
