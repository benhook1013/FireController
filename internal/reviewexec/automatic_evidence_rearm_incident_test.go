package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
	"strings"
	"testing"
)

func TestAutomaticSelectionEvidenceMovementRearmsWithoutUnrelatedEvent(t *testing.T) {
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
				one := int64(1)
				_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "only-pending-pull", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "independent", Queue: queue, Channels: []string{"cli", "hosted"}})
				if err != nil {
					t.Fatal(err)
				}
				roundGrant(t, f, 2, channel, 0, 2, 3, "more-review-after-fix")
				admissionSQL(t, f, `UPDATE grants SET can_manage=1 WHERE credential_id='exec-writer-cred'`)
				zero := int64(0)
				enabled := true
				_, err = f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "automatic-rearm", SchedulingRequest{ExpectedRevision: &zero, AutomaticCLI: &enabled, AutomaticHosted: &enabled})
				if err != nil {
					t.Fatal(err)
				}
				generate := func() bool {
					var worked bool
					var err error
					readers := map[string]TargetReader{"test-capability": &refreshTestReader{}}
					if channel == "cli" {
						worked, err = f.e.generateAutomaticCLI(t.Context(), readers)
					} else {
						worked, err = f.e.generateAutomaticHosted(t.Context(), readers)
					}
					if err != nil {
						t.Fatal(err)
					}
					return worked
				}
				if !generate() {
					t.Fatal("initial automatic selection absent")
				}
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
				choiceReads := reader.calls.Load()
				if proofBeforeSettlement {
					coverageFix(t, f, evidence, "concurrent-fix-proof", finalRevision, 0, "")
				}
				if worked, err := f.e.finishRunChoice(t.Context(), r, repo, pins, choice); err != nil || !worked {
					t.Fatal(worked, err)
				}
				if reader.calls.Load() != choiceReads {
					t.Fatal("local settlement performed provider reads")
				}
				settled := selectionRead(t, f, r.ID)
				wantReason := heldReason
				if proofBeforeSettlement {
					wantReason = "selection_changed"
				}
				if settled.State != "held" || settled.Reason != wantReason || settled.OperationID != "" {
					t.Fatalf("stale local choice settled incorrectly: %+v, want reason %s", settled, wantReason)
				}
				if n := attemptCount(t, f.store, `SELECT count(*) FROM review_operations`); n != 1 {
					t.Fatal("settlement admitted an unverified child", n)
				}
				// An explicit terminal selection never resumes itself, even after
				// restart. A new start must reacquire ordinary provider facts.
				f.e, err = New(f.store, attemptCapabilities)
				if err != nil {
					t.Fatal(err)
				}
				if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
					t.Fatal(err)
				}
				if worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": reader}); err != nil || worked {
					t.Fatal("terminal selection resumed", worked, err)
				}
				if !proofBeforeSettlement {
					coverageFix(t, f, evidence, "later-fix-proof", finalRevision, 0, "")
				}
				if !generate() {
					t.Fatal("evidence movement did not rearm without unrelated event")
				}
				freshRun := schedulingLatest(t, f)
				if freshRun.ID == r.ID {
					t.Fatal("reused terminal selection")
				}
				selectionNext(t, f, reader)
				fresh := selectionRead(t, f, freshRun.ID)
				if fresh.State != "selected" || fresh.OperationID == "" || reader.calls.Load() != 2*choiceReads {
					t.Fatal("did not dispatch through one fresh source pass", fresh, reader.calls.Load())
				}
				op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), fresh.OperationID)
				if err != nil || op.PullNumber != 2 || op.HeadSHA != strings.Repeat("f", 40) || op.Channel != channel {
					t.Fatal(op, err)
				}
				if n := attemptCount(t, f.store, `SELECT count(*) FROM review_operations`); n != 2 {
					t.Fatal("duplicate child operation", n)
				}
			})
		}
	}
}
