package reviewexec

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestFailedEmptyCLIStartCannotConsumeCompletedRoundsOrTaper(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 3, 5, "failed-start-cli-grant")
	roundGrant(t, f, 2, "hosted", 0, 0, 2, "failed-start-hosted-grant")
	// A real dry streak one completion short of both the minimum and taper
	// catches either incrementing or resetting that streak on a failed start.
	for _, n := range []int{100, 101} {
		a, evidence := roundCapture(t, f, n, false)
		roundFinalize(t, f, a, evidence, n, false)
	}
	before := roundRead(t, f, 2, "cli")
	hostedBefore := roundRead(t, f, 2, "hosted")
	if before.Used != 2 || before.Dry != 2 || before.Tapered || before.Minimum-before.Used != 1 || before.Maximum-before.Used != 3 {
		t.Fatal("fixture did not establish the native accounting boundary", before)
	}
	request := f.enqueue(t, attemptProject, "failed-empty-cli-start", "cli", 2, 1)
	a := f.start(t, attemptProject, request)
	anchor := attemptAnchor(attemptProject, 2)
	anchor.ParentSHA = anchor.BaseSHA
	a, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("f", 64)
	failed, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, ReviewEvidenceInput{
		CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024,
		StartedAt: time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano), FinishedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), ExpectedAnchor: anchor,
		Review: reviewprovider.ParsedReview{Outcome: "failed", Complete: false, ReportedFindings: 0, Findings: []reviewprovider.ParsedFinding{}, ReviewedFiles: []string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "failed", "execution_failed", ""); err != nil {
		t.Fatal(err)
	}
	read := evidenceReadScope(t, f, attemptProject, f.reader)
	retained, err := f.e.ReadReviewEvidence(t.Context(), read, failed.ID)
	if err != nil || retained.Capture.Review.Outcome != "failed" || retained.Capture.Review.Complete || len(retained.Capture.Review.Findings) != 0 || retained.Capture.Review.ReportedFindings != 0 {
		t.Fatal("failed capture invented a completed zero result or findings", retained, err)
	}
	state := adjudicationRead(t, f, failed.ID)
	if state.Finalized || state.FrozenCounts != nil || len(state.Findings) != 0 {
		t.Fatal("failed empty capture gained finalized zero-count evidence", state)
	}
	writer := f.scope(t, attemptProject, f.writer, false)
	_, err = f.e.FinalizeReviewEvidence(t.Context(), writer, "must-not-finalize-failed-zero", failed.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
	attemptCode(t, err, "review_conflict")
	roundCompleteHook(t, f, a.OperationID)
	if got := roundRead(t, f, 2, "cli"); !reflect.DeepEqual(got, before) || !reflect.DeepEqual(roundRead(t, f, 2, "hosted"), hostedBefore) {
		t.Fatal("failed empty start changed allowance, dry streak or taper", got, before)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions WHERE operation_id=?`, a.OperationID) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`) != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 {
		t.Fatal("failed start acquired an ordinal or retained execution capacity")
	}
	progress := readProgress(t, f, 2).Items[0].Channels[1]
	if progress.Channel != "cli" || progress.Pending != 0 || progress.Used != 2 || progress.Dry != 2 || progress.Tapered || progress.Minimum-progress.Used != 1 || progress.Maximum-progress.Used != 3 {
		t.Fatal("public accounting treated failed 0/0/0 as a completed round", progress)
	}
	// A genuinely completed zero-finding result remains eligible once, even
	// when both channels were explicitly closed before its finalization.
	clean, evidence := roundCapture(t, f, 102, false)
	for _, channel := range []string{"cli", "hosted"} {
		if _, err := f.e.ConfigureRoundControl(t.Context(), writer, "close-after-failed-start-"+channel, 2, channel, RoundControlRequest{ExpectedRevision: adjudicationRevision(1), Action: "close"}); err != nil {
			t.Fatal(err)
		}
	}
	finalize := FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)}
	original, err := f.e.FinalizeReviewEvidence(t.Context(), writer, "genuine-clean-zero-finalize", evidence, finalize)
	if err != nil {
		t.Fatal(err)
	}
	finalized := adjudicationRead(t, f, evidence)
	if !finalized.Finalized || finalized.FrozenCounts == nil || *finalized.FrozenCounts != (FindingCounts{}) {
		t.Fatal("genuine completed zero result did not retain exact 0/0/0 counts", finalized)
	}
	cli, hosted := roundRead(t, f, 2, "cli"), roundRead(t, f, 2, "hosted")
	if !cli.Closed || cli.Revision != 2 || cli.Used != 3 || cli.Dry != 3 || !cli.Tapered || cli.Minimum-cli.Used != 0 || cli.Maximum-cli.Used != 2 || !hosted.Closed || hosted.Revision != 2 || hosted.Used != 0 || hosted.Dry != 0 {
		t.Fatal("real completion changed closure or received incorrect credit", cli, hosted)
	}
	var ordinal int64
	if err := f.store.DB().QueryRow(`SELECT ordinal FROM review_round_completions WHERE operation_id=?`, clean.OperationID).Scan(&ordinal); err != nil || ordinal != 3 {
		t.Fatal("failed start consumed the clean result's completion ordinal", ordinal, err)
	}
	checkpoint := roundTables(t, f)
	replayed, err := f.e.FinalizeReviewEvidence(t.Context(), writer, "genuine-clean-zero-finalize", evidence, finalize)
	if err != nil || !bytes.Equal(original, replayed) {
		t.Fatal("original completion receipt did not replay", err)
	}
	roundCompleteHook(t, f, clean.OperationID)
	roundCompleteHook(t, f, a.OperationID)
	if replay := f.enqueue(t, attemptProject, "failed-empty-cli-start", "cli", 2, 1); !reflect.DeepEqual(replay, request) || roundTables(t, f) != checkpoint {
		t.Fatal("original failed request or completion replay purchased another round")
	}
}
