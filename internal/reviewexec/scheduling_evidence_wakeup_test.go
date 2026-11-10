package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

// The proof can commit after chooseRun's read transaction but before its final
// write. Exercise that exact interleaving without timers or provider traffic.
func TestSchedulingEvidenceProofBetweenChoiceAndSettlement(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, scenario := range []struct {
			name                  string
			proofBeforeSettlement bool
			precedingDraft        bool
		}{
			{name: "unchanged-hold"},
			{name: "proof-before-settlement", proofBeforeSettlement: true},
			{name: "draft-masks-unchanged-hold", precedingDraft: true},
			{name: "draft-masks-proof-before-settlement", proofBeforeSettlement: true, precedingDraft: true},
		} {
			t.Run(channel+"/"+scenario.name, func(t *testing.T) {
				proofBeforeSettlement := scenario.proofBeforeSettlement
				queue, heldReason := []int64{2}, "review_evidence_pending"
				if scenario.precedingDraft {
					queue, heldReason = []int64{1, 2}, "pull_draft"
				}
				var f *attemptFixture
				var a Attempt
				var evidence string
				var finalRevision int64
				if channel == "cli" {
					f, a, evidence, _ = coverageFixture(t, 1, false)
					adjudicationDecide(t, f, evidence, "accept", 0, 0, "accepted")
					coverageFinalize(t, f, a, evidence, 1)
					finalRevision = 2
				} else {
					f, a, evidence, _ = hostedComparisonFixture(t, false)
					adjudicationDecide(t, f, evidence, "accept", 0, 0, "accepted")
					adjudicationDecide(t, f, evidence, "reject-one", 1, 1, "rejected")
					adjudicationDecide(t, f, evidence, "reject-two", 2, 2, "rejected")
					coverageFinalize(t, f, a, evidence, 3)
					finalRevision = 4
				}
				admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, attemptProject)
				admissionSQL(t, f, `UPDATE grants SET can_manage=1 WHERE credential_id='exec-writer-cred'`)
				one := int64(1)
				_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "only-pending-pull", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "independent", Queue: queue, Channels: []string{"cli", "hosted"}})
				if err != nil {
					t.Fatal(err)
				}
				roundGrant(t, f, 2, channel, 0, 2, 3, "more-review-after-fix")
				generate := func(want bool) {
					if channel == "cli" {
						schedulingCLIGenerate(t, f, want)
					} else {
						schedulingGenerate(t, f, want)
					}
				}
				if channel == "cli" {
					schedulingCLIEnable(t, f, 0, true)
				} else {
					schedulingEnable(t, f, 0, true)
				}
				generate(true)
				r := schedulingLatest(t, f)
				var configuration Configuration
				var repo int64
				var pins map[int64]reviews.SourceMetadataPin
				// Claim just as runNext does, then stop at the real choose/write
				// boundary to admit the concurrent finding proof deterministically.
				err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					var reason string
					configuration, repo, reason, err = f.e.runBinding(ctx, tx, r)
					if err != nil {
						return err
					}
					if reason != "" {
						return fmt.Errorf("unexpected fixture binding: %s", reason)
					}
					if f.e.publishAttempt != nil {
						pins, err = reviews.PinSourceMetadata(ctx, tx, r.ProjectID, configuration.Queue)
						if err != nil {
							return err
						}
					}
					return updateRun(ctx, tx, r, "selecting", "", "")
				})
				if err != nil {
					t.Fatal(err)
				}
				r = selectionRead(t, f, r.ID)
				reader := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
					p := reviewprovider.Pull{RepositoryID: 801, Number: n, State: "open", HeadSHA: strings.Repeat("f", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}
					if scenario.precedingDraft && n == 1 {
						draft := true
						p.IsDraft = &draft
					}
					return p, nil
				}}
				choice, err := f.e.chooseRun(t.Context(), r, repo, configuration, reader)
				if err != nil || choice.state != "held" || choice.reason != heldReason {
					t.Fatalf("expected genuine pending-evidence choice, got %+v: %v", choice, err)
				}
				if proofBeforeSettlement {
					coverageFix(t, f, evidence, "concurrent-fix-proof", finalRevision, 0, "")
				}
				if worked, err := f.e.finishRunChoice(t.Context(), r, repo, pins, choice); err != nil || !worked {
					t.Fatal(worked, err)
				}
				settled := selectionRead(t, f, r.ID)
				var basis, settledBasis string
				if err := f.store.DB().QueryRow(`SELECT basis_sha256,settled_basis_sha256 FROM review_automatic_selections WHERE selection_id=?`, r.ID).Scan(&basis, &settledBasis); err != nil {
					t.Fatal(err)
				}
				if proofBeforeSettlement {
					if settled.State != "held" || settled.Reason != "selection_changed" || settled.OperationID != "" || settledBasis != "" {
						t.Fatalf("stale evidence hold consumed the new eligible basis: selection=%+v settled_basis=%q", settled, settledBasis)
					}
					var currentBasis string
					err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
						var err error
						currentBasis, _, err = automaticPlanningBasisChannel(ctx, tx, r.ProjectID, configuration, r.ID, channel)
						return err
					})
					if err != nil || currentBasis == basis || currentBasis == settledBasis {
						t.Fatal("new evidence basis was already consumed", currentBasis, basis, settledBasis, err)
					}
				} else {
					if settled.Reason != heldReason || settledBasis == "" {
						t.Fatal("unchanged hold must remain settled", settled, settledBasis)
					}
					generate(false)
					coverageFix(t, f, evidence, "later-fix-proof", finalRevision, 0, "")
				}
				// A normal coordinator pass, without unrelated PR changes, observes
				// the proof and performs a fresh source pass before one child.
				generate(true)
				fresh := schedulingLatest(t, f)
				if fresh.ID == r.ID {
					t.Fatal("expected a fresh ordinary intent")
				}
				selectionNext(t, f, reader)
				fresh = schedulingLatest(t, f)
				if fresh.State != "selected" || fresh.OperationID == "" || reader.calls.Load() != int64(2*(1+len(queue))) {
					t.Fatal("did not dispatch through one fresh source pass", fresh, reader.calls.Load())
				}
				op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), fresh.OperationID)
				if err != nil || op.PullNumber != 2 || op.HeadSHA != strings.Repeat("f", 40) || op.Channel != channel {
					t.Fatal(op, err)
				}
				generate(false)
				if n := attemptCount(t, f.store, `SELECT count(*) FROM review_operations`); n != 2 {
					t.Fatal("duplicate child operation", n)
				}
				// Cancellation remains terminal for this intent, including restart.
				_, err = f.e.ControlOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "cancel-new-intent", fresh.OperationID, OperationControl{ExpectedRevision: 1, Action: "cancel"})
				if err != nil {
					t.Fatal(err)
				}
				generate(false)
				f.e, err = New(f.store, attemptCapabilities)
				if err != nil {
					t.Fatal(err)
				}
				if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
					t.Fatal(err)
				}
				generate(false)
			})
		}
	}
}
