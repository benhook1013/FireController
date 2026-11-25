package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"
)

func TestSchedulingSnapshotDiscoversDetachedNativeHoldWithoutJournalRow(t *testing.T) {
	f := schedulingFixture(t)
	receipt := f.enqueue(t, attemptProject, "detached-local-scheduling", "cli", 2, 1)
	attempt := f.start(t, attemptProject, receipt)
	if _, err := f.e.Advance(t.Context(), attemptProject, attempt.ID, attempt.Revision, "held", "pull_scheduling_paused", ""); err != nil {
		t.Fatal(err)
	}
	assertCLIWaitSnapshot(t, f)
	admissionSQL(t, f, "DELETE FROM review_execution_transitions WHERE attempt_id=? AND revision=2", attempt.ID)
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return ValidateOperationWaitsSnapshot(ctx, tx)
	})
	if err == nil {
		t.Fatal("snapshot accepted unverifiable detached history")
	}
}

func TestPausedPlanningBasisUsesDistinctIdentityOnlyForUnreadableLocalEvidence(t *testing.T) {
	f, _, evidence := adjudicationFixture(t, 1, false)
	adjudicationDecide(t, f, evidence, "paused-basis-decision", 0, 0, "rejected")
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, attemptProject)
	schedulingPause(t, f, 2, 0, true)
	var fullBasis, fullPullBasis, degradedBasis string
	read := func(opaque bool) {
		t.Helper()
		err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			plan, eligible, err := automaticPlanningBasisForPrefix(ctx, tx, attemptProject, []int64{2, 4}, "", "hosted")
			if err != nil || !eligible {
				t.Fatalf("independent pull unavailable: %v %v", eligible, err)
			}
			neighbor, err := automaticPullBasisChannel(ctx, tx, attemptProject, 4, "hosted")
			if err != nil {
				return err
			}
			paused, err := automaticPullBasisChannel(ctx, tx, attemptProject, 2, "hosted")
			var input any
			if opaque {
				degradedBasis = plan
				if err == nil || paused != "" {
					t.Fatal("unreadable pull acquired a per-pull digest")
				}
				input = []any{"planning_basis_degraded_paused_v1", []any{[]any{int64(2), nil, int64(1)}, []any{int64(4), neighbor}}, []int64{4}}
			} else {
				fullBasis, fullPullBasis = plan, paused
				if err != nil {
					return err
				}
				input = []any{[]any{[]any{int64(2), paused}, []any{int64(4), neighbor}}, []int64{4}}
			}
			expected := sha256.Sum256([]byte(encoded(input)))
			if plan != hex.EncodeToString(expected[:]) {
				t.Fatalf("unexpected planning identity: %q", plan)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	read(false)
	admissionSQL(t, f, `UPDATE review_execution_evidence SET body_json=json_set(body_json,'$.capture.review.Findings',json('{}')) WHERE id=?`, evidence)
	read(true)
	old, _ := selectionEnqueue(t, f, "full-only-held-intent", "hosted", nil)
	admissionSQL(t, f, `UPDATE review_selections SET state='held',reason='source_unavailable',revision=3 WHERE id=?`, old.ID)
	admissionSQL(t, f, `INSERT INTO review_automatic_selections(selection_id,project_id,scheduling_revision,basis_sha256,actor_id,worker_id,credential_id)
		SELECT id,project_id,1,?,actor_id,worker_id,'test-credential' FROM review_selections WHERE id=?`, fullBasis, old.ID)
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		plan, eligible, err := automaticPlanningBasisForPrefix(ctx, tx, attemptProject, []int64{2, 4}, "", "hosted")
		if err != nil || eligible || plan != degradedBasis || plan == fullBasis {
			t.Fatalf("old unattributed hold reached fresh planning: %q %v %v", plan, eligible, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Exact per-pull custody on the different, paused PR no longer
	// makes the old lane intent ambiguous relative to ready PR 4.
	admissionSQL(t, f, `UPDATE review_automatic_selections SET pull_number=2,pull_basis_sha256=? WHERE selection_id=?`, fullPullBasis, old.ID)
	read(true)
	schedulingPause(t, f, 2, 1, false)
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		plan, eligible, err := automaticPlanningBasisForPrefix(ctx, tx, attemptProject, []int64{2, 4}, "", "hosted")
		var unreadable *automaticPullAdjudicationUnavailable
		if plan != "" || eligible || !errors.As(err, &unreadable) {
			t.Fatalf("active unreadable row was not refused: %q %v %v", plan, eligible, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
