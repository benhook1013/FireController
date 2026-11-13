package reviewexec

import (
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"reflect"
	"strings"
	"testing"
)

func TestFixBatchConflictIdentifiesStoredSHAAndRetainsAtomicRecovery(t *testing.T) {
	f, a, id := adjudicationFixture(t, 3, false)
	writer := f.scope(t, attemptProject, f.writer, false)
	if _, err := f.e.DecideReviewFindings(t.Context(), writer, "accept-three", id, decisionBatch(0, 0, 1, 2)); err != nil {
		t.Fatal(err)
	}
	before := adjudicationState(t, f, a)
	bad := fixBatch(1, 0, 1, 2)
	bad.Items[1].ExpectedSHA = adjudicationSHA(strings.Repeat("b", 40))
	_, err := f.e.RecordFindingFixes(t.Context(), writer, "wrong-pr-head", id, bad)
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "review_conflict" || failure.Status != 409 {
		t.Fatal(err)
	}
	want := map[string]any{"reason": "fix_sha_conflict", "field": "expected_sha", "current_revision": int64(1), "finding": int64(1), "current_fix_sha": "", "current_disposition": "accepted"}
	if !reflect.DeepEqual(failure.Details, want) || adjudicationState(t, f, a) != before {
		t.Fatal("conflict hid its guard or changed earlier items", failure.Details)
	}
	good := fixBatch(1, 0, 1, 2)
	for i := range good.Items {
		good.Items[i].FixSHA = strings.Repeat("c", 40)
	}
	body, err := f.e.RecordFindingFixes(t.Context(), writer, "correct-stored-sha", id, good)
	adjudicationReceipt(t, body, err, id, "fixes_recorded_batch", 2)
	replay, err := f.e.RecordFindingFixes(t.Context(), writer, "correct-stored-sha", id, good)
	if err != nil || string(replay) != string(body) {
		t.Fatal("original-key replay changed", err)
	}
	_, err = f.e.RecordFindingFixes(t.Context(), writer, "stale-revision", id, good)
	if !errors.As(err, &failure) || failure.Details.(map[string]any)["reason"] != "adjudication_revision_conflict" || failure.Details.(map[string]any)["current_revision"] != int64(2) {
		t.Fatal("revision guard was not identified", err)
	}
	for _, finding := range adjudicationRead(t, f, id).Findings {
		if finding.FixSHA != strings.Repeat("c", 40) {
			t.Fatal("fix commits were coupled to review heads")
		}
	}
}

func TestFixGuardIdentifiesUnacceptedFindingWithoutRecordingProof(t *testing.T) {
	f, a, id := adjudicationFixture(t, 1, false)
	before := adjudicationState(t, f, a)
	_, err := f.e.RecordFindingFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "undecided-fix", id, FindingFixRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("a", 40), ProofNote: "Private proof"})
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Details.(map[string]any)["reason"] != "finding_not_accepted" || failure.Details.(map[string]any)["current_disposition"] != "unadjudicated" || adjudicationState(t, f, a) != before {
		t.Fatal("disposition guard weakened or hidden", err)
	}
}
