package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"
)

func freshIntentSource(o Operation) SourceObservation {
	a := attemptAnchor(o.ProjectID, o.PullNumber)
	return SourceObservation{RepositoryID: a.RepositoryID, RepositoryName: a.Repository, PullNumber: o.PullNumber, HeadSHA: strings.Repeat("c", 40), BaseSHA: a.BaseSHA, BaseRef: a.BaseRef, ParentPullNumber: a.ParentPullNumber, ParentSHA: a.BaseSHA, ObservedAt: stamp()}
}
func TestQueuedIntentRebindKeepsHistoryAndOnlyOneSuccessor(t *testing.T) {
	f := supervisorFixture(t)
	receipt := f.enqueue(t, attemptProject, "late-intent", "cli", 2, 1)
	o := cliWaitRead(t, f, receipt.ID)
	source := freshIntentSource(o)
	var wg sync.WaitGroup
	success := make(chan Operation, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next, err := f.e.rebindQueuedIntent(t.Context(), o, source)
			if err == nil {
				success <- next
			} else {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(success)
	close(errs)
	if len(success) != 1 || len(errs) != 1 {
		t.Fatal("replacement must have one winner", len(success), len(errs))
	}
	next := <-success
	old := cliWaitRead(t, f, o.ID)
	if old.State != "cancelled" || old.HeadSHA != o.HeadSHA || next.State != "queued" || next.HeadSHA != source.HeadSHA || next.ID == o.ID {
		t.Fatal(old, next)
	}
	lineage, err := readIntentLineage(t.Context(), f.store.DB(), attemptProject, next.ID)
	if err != nil || lineage.RootOperationID != o.ID || lineage.IntentCreatedAt != o.CreatedAt || lineage.Depth != 1 {
		t.Fatal(lineage, err)
	}
	if err = ValidateIntentRebindingSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}
func TestQueuedIntentRebindPreservesQuotaFloorAndBudget(t *testing.T) {
	f, a, _, now := cliWaitFixture(t)
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	old := cliWaitRead(t, f, a.OperationID)
	source := freshIntentSource(old)
	source.ObservedAt = now.UTC().Format(time.RFC3339Nano)
	next, err := f.e.rebindQueuedIntent(t.Context(), old, source)
	if err != nil {
		t.Fatal(err)
	}
	lineage, err := readIntentLineage(t.Context(), f.store.DB(), attemptProject, next.ID)
	if err != nil || lineage.CLIExhaustions != 1 || lineage.NotBefore != old.Wait.RetryNotBefore {
		t.Fatal(lineage, err)
	}
	if ready, err := intentWaitElapsed(t.Context(), f.store.DB(), next, *now); err != nil || ready {
		t.Fatal("lost original floor", ready, err)
	}
	*now = cliWaitDue(t, old).Add(time.Second)
	if ready, err := intentWaitElapsed(t.Context(), f.store.DB(), next, *now); err != nil || !ready {
		t.Fatal(ready, err)
	}
	source.HeadSHA = strings.Repeat("d", 40)
	source.ObservedAt = now.UTC().Format(time.RFC3339Nano)
	newer, err := f.e.rebindQueuedIntent(t.Context(), next, source)
	if err != nil {
		t.Fatal(err)
	}
	lineage, err = readIntentLineage(t.Context(), f.store.DB(), attemptProject, newer.ID)
	if err != nil || lineage.CLIExhaustions != 1 || lineage.NotBefore != old.Wait.RetryNotBefore || lineage.Depth != 2 || lineage.RootOperationID != old.ID {
		t.Fatal(lineage, err)
	}
	if roundRead(t, f, 2, "cli").Used != 0 {
		t.Fatal("source changes spent a review round")
	}
}
func TestQueuedIntentRebindRefusesStartedOrUnknownHistory(t *testing.T) {
	f := supervisorFixture(t)
	receipt := f.enqueue(t, attemptProject, "late-claimed", "cli", 2, 1)
	old := cliWaitRead(t, f, receipt.ID)
	a := f.start(t, attemptProject, receipt)
	if _, err := f.e.rebindQueuedIntent(t.Context(), old, freshIntentSource(old)); err == nil {
		t.Fatal("rebound a claimed attempt")
	}
	if a.ID == "" {
		t.Fatal("fixture did not claim")
	}
}

func TestQueuedIntentRebindMissingLineageCannotBecomeExplicit(t *testing.T) {
	f := supervisorFixture(t)
	r := f.enqueue(t, attemptProject, "late-lost-lineage", "cli", 2, 1)
	old := cliWaitRead(t, f, r.ID)
	next, err := f.e.rebindQueuedIntent(t.Context(), old, freshIntentSource(old))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.DB().ExecContext(t.Context(), `DELETE FROM review_intent_rebindings WHERE successor_operation_id=?`, next.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = readIntentLineage(t.Context(), f.store.DB(), attemptProject, next.ID); err == nil {
		t.Fatal("lost lineage accepted as explicit")
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = automaticOperationSelection(t.Context(), tx, next); err == nil {
		t.Fatal("lost automatic provenance accepted")
	}
}

func TestQueuedIntentRebindRejectsLoweredNegativeBudget(t *testing.T) {
	for _, mutation := range []string{`cli_exhaustions=0`, `not_before=''`} {
		t.Run(mutation, func(t *testing.T) {
			f, a, _, now := cliWaitFixture(t)
			if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
				t.Fatal(err)
			}
			old := cliWaitRead(t, f, a.OperationID)
			source := freshIntentSource(old)
			source.ObservedAt = now.UTC().Format(time.RFC3339Nano)
			next, err := f.e.rebindQueuedIntent(t.Context(), old, source)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.DB().ExecContext(t.Context(), `UPDATE review_intent_rebindings SET `+mutation+` WHERE successor_operation_id=?`, next.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = readIntentLineage(t.Context(), f.store.DB(), attemptProject, next.ID); err == nil {
				t.Fatal("negative budget corruption accepted")
			}
		})
	}
}

func TestPreparingIntentReplacementIsAtomicAndDoesNotCharge(t *testing.T) {
	f := supervisorFixture(t)
	r := f.enqueue(t, attemptProject, "late-preparing", "cli", 2, 1)
	old := cliWaitRead(t, f, r.ID)
	a := f.start(t, attemptProject, r)
	next, err := f.e.replacePreparingIntent(t.Context(), old, a, freshIntentSource(old))
	if err != nil {
		t.Fatal(err)
	}
	prior := cliWaitRead(t, f, r.ID)
	if prior.State != "cancelled" || prior.Reason != "source_superseded" || next.State != "queued" {
		t.Fatal(prior, next)
	}
	if roundRead(t, f, 2, "cli").Used != 0 {
		t.Fatal("unused preparation charged a round")
	}
	if _, err = f.e.replacePreparingIntent(t.Context(), old, a, freshIntentSource(old)); err == nil {
		t.Fatal("duplicate replacement accepted")
	}
	if err = ValidateIntentRebindingSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error { return ValidateOperationWaitsSnapshot(ctx, tx) }); err != nil {
		t.Fatal("replacement history failed snapshot validation", err)
	}
	var reservations int
	if err = f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatal(reservations, err)
	}
}

func TestQueuedIntentDoesNotReserveLaterPlanningCandidates(t *testing.T) {
	f := supervisorFixture(t)
	f.enqueue(t, attemptProject, "late-front-pending", "cli", 2, 1)
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	c, _, err := readConfiguration(t.Context(), tx, attemptProject)
	if err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"cli", "hosted"} {
		limit, err := automaticHostedQueuedPrefixChannel(t.Context(), tx, attemptProject, c, channel)
		if err != nil || limit != len(c.Queue) {
			t.Fatal("queued intent reserved following PRs", channel, limit, err)
		}
	}
	pending, err := automaticHostedPendingPullChannel(t.Context(), tx, attemptProject, 2, "cli")
	if err != nil || !pending {
		t.Fatal("discovery lost duplicate suppression", pending, err)
	}
}

func TestQueuedIntentRebindPreservesAutomaticAuthorizer(t *testing.T) {
	f := schedulingFixture(t)
	schedulingCLIEnable(t, f, 0, true)
	schedulingCLIGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	run := schedulingLatest(t, f)
	old := cliWaitRead(t, f, run.OperationID)
	next, err := f.e.rebindQueuedIntent(t.Context(), old, freshIntentSource(old))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		selection, err := automaticOperationSelection(ctx, tx, next)
		if err != nil {
			return err
		}
		if selection != run.ID {
			t.Fatal("lost original selection", selection, run.ID)
		}
		return f.e.requireAutomaticOperation(ctx, tx, next)
	}); err != nil {
		t.Fatal(err)
	}
	schedulingCLIEnable(t, f, 1, false)
	if err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error { return f.e.requireAutomaticOperation(ctx, tx, next) }); err == nil {
		t.Fatal("rebound intent bypassed disabled automation")
	}
}

