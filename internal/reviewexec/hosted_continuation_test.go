package reviewexec

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func continuationFixture(t *testing.T, beforeRelease ...func(*attemptFixture)) (*attemptFixture, Attempt, *time.Time) {
	t.Helper()
	f, a, evidence := evidenceFixture(t, "hosted", false)
	evidence.ExpectedAnchor.ParentSHA = evidence.ExpectedAnchor.BaseSHA
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: evidence.ExpectedAnchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, evidence.ExpectedAnchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	in := HostedObservationInput{CaptureID: evidence.CaptureID, CaptureSHA256: evidence.CaptureSHA256, CaptureBytes: evidence.CaptureBytes, StartedAt: evidence.StartedAt, FinishedAt: evidence.FinishedAt, ObservedAt: evidence.FinishedAt, ExpectedAnchor: evidence.ExpectedAnchor, Intent: intent}
	if _, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, in); err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "trigger_recorded", "101")
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "hosted_finished_inventory_unproven", "")
	if err != nil {
		t.Fatal(err)
	}
	var at string
	if err = f.store.DB().QueryRow(`SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&at); err != nil {
		t.Fatal(err)
	}
	first, _ := time.Parse(time.RFC3339Nano, at)
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, attemptProject)
	admissionSQL(t, f, `UPDATE grants SET can_manage=1 WHERE credential_id='exec-writer-cred'`)
	schedulingEnable(t, f, 0, true)
	now := first.Add(HostedIncompleteGrace)
	f.e.attemptNow = func() time.Time { return now }
	f.e.schedulingNow = func() time.Time { return now }
	for _, setup := range beforeRelease {
		setup(f)
	}
	released, err := f.e.ReleaseFinishedHosted(t.Context(), attemptProject, a.ID, a.Revision, incompleteHostedSource(in))
	if err != nil {
		t.Fatal(err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_hosted_continuations`) != 1 {
		t.Fatal("release did not qualify continuation")
	}
	return f, released, &now
}
func readContinuationTest(t *testing.T, f *attemptFixture) *hostedContinuation {
	t.Helper()
	c, err := scanContinuation(f.store.DB().QueryRow(`SELECT ` + continuationColumns + ` FROM review_hosted_continuations`))
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	return c
}
func TestHostedContinuationConsumesOneSelectionAndPreservesOriginal(t *testing.T) {
	f, a, now := continuationFixture(t)
	if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		config, _, err := readConfiguration(ctx, tx, attemptProject)
		if err != nil {
			return err
		}
		c, err := f.e.pendingHostedContinuation(ctx, tx, attemptProject, config)
		if c != nil {
			t.Fatal("early credit")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(hostedContinuationDelay)
	schedulingGenerate(t, f, true)
	c := readContinuationTest(t, f)
	if c.Selection == "" || c.Child != "" {
		t.Fatal(c)
	}
	schedulingGenerate(t, f, false)
	if got, err := readAttempt(t.Context(), f.store.DB(), attemptProject, a.ID); err != nil || got.State != "failed" || got.Revision != a.Revision {
		t.Fatal(got, err)
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateHostedContinuationsSnapshot(t.Context(), tx)
	tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	// A new engine must reconstruct the durable consumed receipt, not mint a
	// fresh in-memory budget after restart.
	cliAllowanceReopen(t, f)
	f.e, err = New(f.store, attemptCapabilities, WithExecutionClock(func() time.Time { return *now }))
	if err != nil {
		t.Fatal(err)
	}
	f.e.attemptNow = func() time.Time { return *now }
	if err = f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	if again := readContinuationTest(t, f); again.Selection != c.Selection {
		t.Fatal("restart reset credit")
	}
	selectionNext(t, f, &refreshTestReader{})
	c = readContinuationTest(t, f)
	if c.Child == "" {
		t.Fatal("no child operation", selectionRead(t, f, c.Selection))
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 2 {
		t.Fatal("duplicate operation")
	}
	child := f.start(t, attemptProject, Receipt{ID: c.Child})
	changed := c.Anchor
	changed.BaseSHA = "ffffffffffffffffffffffffffffffffffffffff"
	if _, err = f.e.Admit(t.Context(), attemptProject, child.ID, child.Revision, changed, "unused"); err == nil {
		t.Fatal("changed comparison admitted")
	}
	command, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: c.Child, AttemptID: child.ID, HeadSHA: c.Anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	if admitted, err := f.e.Admit(t.Context(), attemptProject, child.ID, child.Revision, c.Anchor, command); err != nil || admitted.State != "posting" {
		t.Fatal("exact continuation could not admit", admitted, err)
	}
	tx, err = f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateHostedContinuationsSnapshot(t.Context(), tx)
	tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}

}
func TestHostedContinuationSupersessionAndAuthorization(t *testing.T) {
	for _, mode := range []string{"manual cancelled", "disabled", "configuration", "root evidence", "capture"} {
		t.Run(mode, func(t *testing.T) {
			f, _, now := continuationFixture(t)
			*now = now.Add(hostedContinuationDelay)
			c := readContinuationTest(t, f)
			switch mode {
			case "manual cancelled":
				r := f.enqueue(t, attemptProject, "manual-after-release", "hosted", c.Anchor.PullNumber, 1)
				timerControl(t, f, r.ID, "cancel", "cancel-manual")
			case "disabled":
				schedulingEnable(t, f, 1, false)
			case "configuration":
				f.configure(t, attemptProject, "other-account", 1, false)
			case "root evidence":
				admissionSQL(t, f, `UPDATE review_operations SET state='completed' WHERE id=?`, c.Operation)
			case "capture":
				admissionSQL(t, f, `UPDATE review_hosted_observations SET capture_sha256=? WHERE attempt_id=?`, "bad", c.Root)
			}
			err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error { return f.e.validateHostedContinuation(ctx, tx, c) })
			if err == nil {
				t.Fatal("unsafe continuation accepted")
			}
		})
	}
}

func TestHostedContinuationCoordinatorRaceAndLateManualCancellation(t *testing.T) {
	f, _, now := continuationFixture(t)
	*now = now.Add(hostedContinuationDelay)
	results := make(chan bool, 6)
	errs := make(chan error, 6)
	// Each engine normally serializes its coordinator. Race independent
	// coordinators sharing the durable state, including the final writer guard.
	for range 6 {
		engine, err := New(f.store, attemptCapabilities)
		if err != nil {
			t.Fatal(err)
		}
		engine.schedulingNow = f.e.schedulingNow
		go func() {
			worked, err := engine.generateAutomaticHosted(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
			results <- worked
			errs <- err
		}()
	}
	successes := 0
	for i := 0; i < 6; i++ {
		if <-results {
			successes++
		}
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatal("duplicate consumption", successes)
	}
	c := readContinuationTest(t, f)
	selectionNext(t, f, &refreshTestReader{})
	c = readContinuationTest(t, f)
	if c.Child == "" {
		t.Fatal("missing child")
	}
	a := f.start(t, attemptProject, Receipt{ID: c.Child})
	manual, _ := selectionEnqueue(t, f, "manual-during-prepare", "hosted", &c.Anchor.PullNumber)
	selectionNext(t, f, &refreshTestReader{})
	if r := selectionRead(t, f, manual.ID); r.State != "held" {
		t.Fatal("expected terminal manual intent", r)
	}
	if _, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, c.Anchor, "unused"); err == nil {
		t.Fatal("terminal superseding request was ignored at final admission")
	}
	if count := attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`); count != 0 {
		t.Fatal("failed work received review credit", count)
	}
}

func TestHostedContinuationNeverMintsAChildRoot(t *testing.T) {
	f, _, now := continuationFixture(t)
	*now = now.Add(hostedContinuationDelay)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	c := readContinuationTest(t, f)
	a := f.start(t, attemptProject, Receipt{ID: c.Child})
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		o, err := readOperationBase(ctx, tx, attemptProject, c.Child)
		if err != nil {
			return err
		}
		return f.e.qualifyHostedContinuation(ctx, tx, a, o, HostedIncompleteCapture{}, *now)
	})
	if err != nil {
		t.Fatal(err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_hosted_continuations`) != 1 {
		t.Fatal("fresh child replenished retry budget")
	}
}

