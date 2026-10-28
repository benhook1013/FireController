package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// A predecessor-format fixture, not an alternate production mutation. It retains
// exact zero-start legacy epoch records for upgrade and late-history proof.
func seedLegacyRoundGrant(t *testing.T, f *attemptFixture, pull int64, channel string, revision, minimum, maximum int64, key string) {
	t.Helper()
	scope := f.scope(t, attemptProject, f.admin, true)
	roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
		prior, err := readRoundRecord(ctx, tx, scope.ProjectID, pull, channel)
		if err != nil {
			return err
		}
		if prior.Revision != revision || prior.cumulative {
			return fmt.Errorf("legacy fixture must precede cumulative edits")
		}
		at := stamp()
		created := prior.CreatedAt
		if created == "" {
			created = at
		}
		c := roundRecord{RoundControl: RoundControl{ProjectID: scope.ProjectID, PullNumber: pull, Channel: channel, Configured: true, Revision: revision + 1, Epoch: prior.Epoch + 1, Minimum: minimum, Maximum: maximum, Taper: roundTaper(channel), ActorID: scope.PrincipalID, CreatedAt: created, UpdatedAt: at}, grantRevision: revision + 1}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_round_changes(project_id,pull_number,channel,revision,epoch,action,body_json,worker_id) VALUES(?,?,?,?,?,'grant',?,'')`, c.ProjectID, pull, channel, c.Revision, c.Epoch, encoded(c.RoundControl)); err != nil {
			return err
		}
		return storeRoundRecord(ctx, tx, c)
	})
}

func TestRoundCumulativePreservesStreakMinimumAndMaxOnlyReopening(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 8, "first")
	for i := 0; i < 2; i++ {
		a, id := roundCapture(t, f, i, false)
		roundFinalize(t, f, a, id, i, false)
	}
	roundGrant(t, f, 2, "cli", 1, 0, 3, "limits")
	c := roundRead(t, f, 2, "cli")
	if c.Epoch != 1 || c.Used != 2 || c.Dry != 2 || c.Minimum != 2 || c.Maximum != 5 || c.Tapered {
		t.Fatal("edit reset progress or treated additional bounds as totals", c)
	}
	a, id := roundCapture(t, f, 2, false)
	roundFinalize(t, f, a, id, 2, false)
	if c = roundRead(t, f, 2, "cli"); c.Used != 3 || c.Dry != 3 || !c.Tapered {
		t.Fatal("third dry round lost earned taper", c)
	}
	roundGrant(t, f, 2, "cli", 2, 0, 8, "max-only")
	if c = roundRead(t, f, 2, "cli"); c.Tapered || c.Used != 3 || c.Dry != 3 || c.Minimum != 3 || c.Maximum != 11 {
		t.Fatal("max-only grant failed to reopen with earned streak and requested bounds", c)
	}
	roundGrant(t, f, 2, "cli", 3, 2, 4, "extra-minimum")
	if c = roundRead(t, f, 2, "cli"); c.Tapered || c.Minimum != 5 || c.Dry != 3 {
		t.Fatal("new minimum erased streak or stopped too early", c)
	}
	a, id = roundCapture(t, f, 3, true)
	roundFinalize(t, f, a, id, 3, true)
	if c = roundRead(t, f, 2, "cli"); c.Used != 4 || c.Dry != 0 || c.Tapered {
		t.Fatal("accepted round did not reset carried streak", c)
	}
	adoptionValid(t, f)
}

func TestRoundCumulativeUnderwayAndUnfinalizedCountOnceAgainstOneMore(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 1, "first")
	a, id := roundCapture(t, f, 0, true)
	original := roundRead(t, f, 2, "cli")
	revision, minimum, maximum := int64(1), int64(0), int64(1)
	request := RoundControlRequest{ExpectedRevision: &revision, Action: "grant", Minimum: &minimum, Maximum: &maximum}
	scope := f.scope(t, attemptProject, f.writer, false)
	receipt, err := f.e.ConfigureRoundControl(t.Context(), scope, "one-more", 2, "cli", request)
	if err != nil {
		t.Fatal(err)
	}
	c := roundRead(t, f, 2, "cli")
	if c.Epoch != original.Epoch || c.Used != 0 || c.Maximum != 1 {
		t.Fatal("one more allocated a second pending round", c)
	}
	p := readProgress(t, f, 2).Items[0].Channels[1]
	if p.Pending != 1 {
		t.Fatal("completed-unfinalized capture freed its slot", p)
	}
	_, err = f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "extra-should-refuse", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	attemptCode(t, err, "review_round_capacity")
	roundFinalize(t, f, a, id, 0, true)
	c = roundRead(t, f, 2, "cli")
	if c.Used != 1 || c.Dry != 0 || !roundDone(c) {
		t.Fatal("old revision completion failed current allowance", c)
	}
	before := roundTables(t, f)
	recovered, err := f.e.ConfigureRoundControl(t.Context(), scope, "one-more", 2, "cli", request)
	if err != nil || string(receipt) != string(recovered) || roundTables(t, f) != before {
		t.Fatal("replay recalculated thresholds after finalization", err)
	}
	roundCompleteHook(t, f, a.OperationID)
	if roundTables(t, f) != before {
		t.Fatal("completion replay counted twice")
	}
	adoptionValid(t, f)
}

func TestRoundCumulativeLateOriginalRevisionBeyondItsOldCap(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 1, "first")
	late, evidence := roundCapture(t, f, 0, true)
	roundGrant(t, f, 2, "cli", 1, 3, 3, "revised")
	for i := 1; i < 3; i++ {
		a, id := roundCapture(t, f, i, false)
		roundFinalize(t, f, a, id, i, false)
	}
	roundFinalize(t, f, late, evidence, 0, true)
	c := roundRead(t, f, 2, "cli")
	if c.Used != 3 || c.Dry != 0 || c.Epoch != 1 {
		t.Fatal("original cap rejected valid late completion ordinal", c)
	}
	var grant, epoch int64
	if err := f.store.DB().QueryRow(`SELECT grant_revision,epoch FROM review_round_operation_bindings WHERE operation_id=?`, late.OperationID).Scan(&grant, &epoch); err != nil || grant != 1 || epoch != 1 {
		t.Fatal("edit rewrote original binding", grant, epoch, err)
	}
	adoptionValid(t, f)
}

func TestRoundCumulativeAcceptedAfterClosureResetsEffectiveTaper(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 8, "first")
	late, evidence := roundCapture(t, f, 0, true)
	for i := 1; i <= 3; i++ {
		a, id := roundCapture(t, f, i, false)
		roundFinalize(t, f, a, id, i, false)
	}
	if !roundRead(t, f, 2, "cli").Tapered {
		t.Fatal("fixture did not reach taper")
	}
	revision := int64(1)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "close", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	roundFinalize(t, f, late, evidence, 0, true)
	c := roundRead(t, f, 2, "cli")
	if !c.Closed || c.Tapered || c.Dry != 0 || c.Used != 4 {
		t.Fatal("posted accepted result did not reset streak after closure", c)
	}
	adoptionValid(t, f)
}

func TestRoundCumulativeCapacityRefusalAndOriginalKeyReplay(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 4, "first")
	f.enqueue(t, attemptProject, "pending-one", "cli", 2, 1)
	f.enqueue(t, attemptProject, "pending-two", "cli", 2, 1)
	before := roundTables(t, f)
	revision, min, max := int64(1), int64(0), int64(1)
	scope := f.scope(t, attemptProject, f.writer, false)
	_, err := f.e.ConfigureRoundControl(t.Context(), scope, "too-small", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "grant", Minimum: &min, Maximum: &max})
	attemptCode(t, err, "review_round_inflight")
	if roundTables(t, f) != before {
		t.Fatal("capacity refusal changed records")
	}
	max = 2
	request := RoundControlRequest{ExpectedRevision: &revision, Action: "grant", Minimum: &min, Maximum: &max}
	first, err := f.e.ConfigureRoundControl(t.Context(), scope, "exact-recovery", 2, "cli", request)
	if err != nil {
		t.Fatal(err)
	}
	roundGrant(t, f, 2, "cli", 2, 1, 3, "later-edit")
	before = roundTables(t, f)
	replay, err := f.e.ConfigureRoundControl(t.Context(), scope, "exact-recovery", 2, "cli", request)
	if err != nil || string(first) != string(replay) || roundTables(t, f) != before {
		t.Fatal("old key recalculated a grant from current state", err)
	}
	adoptionValid(t, f)
}

func TestRoundCumulativeInitializerOnlyCreatesAbsentRule(t *testing.T) {
	f := roundFixture(t)
	scope := f.scope(t, attemptProject, f.writer, false)
	roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
		return initializeRoundControl(ctx, tx, scope, 2, "hosted", 2, 3)
	})
	c := roundRead(t, f, 2, "hosted")
	if c.Minimum != 2 || c.Maximum != 3 || c.Taper != 1 || c.Epoch != 1 {
		t.Fatal("default helper lost supplied rule", c)
	}
	var body string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_round_changes WHERE channel='hosted'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var wrapper cumulativeRoundHistory
	if json.Unmarshal([]byte(body), &wrapper) != nil || wrapper.Format != 2 {
		t.Fatal("new rule was not format2")
	}
	before := roundTables(t, f)
	roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
		return initializeRoundControl(ctx, tx, scope, 2, "hosted", 0, 9)
	})
	if roundTables(t, f) != before {
		t.Fatal("initializer changed an explicit existing rule")
	}
	adoptionValid(t, f)
}

func TestRoundCumulativeImportedEarnedProofInvalidatesOnAcceptedCompletion(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			f := adoptionFixture(t)
			prepared := adoptionPrepared()
			prepared.InitialDry = 0
			prepared.Tapered = true
			prepared.MinimumRemaining = 1
			adoptRound(t, f, prepared, "carried")
			var original string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM review_round_changes WHERE revision=1`).Scan(&original); err != nil {
				t.Fatal(err)
			}
			roundGrant(t, f, 2, "cli", 1, 2, 4, "limits")
			a, id := roundCapture(t, f, 0, accepted)
			roundFinalize(t, f, a, id, 0, accepted)
			c := roundRead(t, f, 2, "cli")
			if !c.InitiallyTapered || c.InitialDry != 0 || c.OriginSource != prepared.Source || c.Used != 1 || c.Tapered {
				t.Fatal("carried proof or minimum changed", c)
			}
			// Every explicit grant reopens, preserving the imported proof until a
			// new completion evaluates it. An accepted result invalidates that proof.
			roundGrant(t, f, 2, "cli", 2, 0, 2, "minimum-met")
			c = roundRead(t, f, 2, "cli")
			if c.Tapered || c.Dry != map[bool]int64{false: 1, true: 0}[accepted] {
				t.Fatal("accepted result failed to invalidate carried taper proof", c)
			}
			a, id = roundCapture(t, f, 1, false)
			roundFinalize(t, f, a, id, 1, false)
			if current := roundRead(t, f, 2, "cli"); current.Tapered == accepted || current.Used != 2 {
				t.Fatal("fresh result failed to evaluate retained imported proof", current)
			}
			var after string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM review_round_changes WHERE revision=1`).Scan(&after); err != nil || after != original {
				t.Fatal("edit rewrote immutable adoption baseline", err)
			}
			adoptionValid(t, f)
		})
	}
}

func TestRoundCumulativeFiniteThresholdAboveRequestBoundAndOverflowRefusal(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 3, "first")
	a, id := roundCapture(t, f, 0, false)
	roundFinalize(t, f, a, id, 0, false)
	roundGrant(t, f, 2, "cli", 1, 0, 1000, "additional-thousand")
	c := roundRead(t, f, 2, "cli")
	if c.Maximum != 1001 || c.Used != 1 || c.Minimum != 1 || c.Dry != 1 {
		t.Fatal("stored threshold reused the additional-request ceiling", c)
	}
	adoptionValid(t, f)
	// Synthetic numeric-boundary state tests refusal without manufacturing a
	// lifetime native completion history or claiming it is a valid backup.
	if _, err := f.store.DB().Exec(`UPDATE review_round_controls SET used=?,minimum=?,maximum=?,dry=0,tapered=0`, roundSafeInteger-1, roundSafeInteger, roundSafeInteger); err != nil {
		t.Fatal(err)
	}
	before := roundTables(t, f)
	revision, minimum, maximum := int64(2), int64(0), int64(2)
	_, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "overflow", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "grant", Minimum: &minimum, Maximum: &maximum})
	attemptCode(t, err, "review_round_capacity")
	if roundTables(t, f) != before {
		t.Fatal("overflow allocated a history revision or receipt")
	}
}

func TestRoundCumulativeIncompleteTerminalCaptureDoesNotRetainCapacity(t *testing.T) {
	for _, outcome := range []string{"failed", "uncertain"} {
		t.Run(outcome, func(t *testing.T) {
			f := roundFixture(t)
			roundGrant(t, f, 2, "cli", 0, 0, 1, "first")
			receipt := f.enqueue(t, attemptProject, "incomplete-operation", "cli", 2, 1)
			a := f.start(t, attemptProject, receipt)
			anchor := attemptAnchor(attemptProject, 2)
			anchor.ParentSHA = anchor.BaseSHA
			var err error
			a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, "")
			if err != nil {
				t.Fatal(err)
			}
			digest := strings.Repeat("2", 64)
			input := ReviewEvidenceInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024, ExpectedAnchor: anchor, StartedAt: time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano), FinishedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), Review: reviewprovider.ParsedReview{Outcome: outcome, Complete: false, Findings: []reviewprovider.ParsedFinding{}, ReviewedFiles: []string{}}}
			if _, err = f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input); err != nil {
				t.Fatal(err)
			}
			if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "retained_incomplete_capture", ""); err != nil {
				t.Fatal(err)
			}
			progress := readProgress(t, f, 2).Items[0].Channels[1]
			if progress.Used != 0 || progress.Pending != 0 {
				t.Fatal("incomplete capture manufactured a round or occupied a slot", progress)
			}
			roundGrant(t, f, 2, "cli", 1, 0, 1, "retry-capacity")
			adoptionValid(t, f)
		})
	}
}

func TestRoundCumulativeStoppedSettingsCannotBecomeOriginalDispatchGrant(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 8, "first")
	for i := 0; i < 3; i++ {
		a, id := roundCapture(t, f, i, false)
		roundFinalize(t, f, a, id, i, false)
	}
	roundGrant(t, f, 2, "cli", 1, 0, 8, "historical-keep-taper")
	// Preserve a real predecessor-format settings outcome. New grants reopen;
	// an older retained tapered grant remains stopped and cannot bind dispatch.
	roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
		c, err := readRoundRecord(ctx, tx, attemptProject, 2, "cli")
		if err != nil {
			return err
		}
		c.Tapered = true
		if _, err := tx.ExecContext(ctx, `UPDATE review_round_changes SET body_json=? WHERE project_id=? AND pull_number=2 AND channel='cli' AND revision=2`, encodeRoundHistory(c.RoundControl, true), attemptProject); err != nil {
			return err
		}
		return storeRoundRecord(ctx, tx, c)
	})
	if _, err := readRoundGrant(t.Context(), f.store.DB(), attemptProject, 2, "cli", 2, 1); err == nil {
		t.Fatal("already-tapered settings became an admission grant")
	}
	adoptionValid(t, f)
}

// Legacy stopped updating Dry once carried taper had stopped the channel, even
// while previously captured native reviews finalized. Conversion derives the
// actual streak from those same credited immutable results, without new credit.
func TestRoundCumulativeLegacyFrozenCarryConversion(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprintf("late_accepted_%t", accepted), func(t *testing.T) {
			f := adoptionFixture(t)
			p := adoptionPrepared()
			p.InitialDry, p.Tapered, p.MinimumRemaining = 0, true, 1
			adoptRound(t, f, p, "legacy-carry")
			late, evidence := roundCapture(t, f, 0, accepted)
			first, firstEvidence := roundCapture(t, f, 1, false)
			roundFinalize(t, f, first, firstEvidence, 1, false)
			roundFinalize(t, f, late, evidence, 0, accepted)
			before := roundRead(t, f, 2, "cli")
			if before.Used != 2 || before.Dry != 1 || !before.Tapered {
				t.Fatal("fixture lacks legacy frozen carried taper", before)
			}
			adoptionValid(t, f)
			var original string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM review_round_changes WHERE pull_number=2 AND channel='cli' AND revision=1`).Scan(&original); err != nil {
				t.Fatal(err)
			}
			roundGrant(t, f, 2, "cli", 1, 0, 2, "convert-frozen-carry")
			after := roundRead(t, f, 2, "cli")
			wantDry := int64(2)
			if accepted {
				wantDry = 0
			}
			if after.Used != before.Used || after.Epoch != before.Epoch || after.Dry != wantDry || after.Tapered || !sameRoundBaseline(before, after) {
				t.Fatal("conversion lost evidence-backed earned carry or retained accepted carry", before, after)
			}
			adoptionValid(t, f)
			roundGrant(t, f, 2, "cli", 2, 0, 3, "later-cumulative-edit")
			if current := roundRead(t, f, 2, "cli"); current.Used != 2 || current.Dry != wantDry || current.Tapered {
				t.Fatal("subsequent edit recredited or changed the projected streak", current)
			}
			var unchanged string
			var completions int64
			if err := f.store.DB().QueryRow(`SELECT body_json FROM review_round_changes WHERE pull_number=2 AND channel='cli' AND revision=1`).Scan(&unchanged); err != nil {
				t.Fatal(err)
			}
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_round_completions`).Scan(&completions); err != nil {
				t.Fatal(err)
			}
			if unchanged != original || completions != 2 {
				t.Fatal("conversion rewrote history or completion credit", completions)
			}
			adoptionValid(t, f)
			// An internally consistent public predicate alone is insufficient:
			// the conversion streak must match immutable completion evidence.
			wrongDry, wrongTaper := int64(0), "false"
			if accepted {
				wrongDry, wrongTaper = 2, "true"
			}
			if _, err := f.store.DB().Exec(`UPDATE review_round_changes SET body_json=json_set(body_json,'$.control.dry',?,'$.control.tapered',json(?)) WHERE revision=2`, wrongDry, wrongTaper); err != nil {
				t.Fatal(err)
			}
			if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err == nil {
				t.Fatal("conversion accepted a fabricated dry projection")
			}
		})
	}
}
