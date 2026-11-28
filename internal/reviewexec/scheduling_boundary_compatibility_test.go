package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestSchedulingBoundaryExistingChildAndUnselectedOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, intermediate string
		selected, capable  bool
	}{
		{"unchanged-owner", "test-actor", false, true},
		{"new-selection-requires-continuity", "alternate-authorizer", false, false},
		{"retained-child-keeps-continuity-rule", "alternate-authorizer", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := schedulingEventSnapshotFixture(t)
			const settings = `{"automatic_hosted":true}`
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO events(project_id,resource_id,event_type,revision,actor_id,payload_json) VALUES
			 ('test-project','test-project','review.scheduling_updated',1,'test-actor',?),
			 ('test-project','test-project','review.scheduling_updated',2,?,?),
			 ('test-project','test-project','review.scheduling_updated',3,'test-actor',?)`, settings, tc.intermediate, settings, settings); err != nil {
				t.Fatal(err)
			}
			current := schedulingRecord{Scheduling: Scheduling{ProjectID: "test-project", Revision: 3, AutomaticHosted: true}, actor: "test-actor"}
			r := Run{ProjectID: "test-project", Channel: "hosted", State: "selecting"}
			if tc.selected {
				r.State, r.OperationID = "selected", "retained-child"
			}
			if allowed, err := automaticChannelContinuous(t.Context(), tx, r, 1, "test-actor", current); err != nil || allowed != tc.capable {
				t.Fatalf("continuity allowed=%v want=%v err=%v", allowed, tc.capable, err)
			}
		})
	}
}

func TestFrontLocalSchedulingHoldPreservesIndependentAndStackedEligibility(t *testing.T) {
	f := schedulingFixture(t)
	schedulingPause(t, f, 2, 0, true)
	admissionSQL(t, f, `UPDATE review_pull_scheduling SET updated_at='' WHERE project_id=? AND pull_number=2`, attemptProject)
	parent := SourcePlan{Repository: reviewprovider.Repository{ID: 801}, Pull: reviewprovider.Pull{RepositoryID: 801, Number: 2, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("a", 40)}}
	independent := SourcePlan{Repository: parent.Repository, Pull: reviewprovider.Pull{RepositoryID: 801, Number: 4, State: "open", HeadSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("a", 40)}}
	if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		r := Run{ProjectID: attemptProject, Channel: "cli"}
		basis, eligible, err := automaticPlanningBasisForPrefix(ctx, tx, attemptProject, []int64{2, 4}, "", r.Channel)
		if err != nil || !eligible || !hash.MatchString(basis) {
			t.Fatalf("independent PR did not produce an eligible planning basis: %q %v %v", basis, eligible, err)
		}
		basis, eligible, err = automaticPlanningBasisForPrefix(ctx, tx, attemptProject, []int64{2}, "", r.Channel)
		if err != nil || eligible || !hash.MatchString(basis) {
			t.Fatalf("unreadable PR was not excluded from planning: %q %v %v", basis, eligible, err)
		}
		got, err := f.e.evaluateFront(ctx, tx, r, 801, []SourcePlan{parent, independent}, true)
		if err != nil || got.state != "ready" || got.plan.Pull.Number != 4 {
			t.Fatalf("unrelated ready candidate not available: %+v %v", got, err)
		}
		if target, reason, err := f.e.runFront(ctx, tx, r, 801, []SourcePlan{parent}, false); err != nil || target != 0 || reason != "automatic_scheduling_unavailable" {
			t.Fatalf("the first prefix did not retain a traversable local hold: %d %q %v", target, reason, err)
		}
		if target, reason, err := f.e.runFront(ctx, tx, r, 801, []SourcePlan{parent, independent}, true); err != nil || target != 4 || reason != "" {
			t.Fatalf("the final prefix did not select the independent candidate: %d %q %v", target, reason, err)
		}
		dependent := independent
		dependent.ParentPullNumber, dependent.ParentSHA = 2, parent.Pull.HeadSHA
		got, err = f.e.evaluateFront(ctx, tx, r, 801, []SourcePlan{parent, dependent}, true)
		if err != nil || got.state != "held" || got.reason != "automatic_scheduling_unavailable" {
			t.Fatalf("dependent candidate did not retain upstream hold: %+v %v", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