func TestQueuedIntentRebindDoesNotBroadenConsumedHostedContinuation(t *testing.T) {
	f, _, now := continuationFixture(t)
	*now = now.Add(hostedContinuationDelay)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	c := readContinuationTest(t, f)
	if c.Child == "" {
		t.Fatal("missing continuation child")
	}
	old := cliWaitRead(t, f, c.Child)
	source := freshIntentSource(old)
	source.ObservedAt = now.UTC().Format(time.RFC3339Nano)
	before := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_hosted_continuations")
	if _, err := f.e.rebindQueuedIntent(t.Context(), old, source); err == nil {
		t.Fatal("broadened exact-source continuation")
	}
	if after := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_hosted_continuations"); after != before {
		t.Fatal("failed replacement mutated continuation")
	}
}

func TestQueuedIntentRepeatedMovesCannotResetSourceRetryBudget(t *testing.T) {
	f, r, now := sourceFixture(t)
	root := r.ID
	for i := 0; i < sourceRetryLimit; i++ {
		scheduleSourceFixture(t, f, r, *now)
		old := cliWaitRead(t, f, r.ID)
		source := freshIntentSource(old)
		source.HeadSHA = strings.Repeat(string(rune('c'+i)), 40)
		source.ObservedAt = now.UTC().Format(time.RFC3339Nano)
		next, err := f.e.rebindQueuedIntent(t.Context(), old, source)
		if err != nil {
			t.Fatal(err)
		}
		v, err := readIntentLineage(t.Context(), f.store.DB(), attemptProject, next.ID)
		if err != nil || v.SourceRetries != i+1 || v.RootOperationID != root {
			t.Fatal(v, err)
		}
		if a, err := f.e.StartAttempt(t.Context(), attemptProject, next.ID); err != nil || a.ID != "" {
			t.Fatal("rebind bypassed source floor", a, err)
		}
		*now = now.Add(time.Minute + time.Second)
		r = Receipt{ID: next.ID}
	}
	a := f.start(t, attemptProject, r)
	if retry, err := f.e.ScheduleSourceRetry(t.Context(), attemptProject, a.ID, a.Revision, sourceFailure(*now)); err != nil || retry {
		t.Fatal("parent moves replenished source retries", retry, err)
	}
	if roundRead(t, f, 2, "cli").Used != 0 {
		t.Fatal("source retries charged round")
	}
}