func TestHostedContinuationExactFreshSourceAndConsumedHold(t *testing.T) {
	f, _, now := continuationFixture(t)
	*now = now.Add(hostedContinuationDelay)
	schedulingGenerate(t, f, true)
	c := readContinuationTest(t, f)
	reader := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: c.Anchor.CandidateSHA, BaseSHA: "ffffffffffffffffffffffffffffffffffffffff", BaseRef: "main", ChangedFiles: 1}, nil
	}}
	selectionNext(t, f, reader)
	r := selectionRead(t, f, c.Selection)
	if r.OperationID != "" || r.State != "held" {
		t.Fatal("changed base consumed an operation", r)
	}
	if readContinuationTest(t, f).Selection != c.Selection {
		t.Fatal("hold returned credit to pool")
	}
}

func TestHostedContinuationSnapshotRejectsCorruptProvenance(t *testing.T) {
	for _, damage := range []string{"scheduling_revision=scheduling_revision+1", "release_revision=release_revision+1", "capture_sha256='ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff'", "accepted_sequence=accepted_sequence+1", "not_before='2099-01-01T00:00:00Z'"} {
		t.Run(damage, func(t *testing.T) {
			f, _, _ := continuationFixture(t)
			admissionSQL(t, f, `UPDATE review_hosted_continuations SET `+damage)
			tx, err := f.store.DB().BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err = ValidateHostedContinuationsSnapshot(t.Context(), tx); err == nil {
				t.Fatal("corruption accepted")
			}
		})
	}
}

