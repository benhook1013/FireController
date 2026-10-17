package reviewruntime

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func runtimeRoundGrant(t *testing.T, f *cliDriverFixture, pull int64, channel string, maximum int64) {
	t.Helper()
	zero, minimum := int64(0), int64(0)
	_, err := f.engine.ConfigureRoundControl(t.Context(), f.scope(t, f.admin, true), fmt.Sprintf("grant-%d-%s", pull, channel), pull, channel, reviewexec.RoundControlRequest{ExpectedRevision: &zero, Action: "grant", Minimum: &minimum, Maximum: &maximum})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRoundControlsRealRunnerRepeatsCoveredHeadUntilThreeDryRounds(t *testing.T) {
	f := newCLIDriverFixture(t)
	// Older native coverage remains evidence, but is not additional-round credit.
	seedCLIDriverNativeCoverage(t, f, 2)
	runtimeRoundGrant(t, f, 2, "cli", 8)
	for i := 1; i <= 3; i++ {
		op, a := f.prepareAdmit(t, 2, 1, fmt.Sprintf("additional-cli-%d", i))
		result, err := f.driver.Dispatch(t.Context(), op, a, nil)
		if err != nil || result.State != "completed" {
			t.Fatal("covered head prevented required next round", result, err)
		}
		if _, err = f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, result.State, result.Reason, ""); err != nil {
			t.Fatal(err)
		}
		var evidence string
		if err = f.store.DB().QueryRow(`SELECT id FROM review_execution_evidence WHERE attempt_id=?`, a.ID).Scan(&evidence); err != nil {
			t.Fatal(err)
		}
		zero := int64(0)
		if _, err = f.engine.DecideReviewFinding(t.Context(), f.scope(t, f.writer, false), fmt.Sprintf("reject-%d", i), evidence, reviewexec.FindingDecisionRequest{ExpectedRevision: &zero, Finding: 0, Disposition: "rejected", Reason: "Synthetic fixture finding is deliberately not accepted"}); err != nil {
			t.Fatal(err)
		}
		one := int64(1)
		if _, err = f.engine.FinalizeReviewEvidence(t.Context(), f.scope(t, f.writer, false), fmt.Sprintf("finalize-%d", i), evidence, reviewexec.FinalizeReviewRequest{ExpectedRevision: &one}); err != nil {
			t.Fatal(err)
		}
		state, err := f.engine.ReadRoundControl(t.Context(), f.scope(t, f.writer, false), 2, "cli")
		if err != nil || state.Used != int64(i) || state.Dry != int64(i) || state.Tapered != (i == 3) {
			t.Fatal("actual native finalization did not count once", state, err)
		}
	}
	before, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(before)), "\n"); len(lines) != 6 {
		t.Fatal("required rounds did not make exactly three usage/review pairs", len(lines))
	}
	_, err = f.engine.Enqueue(context.Background(), f.scope(t, f.writer, false), "after-taper", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: f.headSHA})
	assertCoreErrorCode(t, err, "review_round_complete")
	after, _ := os.ReadFile(f.log)
	if string(after) != string(before) {
		t.Fatal("taper started another provider process")
	}
}

func TestRoundControlsCloseAfterPrepareBlocksRealRunner(t *testing.T) {
	f := newCLIDriverFixture(t)
	runtimeRoundGrant(t, f, 2, "cli", 3)
	op, a := f.prepareAdmit(t, 2, 1, "prepare-then-close")
	before, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	one := int64(1)
	if _, err := f.engine.ConfigureRoundControl(t.Context(), f.scope(t, f.admin, true), "close-prepared", 2, "cli", reviewexec.RoundControlRequest{ExpectedRevision: &one, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	result, err := f.driver.Dispatch(t.Context(), op, a, nil)
	if err != nil || result.State != "failed" {
		t.Fatal("closed channel reached provider", result, err)
	}
	after, err := os.ReadFile(f.log)
	if err != nil || string(after) != string(before) {
		t.Fatal("closed dispatch launched an additional provider process", err)
	}
}
