package reviewexec

import "testing"

func TestVerifyActionRetainedRequestRequiresCurrentParentEnrollment(t *testing.T) {
	for _, removeParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrelated suffix removed", true: "actual parent removed"}[removeParent], func(t *testing.T) {
			f, prior, id, parent := coverageFixture(t, 1, false)
			adjudicationDecide(t, f, id, "reject-parent", 0, 0, "rejected")
			coverageFinalize(t, f, prior, id, 1)
			revision := int64(1)
			c := Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{4, 2, 6}, Channels: []string{"cli", "hosted"}}
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "stack", c); err != nil {
				t.Fatal(err)
			}
			child := f.start(t, attemptProject, f.enqueue(t, attemptProject, "child", "cli", 4, 2))
			// Keep source resolution bound to the original queue even after its suffix changes.
			revision = 2
			c.Queue = []int64{4, 2}
			if removeParent {
				c.Queue = []int64{4, 6}
			}
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "suffix", c); err != nil {
				t.Fatal(err)
			}
			current, err := f.e.ExecutionContext(t.Context(), attemptProject, child.OperationID)
			if err != nil || len(current.Configuration.Queue) != 3 || current.Configuration.Queue[1] != 2 {
				t.Fatal("original source configuration lost", current, err)
			}
			anchor := attemptAnchor(attemptProject, 4)
			anchor.ParentPullNumber, anchor.ParentSHA, anchor.BaseSHA, anchor.BaseRef = 2, parent.Pull.HeadSHA, parent.Pull.HeadSHA, "parent-branch"
			child, err = f.e.Admit(t.Context(), attemptProject, child.ID, child.Revision, anchor, "")
			if err != nil {
				t.Fatal(err)
			}
			parent.Pull.HeadRef = "parent-branch"
			before := adjudicationState(t, f, child)
			err = f.e.VerifyAction(t.Context(), attemptProject, child.ID, child.Revision, parent)
			if removeParent {
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
