package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestHostedLookaheadKeepsAncestorFindingsAndOwnGuard(t *testing.T) {
	for _, finalized := range []bool{false, true} {
		t.Run(map[bool]string{false: "unadjudicated", true: "accepted-unfixed"}[finalized], func(t *testing.T) {
			hostedAdmissionNoHTTP(t)
			f, prior, id, parent := hostedComparisonFixture(t, false)
			adjudicationComplete(t, f, prior)
			if finalized {
				adjudicationDecide(t, f, id, "lookahead-accept", 0, 0, "accepted")
				adjudicationDecide(t, f, id, "lookahead-reject-one", 1, 1, "rejected")
				adjudicationDecide(t, f, id, "lookahead-reject-two", 2, 2, "rejected")
				body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "lookahead-finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(3)})
				adjudicationReceipt(t, body, err, id, "finalized", 4)
			}
			child, parents := hostedAdmissionStack(t, f, parent)
			parent = parents[0]
			childPlan := parent
			childPlan.Pull.Number, childPlan.Pull.HeadSHA = 4, child.Anchor.CandidateSHA
			childPlan.Pull.BaseSHA, childPlan.Pull.BaseRef = parent.Pull.HeadSHA, parent.Pull.HeadRef
			childPlan.ParentPullNumber, childPlan.ParentSHA = 2, parent.Pull.HeadSHA
			before := adjudicationState(t, f, prior)
			if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				own, err := f.e.evaluateFrontPlan(ctx, tx, Run{ProjectID: attemptProject, Channel: "hosted"}, 801, &parent)
				if err != nil || own.state != "held" || own.reason != "review_evidence_pending" {
					t.Fatalf("own unresolved evidence no longer holds: %+v %v", own, err)
				}
				got, err := f.e.evaluateFront(ctx, tx, Run{ProjectID: attemptProject, Channel: "hosted"}, 801, []SourcePlan{parent, childPlan}, true)
				if err != nil || got.state != "ready" || got.plan == nil || got.plan.Pull.Number != 4 {
					t.Fatalf("Hosted successor did not advance past ancestor findings: %+v %v", got, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			hostedAdmissionVerify(t, f, child, "", "", parents...)
			if adjudicationState(t, f, prior) != before {
				t.Fatal("look-ahead changed ancestor evidence or review credit")
			}
			hostedComparisonRead(t, f, prior, parent, ComparisonState{Pending: true})
			err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				op, err := readOperation(ctx, tx, attemptProject, child.OperationID)
				if err != nil {
					return err
				}
				current, err := f.e.currentExecutionContext(ctx, tx, op)
				if err != nil {
					return err
				}
				current.Operation.Channel = "cli"
				return verifyPredecessors(ctx, tx, attemptProject, current, *child.Anchor, parents)
			})
			if err != nil || adjudicationState(t, f, prior) != before {
				t.Fatal("CLI ancestor findings blocked child verification or changed retained evidence", err)
			}
			moved := append([]SourcePlan(nil), parents...)
			moved[0].Pull.HeadSHA = strings.Repeat("9", 40)
			hostedAdmissionVerify(t, f, child, "validation_failed", "", moved...)
		})
	}
}

func TestHostedLookaheadSelectionContinuesThroughHeldPrefix(t *testing.T) {
	f, prior, _, parent := hostedComparisonFixture(t, false)
	adjudicationComplete(t, f, prior)
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, attemptProject)
	one := int64(1)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "lookahead-selection-stack", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	parent.Pull.HeadRef = "lookahead-parent"
	child := parent.Pull
	child.Number, child.HeadSHA, child.HeadRef = 4, strings.Repeat("9", 40), "lookahead-child"
	child.BaseSHA, child.BaseRef = parent.Pull.HeadSHA, parent.Pull.HeadRef
	reader := &actualParentSelectionReader{pulls: map[int64]reviewprovider.Pull{2: parent.Pull, 4: child}}
	r, _ := selectionEnqueue(t, f, "lookahead-selection", "hosted", nil)
	selectionNext(t, f, reader)
	got := selectionRead(t, f, r.ID)
	if got.State != "selected" || got.OperationID == "" {
		t.Fatal("held prefix stopped source acquisition", got)
	}
	op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), got.OperationID)
	if err != nil || op.PullNumber != 4 || op.HeadSHA != child.HeadSHA || op.AllowOutOfOrder {
		t.Fatal("selection lost ordinary child intent", op, err)
	}
	frame, found, err := readFrontFrame(t.Context(), f.store.DB(), attemptProject, "hosted")
	if err != nil || !found || !frame.complete || len(frame.prefix) != 2 {
		t.Fatal("missing verified prefix", frame, err)
	}
	if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		front, err := f.e.evaluateFront(ctx, tx, Run{ProjectID: attemptProject, Channel: "hosted"}, 801, frame.plans(), true)
		if err != nil || front.state != "ready" || front.plan == nil || front.plan.Pull.Number != 4 {
			t.Fatal("compact stored frame lost look-ahead", front, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hostedComparisonRead(t, f, prior, parent, ComparisonState{Pending: true})
}
