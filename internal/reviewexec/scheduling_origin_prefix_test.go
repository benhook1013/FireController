package reviewexec

import (
	"context"
	"database/sql"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestAutomaticSelectionOriginSurvivesMissingAttribution(t *testing.T) {
	for _, state := range []string{"queued", "selecting", "selected"} {
		t.Run(state, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2, 4})
			schedulingEnable(t, f, 0, true)
			schedulingGenerate(t, f, true)
			run := schedulingLatest(t, f)
			reader := &refreshTestReader{}
			switch state {
			case "selecting":
				admissionSQL(t, f, `UPDATE review_selections SET state='selecting',revision=2 WHERE id=?`, run.ID)
			case "selected":
				selectionNext(t, f, reader)
				run = selectionRead(t, f, run.ID)
			}
			admissionSQL(t, f, `DELETE FROM review_automatic_selections WHERE selection_id=?`, run.ID)
			schedulingEnable(t, f, 1, false)
			err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				return ValidateSelectionSnapshot(ctx, tx)
			})
			if !automaticProjectRefused(err) {
				t.Fatal("snapshot accepted automatic selection without attribution", err)
			}
			switch state {
			case "queued":
				selectionNext(t, f, reader)
			case "selecting":
				if err := f.e.RecoverReviewRuns(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "selected":
				err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					op, err := readOperation(ctx, tx, attemptProject, run.OperationID)
					if err != nil {
						return err
					}
					return f.e.requireAutomaticOperation(ctx, tx, op)
				})
				if !automaticProjectRefused(err) {
					t.Fatal("automatic child inherited explicit admission", err)
				}
				return
			}
			if got := selectionRead(t, f, run.ID); got.State != "held" || got.Reason != "automatic_scheduling_unavailable" || got.OperationID != "" || reader.calls.Load() != 0 {
				t.Fatal("missing attribution reached source or failed to hold", got)
			}
		})
	}
}

func TestExplicitSelectionOriginRemainsUsableWithoutReceipt(t *testing.T) {
	for _, mode := range []string{"marked", "legacy-pre-enable", "legacy-pinned", "legacy-exact-receipt"} {
		t.Run(mode, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			if mode != "legacy-pre-enable" {
				schedulingEnable(t, f, 0, true)
			}
			var expected *int64
			if mode == "legacy-pinned" {
				number := int64(2)
				expected = &number
			}
			run, _ := selectionEnqueue(t, f, mode, "hosted", expected)
			if mode != "marked" {
				admissionSQL(t, f, `UPDATE events SET payload_json='{"channel":"hosted"}' WHERE resource_id=? AND event_type='review.selection_requested'`, run.ID)
			}
			if mode != "legacy-exact-receipt" {
				admissionSQL(t, f, `DELETE FROM receipts WHERE operation='reviews.run' AND key=?`, mode)
			}
			if mode == "legacy-pre-enable" {
				schedulingEnable(t, f, 0, true)
			}
			selectionNext(t, f, &refreshTestReader{})
			if got := selectionRead(t, f, run.ID); got.State != "selected" || got.OperationID == "" {
				t.Fatal("proved explicit selection failed to progress", got)
			}
		})
	}
}

