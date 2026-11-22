package reviewexec

import (
	"bytes"
	"fmt"
	"testing"
)

func assertAutoFinalizationAbsent(t *testing.T, f *attemptFixture, evidenceID string, revision int64) {
	t.Helper()
	state := adjudicationRead(t, f, evidenceID)
	if state.Revision != revision || state.Finalized || state.FrozenCounts != nil {
		t.Fatalf("source finalized before every finding had a decision: %+v", state)
	}
	if count := attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_publications WHERE evidence_id=?`, evidenceID); count != 0 {
		t.Fatalf("incomplete source queued %d checkpoint publications", count)
	}
}

func assertAutoFinalizedAndPublished(t *testing.T, f *attemptFixture, evidenceID string, revision int64, counts FindingCounts) {
	t.Helper()
	state := adjudicationRead(t, f, evidenceID)
	if state.Revision != revision || !state.Finalized || state.FrozenCounts == nil || *state.FrozenCounts != counts {
		t.Fatalf("automatic finalization did not freeze source counts at the action revision: %+v", state)
	}
	if changes := attemptCount(t, f.store, `SELECT count(*) FROM review_adjudication_changes WHERE evidence_id=?`, evidenceID); int64(changes) != revision {
		t.Fatalf("automatic finalization used %d adjudication revisions; want %d", changes, revision)
	}
	if publications := attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_publications WHERE evidence_id=?`, evidenceID); publications != 1 {
		t.Fatalf("automatic finalization queued %d publications; want exactly one", publications)
	}
	var publicationID string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_checkpoint_publications WHERE evidence_id=?`, evidenceID).Scan(&publicationID); err != nil {
		t.Fatal(err)
	}
	publication, err := f.e.ReadCheckpointPublication(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), publicationID)
	if err != nil || publication.EvidenceID != evidenceID || publication.SourceRevision != revision || publication.Revision != 1 || publication.Counts != counts {
		t.Fatalf("checkpoint publication is not bound to the auto-finalized source: %+v %v", publication, err)
	}
}

func autoFinalizationReplayState(t *testing.T, f *attemptFixture, attempt Attempt, evidenceID string) string {
	t.Helper()
	var publications, receipts, events int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_checkpoint_publications WHERE evidence_id=?`, evidenceID).Scan(&publications); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s/%d/%d/%d", adjudicationState(t, f, attempt), publications, receipts, events)
}

func TestAdjudicationAutoFinalizesOnLastSingleDecision(t *testing.T) {
	f, a, evidenceID := adjudicationFixture(t, 2, false)
	adjudicationComplete(t, f, a)
	adjudicationDecide(t, f, evidenceID, "auto-first-single", 0, 0, "accepted")
	assertAutoFinalizationAbsent(t, f, evidenceID, 1)

	scope := f.scope(t, attemptProject, f.writer, false)
	request := FindingDecisionRequest{ExpectedRevision: adjudicationRevision(1), Finding: 1, Disposition: "rejected", Reason: "Explicit final source decision"}
	body, err := f.e.DecideReviewFinding(t.Context(), scope, "auto-last-single", evidenceID, request)
	first := adjudicationReceipt(t, body, err, evidenceID, "adjudicated", 2)
	assertAutoFinalizedAndPublished(t, f, evidenceID, 2, FindingCounts{Found: 2, Accepted: 1})

	before := autoFinalizationReplayState(t, f, a, evidenceID)
	replay, err := f.e.DecideReviewFinding(t.Context(), scope, "auto-last-single", evidenceID, request)
	if err != nil || !bytes.Equal(first, replay) || autoFinalizationReplayState(t, f, a, evidenceID) != before {
		t.Fatal("exact last-decision replay changed automatic finalization", err)
	}
}

func TestAdjudicationAutoFinalizesOnLastBatchDecision(t *testing.T) {
	f, a, evidenceID := adjudicationFixture(t, 2, false)
	adjudicationComplete(t, f, a)
	adjudicationDecide(t, f, evidenceID, "auto-first-batch", 0, 0, "accepted")
	assertAutoFinalizationAbsent(t, f, evidenceID, 1)

	scope := f.scope(t, attemptProject, f.writer, false)
	request := decisionBatch(1, 1)
	body, err := f.e.DecideReviewFindings(t.Context(), scope, "auto-last-batch", evidenceID, request)
	first := adjudicationReceipt(t, body, err, evidenceID, "adjudicated_batch", 2)
	assertAutoFinalizedAndPublished(t, f, evidenceID, 2, FindingCounts{Found: 2, Accepted: 2})

	before := autoFinalizationReplayState(t, f, a, evidenceID)
	replay, err := f.e.DecideReviewFindings(t.Context(), scope, "auto-last-batch", evidenceID, request)
	if err != nil || !bytes.Equal(first, replay) || autoFinalizationReplayState(t, f, a, evidenceID) != before {
		t.Fatal("exact final-batch replay changed automatic finalization", err)
	}
}

