package reviewexec

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

// A synthetic pre-upgrade database contains the normal automatic attribution
// and a terminal ancestor-era hold, with no linked child. Restart must preserve
// its history: unchanged planning fingerprints deliberately do not reopen it.
func TestStackedReviewUpgradeRetainsOldHoldAndAllowsGuardedSelection(t *testing.T) {
	for _, capClosed := range []bool{false, true} {
		t.Run(map[bool]string{false: "eligible-child", true: "child-grant-closed"}[capClosed], func(t *testing.T) {
			f := schedulingFixture(t)
			roundCapture(t, f, 1, false) // The parent's own CLI finding remains unresolved.
			revision := int64(1)
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "stacked-upgrade", Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}); err != nil {
				t.Fatal(err)
			}
			schedulingCLIEnable(t, f, 0, true)
			schedulingCLIGenerate(t, f, true)
			old := schedulingLatest(t, f)
			// Reproduce the durable transition performed by the old selector. The first
			// held parent determines the public reason even when its child was blocked
			// only by the old ancestor-completion rule.
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				if err := updateRun(ctx, tx, old, "selecting", "", ""); err != nil {
					return err
				}
				claimed, err := readRun(ctx, tx, attemptProject, old.ID)
				if err != nil {
					return err
				}
				if err = updateRun(ctx, tx, claimed, "held", "review_evidence_pending", ""); err != nil {
					return err
				}
				held, err := readRun(ctx, tx, attemptProject, old.ID)
				if err != nil {
					return err
				}
				return f.e.settleAutomaticBasis(ctx, tx, held, 0)
			})
			if err != nil {
				t.Fatal(err)
			}
			old = selectionRead(t, f, old.ID)
			counts := selectionCounts(t, f)
			restarted, err := New(f.store, attemptCapabilities)
			if err != nil {
				t.Fatal(err)
			}
			f.e = restarted
			if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
				t.Fatal(err)
			}
			schedulingCLIGenerate(t, f, false)
			if selectionCounts(t, f) != counts || !reflect.DeepEqual(selectionRead(t, f, old.ID), old) {
				t.Fatal("restart rewrote or regenerated held intent")
			}
			if capClosed {
				zero := int64(0)
				if _, err = f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close-upgrade-child", 4, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "close"}); err != nil {
					t.Fatal(err)
				}
			}
			before := attemptCount(t, f.store, `SELECT count(*) FROM review_operations`)
			next, receipt := selectionEnqueue(t, f, "authorized-post-upgrade-selection", "cli", nil)
			selectionNext(t, f, schedulingStackReader())
			got := selectionRead(t, f, next.ID)
			if capClosed {
				if got.OperationID != "" || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != before {
					t.Fatal("normal reevaluation bypassed target grant", got)
				}
			} else {
				if got.State != "selected" || got.OperationID == "" {
					t.Fatal("ordinary reevaluation did not select eligible child", got)
				}
				child := roundOperation(t, f, got.OperationID)
				if child.PullNumber != 4 || child.AllowOutOfOrder || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != before+1 {
					t.Fatal("unexpected child or bypass", child)
				}
			}
			after := selectionCounts(t, f)
			_, replay := selectionEnqueue(t, f, "authorized-post-upgrade-selection", "cli", nil)
			if string(replay) != string(receipt) || selectionCounts(t, f) != after || !reflect.DeepEqual(selectionRead(t, f, old.ID), old) {
				t.Fatal("replay duplicated work or changed historical hold")
			}
		})
	}
}