func TestHostedContinuationCannotBypassClosedRoundControl(t *testing.T) {
	f, _, now := continuationFixture(t)
	*now = now.Add(hostedContinuationDelay)
	c := readContinuationTest(t, f)
	zero := int64(0)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close-continuation", c.Anchor.PullNumber, "hosted", RoundControlRequest{ExpectedRevision: &zero, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	schedulingGenerate(t, f, true)
	c = readContinuationTest(t, f)
	selectionNext(t, f, &refreshTestReader{})
	if r := selectionRead(t, f, c.Selection); r.OperationID != "" {
		t.Fatal("closed channel created operation", r)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 {
		t.Fatal("closed channel dispatched")
	}
}

func TestHostedContinuationRecoveryWinningBeforeChildIsNotDispatched(t *testing.T) {
	for _, consumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-selection", true: "after-selection"}[consumed], func(t *testing.T) {
			f, _, now := continuationFixture(t)
			*now = now.Add(hostedContinuationDelay)
			if consumed {
				schedulingGenerate(t, f, true)
			}
			c := readContinuationTest(t, f)
			// Model the atomic winning settlement state. The recovery endpoint's own
			// complete-evidence/journal proof is exercised by retained-recovery tests.
			admissionSQL(t, f, `UPDATE review_operations SET state='completed' WHERE id=?`, c.Operation)
			if consumed {
				selectionNext(t, f, &refreshTestReader{})
				if r := selectionRead(t, f, c.Selection); r.OperationID != "" {
					t.Fatal("settled root reused", r)
				}
			} else {
				err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					config, _, err := readConfiguration(ctx, tx, attemptProject)
					if err != nil {
						return err
					}
					candidate, err := f.e.pendingHostedContinuation(ctx, tx, attemptProject, config)
					if candidate != nil {
						t.Fatal("settled root nominated")
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
				t.Fatal("recovery race created child")
			}
		})
	}
}