func TestAdjudicationAutoFinalizesOnFinalRoute(t *testing.T) {
	f, a, evidenceID := routeFixture(t, 2)
	adjudicationComplete(t, f, a)
	adjudicationDecide(t, f, evidenceID, "auto-first-route", 0, 0, "rejected")
	assertAutoFinalizationAbsent(t, f, evidenceID, 1)

	request := routeRequest(1, 1, 4)
	created := routeCreate(t, f, evidenceID, "auto-final-route", request)
	assertAutoFinalizedAndPublished(t, f, evidenceID, 2, FindingCounts{Found: 2, Routed: 1})
	if created.SourceRevision != 2 {
		t.Fatalf("final route receipt omitted the coupled source revision: %+v", created)
	}

	before := autoFinalizationReplayState(t, f, a, evidenceID)
	replay, err := f.e.CreateFindingRoute(t.Context(), f.scope(t, attemptProject, f.writer, false), "auto-final-route", evidenceID, request)
	if err != nil || !bytes.Equal([]byte(encoded(created)), replay) || autoFinalizationReplayState(t, f, a, evidenceID) != before {
		t.Fatal("exact final-route replay changed automatic finalization", err)
	}
}

func TestAdjudicationAutoFinalizesEarlyDecisionOnSettlement(t *testing.T) {
	f, a, evidenceID := adjudicationFixture(t, 1, false)
	adjudicationDecide(t, f, evidenceID, "auto-early-decision", 0, 0, "accepted")
	assertAutoFinalizationAbsent(t, f, evidenceID, 1)
	adjudicationComplete(t, f, a)
	assertAutoFinalizedAndPublished(t, f, evidenceID, 2, FindingCounts{Found: 1, Accepted: 1})
}

func TestAdjudicationAutoFinalizesEmptyReviewOnSettlement(t *testing.T) {
	f, a, evidenceID := adjudicationFixture(t, 0, false)
	assertAutoFinalizationAbsent(t, f, evidenceID, 0)
	adjudicationComplete(t, f, a)
	assertAutoFinalizedAndPublished(t, f, evidenceID, 1, FindingCounts{})
}

func TestAdjudicationExplicitFinalizeKeepsLegacyCompletedSourceUsable(t *testing.T) {
	f, a, evidenceID := adjudicationFixture(t, 1, false)
	adjudicationDecide(t, f, evidenceID, "legacy-decision", 0, 0, "rejected")
	// A pre-upgrade result could have been settled after every decision without
	// freezing the counts. Seed that retained state without a new completion.
	admissionSQL(t, f, `UPDATE review_execution_attempts SET state='completed',revision=revision+1 WHERE id=?`, a.ID)
	admissionSQL(t, f, `UPDATE review_operations SET state='completed',revision=revision+1 WHERE id=?`, a.OperationID)
	assertAutoFinalizationAbsent(t, f, evidenceID, 1)

	scope := f.scope(t, attemptProject, f.writer, false)
	request := FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)}
	body, err := f.e.FinalizeReviewEvidence(t.Context(), scope, "legacy-finalize", evidenceID, request)
	first := adjudicationReceipt(t, body, err, evidenceID, "finalized", 2)
	assertAutoFinalizedAndPublished(t, f, evidenceID, 2, FindingCounts{Found: 1})
	before := autoFinalizationReplayState(t, f, a, evidenceID)
	body, err = f.e.FinalizeReviewEvidence(t.Context(), scope, "legacy-finalize", evidenceID, request)
	if err != nil || !bytes.Equal(first, body) || autoFinalizationReplayState(t, f, a, evidenceID) != before {
		t.Fatal("legacy finalization replay changed the current source", err)
	}
}

func TestAdjudicationAutoFinalizationPublicationFailureRollsBackLastDecision(t *testing.T) {
	f, a, evidenceID := adjudicationFixture(t, 2, false)
	adjudicationComplete(t, f, a)
	adjudicationDecide(t, f, evidenceID, "auto-rollback-first", 0, 0, "accepted")
	before := autoFinalizationReplayState(t, f, a, evidenceID)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_auto_checkpoint BEFORE INSERT ON review_checkpoint_publications BEGIN SELECT RAISE(ABORT,'synthetic auto-finalization publication failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := f.e.DecideReviewFinding(t.Context(), f.scope(t, attemptProject, f.writer, false), "auto-rollback-last", evidenceID, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(1), Finding: 1, Disposition: "rejected", Reason: "Explicit final source decision"})
	if err == nil || autoFinalizationReplayState(t, f, a, evidenceID) != before {
		t.Fatalf("publication insertion failure did not roll back the last decision atomically: %v", err)
	}
}
