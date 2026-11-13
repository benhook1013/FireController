package reviewexec

import (
	"github.com/benhook1013/FireController/internal/core"
	"testing"
)

func TestOperationTimingReportsFinishRetentionAndSettlementWithoutClockAssumptions(t *testing.T) {
	f, a, in := standingEvidenceFixture(t)
	capture, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "capture_complete", a.ExternalID); err != nil {
		t.Fatal(err)
	}
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	check := func(wantFinish, wantRecord string) {
		t.Helper()
		before := evidenceState(t, f, a)
		v, err := f.e.ReadOperationStatus(t.Context(), scope, a.OperationID)
		if err != nil || v.EvidenceID != capture.ID || v.ReviewFinishedAt != wantFinish || v.EvidenceRecordedAt != wantRecord || v.Stage != "evidence_ready" {
			t.Fatal(v, err)
		}
		if before != evidenceState(t, f, a) {
			t.Fatal("timing read changed accounting or source")
		}
	}
	stored, err := f.e.ReadReviewEvidence(t.Context(), scope, capture.ID)
	if err != nil {
		t.Fatal(err)
	}
	check(in.FinishedAt, stored.RecordedAt)
	// A wall-clock correction can put local retention before provider time;
	// these are distinct source clocks, not a new readiness or coverage guard.
	earlier := "2020-01-01T00:00:00Z"
	if _, err = f.store.DB().Exec(`UPDATE review_execution_evidence SET body_json=json_set(body_json,'$.recorded_at',?) WHERE id=?`, earlier, capture.ID); err != nil {
		t.Fatal(err)
	}
	check(in.FinishedAt, earlier)
	if _, err = f.store.DB().Exec(`UPDATE review_execution_evidence SET body_json=json_remove(body_json,'$.recorded_at') WHERE id=?`, capture.ID); err != nil {
		t.Fatal(err)
	}
	check("", "")
	_, err = f.e.ReadOperationStatus(t.Context(), core.Scope{}, a.OperationID)
	if err == nil {
		t.Fatal("timing projection bypassed authorization")
	}
}
