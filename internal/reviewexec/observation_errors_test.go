package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestExternalNativeReadFailures(t *testing.T) {
	for _, mode := range []string{"attempt-corrupt", "evidence-corrupt", "attempt-missing", "evidence-missing", "cancelled", "storage-fault"} {
		t.Run(mode, func(t *testing.T) {
			f, a, in := hostedPublicationFixture(t, true)
			if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in); err != nil {
				t.Fatal(err)
			}
			admissionSQL(t, f, `UPDATE review_execution_attempts SET state='completed' WHERE id=?`, a.ID)
			switch mode {
			case "attempt-corrupt":
				admissionSQL(t, f, `UPDATE review_execution_attempts SET anchor_json='{' WHERE id=?`, a.ID)
			case "evidence-corrupt":
				admissionSQL(t, f, `UPDATE review_execution_evidence SET body_json=json_set(body_json,'$.capture.capture_sha256','invalid') WHERE attempt_id=?`, a.ID)
			case "attempt-missing":
				admissionSQL(t, f, `UPDATE review_execution_attempts SET project_id=? WHERE id=?`, otherAttemptProject, a.ID)
			case "evidence-missing":
				admissionSQL(t, f, `DELETE FROM review_execution_evidence WHERE attempt_id=?`, a.ID)
			}
			run := reviewprovider.ExternalHostedRun{HeadSHA: a.Anchor.CandidateSHA, NativeOperationID: a.OperationID, TriggerCommentID: 101, Inventory: reviewprovider.HostedFindingInventory{Publication: reviewprovider.HostedPublication{PublicationID: in.Hosted.PublicationID}}}
			err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				if mode == "cancelled" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				if mode == "storage-fault" {
					if err := tx.Rollback(); err != nil {
						t.Fatal(err)
					}
				}
				target := lifecycleTarget{project: attemptProject, repository: a.Anchor.RepositoryID, number: 2}
				owned, held, err := externalHostedNativeBinding(ctx, tx, target, run)
				missing := strings.HasSuffix(mode, "missing")
				if missing {
					if err != nil || owned || !held {
						t.Fatalf("missing binding: owned=%t held=%t err=%v", owned, held, err)
					}
				} else if err == nil || owned || held {
					t.Fatalf("failure hidden by binding: owned=%t held=%t err=%v", owned, held, err)
				}
				if mode == "cancelled" && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
				captures, held, err := externalNativeCaptures(ctx, tx, target, run)
				if missing {
					if err != nil || held != (mode == "attempt-missing") || len(captures) != 0 {
						t.Fatalf("missing capture: %v %t %v", captures, held, err)
					}
				} else if err == nil || held || len(captures) != 0 {
					t.Fatalf("failure hidden by capture: %v %t %v", captures, held, err)
				}
				if mode == "attempt-corrupt" || mode == "evidence-corrupt" {
					var failure *core.Error
					if !errors.As(err, &failure) || failure.Code != "storage_unavailable" {
						t.Fatalf("lost storage failure: %v", err)
					}
				}
				if mode == "storage-fault" {
					return sql.ErrTxDone
				}
				return nil
			})
			if mode != "storage-fault" && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLifecycleLocalValidationDoesNotCoolAccount(t *testing.T) {
	for _, mode := range []string{"repository-id", "repository-name", "pull-id", "head", "state", "merged-open", "changed-files", "title", "outage"} {
		for _, lane := range []lifecycleLane{lifecycleMetadata, lifecycleEnrichment} {
			t.Run(fmt.Sprintf("%s/lane-%d", mode, lane), func(t *testing.T) {
				f, s, clock := lifecycleFixture(t)
				lifecycleProject(t, f, otherAttemptProject, "test-capability", 802)
				r := externalReader(externalSnapshot(0, strings.Repeat("a", 40)))
				var repositoryID int64
				r.repository = func(_ context.Context, id int64) (reviewprovider.Repository, error) {
					repositoryID = id
					repo := reviewprovider.Repository{ID: id, FullName: "owner/repo", DefaultBranch: "main"}
					if id == 801 {
						switch mode {
						case "repository-id":
							repo.ID++
						case "repository-name":
							repo.FullName = "invalid"
						case "outage":
							return repo, &reviewprovider.Error{Code: "provider_unavailable"}
						}
					}
					return repo, nil
				}
				r.pull = func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
					p := lifecyclePull(n)
					p.RepositoryID = repositoryID
					if repositoryID == 801 {
						switch mode {
						case "pull-id":
							p.Number++
						case "head":
							p.HeadSHA = "invalid"
						case "state":
							p.State = "invalid"
						case "merged-open":
							p.Merged = true
						case "changed-files":
							p.ChangedFiles = -1
						case "title":
							p.Title = " "
						}
					}
					return p, nil
				}
				readers := map[string]TargetReader{"test-capability": r}
				for round := 0; round < 2; round++ {
					target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane)
					if err != nil || target == nil || target.project != attemptProject {
						t.Fatalf("first target: %v %v", target, err)
					}
					if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
						t.Fatal(err)
					}
					for _, account := range s.accounts {
						if mode == "outage" {
							if account.backoff <= 0 || !account.until.After(clock.Now()) {
								t.Fatal("outage lost account backoff")
							}
						} else if account.backoff != 0 || !account.until.IsZero() {
							t.Fatal("invalid target cooled account")
						}
					}
					next, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane)
					if err != nil {
						t.Fatal(err)
					}
					if mode == "outage" {
						if next != nil {
							t.Fatal("outage did not block shared account")
						}
						return
					}
					if next == nil || next.project != otherAttemptProject {
						t.Fatalf("unrelated project starved: %v", next)
					}
					if err = f.e.observeLifecycle(t.Context(), *next, s); err != nil {
						t.Fatal(err)
					}
					clock.Add(59 * time.Second)
					if early, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane); err != nil || early != nil {
						t.Fatal("target retried early", early, err)
					}
					clock.Add(time.Second)
				}
			})
		}
	}
}
