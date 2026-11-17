package reviewexec

import "testing"

func TestVerifyActionRetainedRequestRequiresCurrentParentEnrollment(t *testing.T) {
	for _, scenario := range []string{"unrelated suffix removed", "parent removed before admission", "parent removed after admission"} {
		t.Run(scenario, func(t *testing.T) {
			f, prior, id, parent := coverageFixture(t, 1, false)
			adjudicationDecide(t, f, id, "reject-parent", 0, 0, "rejected")
			coverageFinalize(t, f, prior, id, 1)
			revision := int64(1)
			c := Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{4, 2, 6}, Channels: []string{"cli", "hosted"}}
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "stack", c); err != nil {
				t.Fatal(err)
			}
			child := f.start(t, attemptProject, f.enqueue(t, attemptProject, "child", "cli", 4, 2))
			anchor := attemptAnchor(attemptProject, 4)
			anchor.ParentPullNumber, anchor.ParentSHA, anchor.BaseSHA, anchor.BaseRef = 2, parent.Pull.HeadSHA, parent.Pull.HeadSHA, "parent-branch"
			admit := func() error {
				var err error
				child, err = f.e.Admit(t.Context(), attemptProject, child.ID, child.Revision, anchor, "")
				return err
			}
			if scenario == "parent removed after admission" {
				if err := admit(); err != nil {
					t.Fatal(err)
				}
			}
			revision = 2
			c.Queue = []int64{4, 2}
			if scenario != "unrelated suffix removed" {
				c.Queue = []int64{4, 6}
			}
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "suffix", c); err != nil {
				t.Fatal(err)
			}
			if scenario != "parent removed after admission" {
				current, err := f.e.ExecutionContext(t.Context(), attemptProject, child.OperationID)
				if err != nil || len(current.Configuration.Queue) != 3 || current.Configuration.Queue[1] != 2 {
					t.Fatal("original source configuration lost", current, err)
				}
				err = admit()
				if scenario == "parent removed before admission" {
					attemptCode(t, err, "review_conflict")
					return
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			parent.Pull.HeadRef = "parent-branch"
			before := adjudicationState(t, f, child)
			err := f.e.VerifyAction(t.Context(), attemptProject, child.ID, child.Revision, parent)
			if scenario == "parent removed after admission" {
				attemptCode(t, err, "validation_failed")
			} else if err != nil {
				t.Fatal("unrelated removal blocked action", err)
			}
			if before != adjudicationState(t, f, child) {
				t.Fatal("final guard changed retained state")
			}
		})
	}
}