func TestHostedContinuationUnrelatedSelectionAtReleaseKeepsReceipt(t *testing.T) {
	var unrelated Receipt
	f, _, now := continuationFixture(t, func(f *attemptFixture) {
		other := int64(4)
		unrelated, _ = selectionEnqueue(t, f, "unrelated-at-release", "hosted", &other)
	})
	*now = now.Add(hostedContinuationDelay)
	schedulingGenerate(t, f, false) // existing selector owns the lane
	selectionNext(t, f, &refreshTestReader{})
	if r := selectionRead(t, f, unrelated.ID); r.State != "held" {
		t.Fatal(r)
	}
	schedulingGenerate(t, f, true)
	c := readContinuationTest(t, f)
	selectionNext(t, f, &refreshTestReader{})
	if got := readContinuationTest(t, f); got.Selection != c.Selection || got.Child == "" {
		t.Fatal(got)
	}
}

func TestHostedContinuationQueuedChildInvalidationSettlesWithoutAttempt(t *testing.T) {
	for _, mode := range []string{"terminal-manual", "disable"} {
		t.Run(mode, func(t *testing.T) {
			f, _, now := continuationFixture(t)
			*now = now.Add(hostedContinuationDelay)
			schedulingGenerate(t, f, true)
			selectionNext(t, f, &refreshTestReader{})
			c := readContinuationTest(t, f)
			if mode == "disable" {
				schedulingEnable(t, f, 1, false)
			} else {
				manual, _ := selectionEnqueue(t, f, "late-manual", "hosted", &c.Anchor.PullNumber)
				selectionNext(t, f, &refreshTestReader{})
				if r := selectionRead(t, f, manual.ID); r.State != "held" {
					t.Fatal(r)
				}
			}
			a, err := f.e.StartAttempt(t.Context(), attemptProject, c.Child)
			if err != nil || a.ID != "" {
				t.Fatal(a, err)
			}
			o, err := readOperationBase(t.Context(), f.store.DB(), attemptProject, c.Child)
			if err != nil || o.State != "held" || o.AttemptID != "" {
				t.Fatal(o, err)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 {
				t.Fatal("invalidated child began")
			}
		})
	}
}

func TestHostedContinuationTransientFrontHoldRetainsSelection(t *testing.T) {
	f, _, now := continuationFixture(t)
	*now = now.Add(hostedContinuationDelay)
	schedulingGenerate(t, f, true)
	c := readContinuationTest(t, f)
	// Model the selector's exact transient front outcome without a provider
	// operation. Recovery must reuse this identity, never refund the receipt.
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		r, err := readRun(ctx, tx, attemptProject, c.Selection)
		if err != nil {
			return err
		}
		return updateRunAt(ctx, tx, r, "held", "expected_pr_mismatch", "", *now)
	})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(hostedContinuationDelay)
	schedulingGenerate(t, f, true)
	if got := readContinuationTest(t, f); got.Selection != c.Selection {
		t.Fatal("new selection", got)
	}
	selectionNext(t, f, &refreshTestReader{})
	if got := readContinuationTest(t, f); got.Child == "" || got.Selection != c.Selection {
		t.Fatal(got)
	}
}