func TestAutomaticPlanningIgnoresSemanticFaultBeyondQueuedCutoff(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, held := range []bool{false, true} {
			name := channel + "/selected"
			if held {
				name = channel + "/held"
			}
			t.Run(name, func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2, 4, 6})
				f.enqueue(t, attemptProject, "cutoff-queued", channel, 4, 1)
				// The ordinary retained route reader is unaffected on PR 2. At
				// PR 6 this fixture exceeds the semantic basis row bound.
				admissionSQL(t, f, `ALTER TABLE review_finding_routes RENAME TO saved_finding_routes`)
				admissionSQL(t, f, `CREATE TABLE review_finding_routes AS SELECT * FROM saved_finding_routes`)
				admissionSQL(t, f, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<10001)
				 INSERT INTO review_finding_routes(project_id,id,revision,state,disposition,has_fix,source_pull_number,target_pull_number)
				 SELECT ?,printf('route-%d',x),1,'active','accepted',0,6,7 FROM n`, attemptProject)
				var generate func(*testing.T, *attemptFixture, bool)
				if channel == "hosted" {
					schedulingEnable(t, f, 0, true)
					generate = schedulingGenerate
				} else {
					schedulingCLIEnable(t, f, 0, true)
					generate = schedulingCLIGenerate
				}
				generate(t, f, true)
				run := schedulingLatest(t, f)
				reader := &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
					if number != 2 {
						t.Errorf("source read escaped the cutoff to PR %d", number)
					}
					pull := schedulingPull(number)
					pull.IsDraft = &held
					return pull, nil
				}}
				selectionNext(t, f, reader)
				got := selectionRead(t, f, run.ID)
				if held {
					if got.State != "held" || got.Reason != "pull_draft" || got.OperationID != "" {
						t.Fatal("no-child hold did not settle", got)
					}
				} else {
					if got.State != "selected" || got.OperationID == "" {
						t.Fatal("earlier candidate did not settle", got)
					}
				}
				if err := f.e.RecoverReviewRuns(t.Context()); err != nil {
					t.Fatal(err)
				}
				generate(t, f, false)
			})
		}
	}
}

func TestAutomaticPlanningStrictCutoffPreservesProoflessLegacyHold(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2, 4, 6})
	f.enqueue(t, attemptProject, "legacy-cutoff-queued", "hosted", 4, 1)
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	run := schedulingLatest(t, f)
	draft := true
	selectionNext(t, f, &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
		pull := schedulingPull(number)
		pull.IsDraft = &draft
		return pull, nil
	}})
	if got := selectionRead(t, f, run.ID); got.State != "held" || got.OperationID != "" {
		t.Fatal("fixture did not retain a pre-child hold", got)
	}

	admissionSQL(t, f, `UPDATE events SET payload_json='{"channel":"hosted"}' WHERE resource_id=? AND event_type='review.selection_requested'`, run.ID)
	admissionSQL(t, f, `UPDATE review_automatic_selections SET
	  basis_sha256=printf('%064d',1),settled_basis_sha256=printf('%064d',2) WHERE selection_id=?`, run.ID)
	schedulingGenerate(t, f, false)

	// Enabling only the other lane keeps Hosted's basis epoch intact.
	revision, enabled := int64(1), true
	if _, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "enable-cli-preserve-hosted", SchedulingRequest{ExpectedRevision: &revision, AutomaticHosted: &enabled, AutomaticCLI: &enabled}); err != nil {
		t.Fatal(err)
	}
	schedulingGenerate(t, f, false)
	// Deliberately reasserting Hosted rearms its earlier prefix.
	schedulingEnable(t, f, 2, true)
	schedulingGenerate(t, f, true)
	if latest := schedulingLatest(t, f); latest.ID == run.ID {
		t.Fatal("rearm reused the earlier held intent", latest)
	}
}

func TestLegacyAutomaticOriginCannotBecomeExplicit(t *testing.T) {
	for _, wrongProject := range []bool{false, true} {
		name := "missing"
		if wrongProject {
			name = "wrong-project"
		}
		t.Run(name, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			schedulingEnable(t, f, 0, true)
			schedulingGenerate(t, f, true)
			run := schedulingLatest(t, f)
			admissionSQL(t, f, `UPDATE events SET payload_json='{"channel":"hosted"}' WHERE resource_id=? AND event_type='review.selection_requested'`, run.ID)
			if wrongProject {
				admissionSQL(t, f, `UPDATE review_automatic_selections SET project_id=? WHERE selection_id=?`, otherAttemptProject, run.ID)
			} else {
				admissionSQL(t, f, `DELETE FROM review_automatic_selections WHERE selection_id=?`, run.ID)
			}
			schedulingEnable(t, f, 1, false)
			reader := &refreshTestReader{}
			selectionNext(t, f, reader)
			if got := selectionRead(t, f, run.ID); got.State != "held" || got.OperationID != "" || reader.calls.Load() != 0 {
				t.Fatal("unproved legacy selection inherited explicit authority", got)
			}
		})
	}
}

func TestAutomaticPlanningKeepsLegacySelectedPullSuppressionAtCutoff(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2, 4, 6})
	f.enqueue(t, attemptProject, "legacy-attributable-cutoff", "hosted", 6, 1)
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	run := schedulingLatest(t, f)
	admissionSQL(t, f, `UPDATE events SET payload_json='{"channel":"hosted"}' WHERE resource_id=? AND event_type='review.selection_requested'`, run.ID)
	_, err := f.e.ControlOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "cancel-legacy-selected", run.OperationID, OperationControl{ExpectedRevision: 1, Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	next := schedulingLatest(t, f)
	nextOp, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), next.OperationID)
	if err != nil || nextOp.PullNumber != 4 {
		t.Fatal("legacy selected basis was lost or blocked an independent PR", next, nextOp, err)
	}
}