func TestSupervisorRebindsThenDispatchesCurrentComparisonExactlyOnce(t *testing.T) {
	f := supervisorFixture(t)
	r := f.enqueue(t, attemptProject, "late-dispatch-flow", "cli", 2, 1)
	old := cliWaitRead(t, f, r.ID)
	source := freshIntentSource(old)
	driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, c Configuration, a Attempt) (Preparation, error) {
		if o.HeadSHA == old.HeadSHA {
			return Preparation{}, &IntentSourceChanged{Source: source}
		}
		anchor := attemptAnchor(o.ProjectID, o.PullNumber)
		anchor.CandidateSHA = o.HeadSHA
		return Preparation{Anchor: anchor}, nil
	}}
	drivers := map[string]Driver{"test-capability": driver}
	if err := f.e.processNext(t.Context(), drivers); err != nil {
		t.Fatal(err)
	}
	if driver.calls.Load() != 0 || roundRead(t, f, 2, "cli").Used != 0 {
		t.Fatal("unused source handoff dispatched or charged")
	}
	if err := f.e.processNext(t.Context(), drivers); err != nil {
		t.Fatal(err)
	}
	if driver.calls.Load() != 1 || driver.prepares.Load() != 2 {
		t.Fatal("successor did not dispatch exactly once", driver.calls.Load(), driver.prepares.Load())
	}
	var candidate string
	if err := f.store.DB().QueryRow(`SELECT json_extract(anchor_json,'$.candidate_sha') FROM review_execution_attempts WHERE state='completed'`).Scan(&candidate); err != nil || candidate != source.HeadSHA {
		t.Fatal("run did not pin current comparison", candidate, err)
	}
}

func TestPreparingIntentFailedTransactionLeavesOriginalClaimRecoverable(t *testing.T) {
	f := supervisorFixture(t)
	r := f.enqueue(t, attemptProject, "late-rollback", "cli", 2, 1)
	old := cliWaitRead(t, f, r.ID)
	a := f.start(t, attemptProject, r)
	source := freshIntentSource(old)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_intent_insert BEFORE INSERT ON review_intent_rebindings BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.replacePreparingIntent(t.Context(), old, a, source); err == nil {
		t.Fatal("injected write failure succeeded")
	}
	current := cliWaitRead(t, f, r.ID)
	if current.State != "preparing" || current.AttemptID != a.ID || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
		t.Fatal("failed write left partial retirement", current)
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER fail_intent_insert`); err != nil {
		t.Fatal(err)
	}
	next, err := f.e.replacePreparingIntent(t.Context(), old, a, source)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.e.replacePreparingIntent(t.Context(), old, a, source)
	if err != nil || again.ID != next.ID || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 2 {
		t.Fatal("lost-response retry minted another successor", again, err)
	}
}