func TestHostedContinuationEarlierFrontRunsBeforeSameSelectionResumes(t *testing.T) {
	f, _, now := continuationFixture(t, func(f *attemptFixture) {
		// Synthetic fixture starts with PR4 ahead of original PR2; retain the
		// same exact configuration revision throughout the observed release.
		admissionSQL(t, f, `UPDATE review_execution_config SET body_json=json_set(body_json,'$.queue',json('[4,2]')) WHERE project_id=?`, attemptProject)
		scope := f.scope(t, attemptProject, f.admin, true)
		if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			return reviews.PublishExecutionQueue(ctx, tx, scope, []int64{2, 4}, []int64{4, 2})
		}); err != nil {
			t.Fatal(err)
		}

	})
	*now = now.Add(hostedContinuationDelay)
	schedulingGenerate(t, f, true)
	c := readContinuationTest(t, f)
	selectionNext(t, f, &refreshTestReader{})
	if r := selectionRead(t, f, c.Selection); r.State != "held" || r.Reason != "expected_pr_mismatch" || r.OperationID != "" {
		t.Fatal(r)
	}
	// An ordinary selection is not suppressed by the receipt's read-only hold.
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	var earlier string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_operations WHERE project_id=? AND pull_number=4 AND state='queued'`, attemptProject).Scan(&earlier); err != nil {
		t.Fatal("earlier front did not progress", err)
	}
	timerControl(t, f, earlier, "cancel", "end-earlier")
	// Earlier work is now closed for this channel. No config or global policy
	// mutation revokes the later receipt; normal front checks observe closure.
	zero := int64(0)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close-earlier", 4, "hosted", RoundControlRequest{ExpectedRevision: &zero, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(hostedContinuationDelay)
	// The first ordinary read establishes the changed earlier basis; it must
	// not replace the retained receipt with a fresh provider budget.
	for i := 0; i < 3 && readContinuationTest(t, f).Child == ""; i++ {
		worked, err := f.e.generateAutomaticHosted(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
		if err != nil || !worked {
			t.Fatalf("iteration %d: %v %v receipt=%+v selection=%+v operations=%s", i, worked, err, readContinuationTest(t, f), selectionRead(t, f, c.Selection), snapshotTable(t, f, "review_operations"))
		}
		selectionNext(t, f, &refreshTestReader{})
		*now = now.Add(hostedContinuationDelay)
	}
	got := readContinuationTest(t, f)
	if got.Selection != c.Selection || got.Child == "" {
		t.Fatal("receipt lost after earlier front", got)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_hosted_continuations`) != 1 {
		t.Fatal("budget reminted")
	}
}

func TestHostedContinuationSupersessionDoesNotStopSupervisor(t *testing.T) {
	f, _, now := continuationFixture(t)
	*now = now.Add(hostedContinuationDelay)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	c := readContinuationTest(t, f)
	_, _ = selectionEnqueue(t, f, "supersede-queued-child", "hosted", &c.Anchor.PullNumber)
	selectionNext(t, f, &refreshTestReader{})
	independent := f.enqueue(t, attemptProject, "independent-cli", "cli", 4, 1)
	driver := &controlledDriver{prepare: func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
		return Preparation{}, sourceHold("source_unavailable")
	}}
	if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
		t.Fatal("supervisor stopped at continuation refusal", err)
	}
	o, err := readOperationBase(t.Context(), f.store.DB(), attemptProject, c.Child)
	if err != nil || o.State != "held" || o.AttemptID != "" {
		t.Fatal(o, err)
	}
	other, err := readOperationBase(t.Context(), f.store.DB(), attemptProject, independent.ID)
	if err != nil || other.State != "held" || other.AttemptID == "" || driver.prepares.Load() != 1 || driver.calls.Load() != 0 {
		t.Fatal("independent preparation did not progress", other, err)
	}
}

func TestHostedContinuationChildLinkFailureRollsBackRequest(t *testing.T) {
	f, _, now := continuationFixture(t)
	*now = now.Add(hostedContinuationDelay)
	schedulingGenerate(t, f, true)
	c := readContinuationTest(t, f)
	admissionSQL(t, f, `CREATE TRIGGER ignore_continuation_link BEFORE UPDATE OF child_operation ON review_hosted_continuations BEGIN SELECT RAISE(IGNORE); END`)
	// Use the real worker; a refused atomic link must not leave a new operation.
	_, _ = f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 || readContinuationTest(t, f).Child != "" {
		t.Fatal("orphan request survived refused lineage link")
	}
	if readContinuationTest(t, f).Selection != c.Selection {
		t.Fatal("receipt refunded")
	}
}
