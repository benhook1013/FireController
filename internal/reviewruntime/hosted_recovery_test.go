package reviewruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

type hostedRecoveryReader struct {
	record       HostedCaptureRecord
	calls        []string
	failure      error
	repositoryID int64
}

func (r *hostedRecoveryReader) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	r.calls = append(r.calls, fmt.Sprintf("repository:%d", id))
	if r.failure != nil {
		return reviewprovider.Repository{}, r.failure
	}
	return reviewprovider.Repository{ID: r.repositoryID, FullName: "renamed/project", DefaultBranch: "main"}, nil
}

func (r *hostedRecoveryReader) ObserveHosted(ctx context.Context, repository string, number int64) (reviewprovider.HostedPublicationSnapshot, error) {
	r.calls = append(r.calls, fmt.Sprintf("observe:%s:%d", repository, number))
	snapshot := r.record.Snapshot
	snapshot.ObservedAt = time.Now().UTC()
	return snapshot, nil
}

func recoveryRuntimeFixture(t *testing.T, state string) (*cliEvidenceFixture, *HostedCaptureStore, reviewexec.Attempt, *hostedRecoveryReader) {
	t.Helper()
	f, captures, attempt, record := hostedRetainFixture(t, false)
	var err error
	if err = reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	f.engine, err = reviewexec.New(f.store, []reviewexec.Capability{{ID: "runtime-capability", Provider: "coderabbit", Account: "runtime-account", Channels: []string{"cli", "hosted"}}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	// The source fixture represents a completed provider response. Place its
	// real retained admission before that response, keeping GitHub second precision.
	if _, err = f.store.DB().Exec(`UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND reason='admitted'`, record.EarliestPosting.Format(time.RFC3339Nano), attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.DB().Exec(`UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=1`, record.EarliestPosting.Add(-time.Second).Format(time.RFC3339Nano), attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.DB().Exec(`UPDATE review_operations SET created_at=? WHERE id=?`, record.EarliestPosting.Add(-time.Second).Format(time.RFC3339Nano), attempt.OperationID); err != nil {
		t.Fatal(err)
	}
	if state == "posting" {
		if _, err = f.store.DB().Exec(`DELETE FROM review_execution_transitions WHERE attempt_id=? AND revision>2`, attempt.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.DB().Exec(`UPDATE review_execution_attempts SET state='posting',revision=2,external_id='' WHERE id=?`, attempt.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.DB().Exec(`UPDATE review_operations SET state='posting',revision=3 WHERE id=?`, attempt.OperationID); err != nil {
			t.Fatal(err)
		}
	} else if state == "uncertain" {
		if err = f.engine.RecoverInterrupted(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	bound, err := f.engine.ReadHostedRecovery(t.Context(), f.project, attempt.ID, "runtime-capability")
	if err != nil {
		t.Fatal(err)
	}
	return f, captures, bound.Attempt, &hostedRecoveryReader{record: record, repositoryID: 901}
}

func recoveryRuntimeMetadata(t *testing.T, f *cliEvidenceFixture, attempt reviewexec.Attempt) {
	t.Helper()
	metadata := reviews.PullInput{Number: 2, Title: "Current stored facts", State: "merged", HeadSHA: strings.Repeat("9", 40), BaseRef: "main", BaseSHA: f.baseSHA, CIState: "success", Mergeability: "unknown", Complete: true}
	if _, err := f.engine.PublishAttempt(t.Context(), f.project, attempt.ID, &metadata); err != nil {
		t.Fatal(err)
	}
}

func recoverRuntime(t *testing.T, f *cliEvidenceFixture, captures *HostedCaptureStore, a reviewexec.Attempt, reader *hostedRecoveryReader) (RecoveredHosted, error) {
	t.Helper()
	return RecoverHosted(t.Context(), reader, captures, f.engine, f.project, a.ID, "runtime-capability", reader.record.PostingActor, reader.record.ProviderActor)
}

func recoveryRuntimeCount(t *testing.T, f *cliEvidenceFixture, table string) int {
	t.Helper()
	var n int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRecoverHostedAdoptsAndPublishesWithoutReposting(t *testing.T) {
	for _, state := range []string{"posting", "active", "uncertain"} {
		t.Run(state, func(t *testing.T) {
			f, captures, a, reader := recoveryRuntimeFixture(t, state)
			recoveryRuntimeMetadata(t, f, a)
			got, err := recoverRuntime(t, f, captures, a, reader)
			if err != nil || got.State != "completed" || got.EvidenceID == "" || got.TriggerCommentID != 100 {
				t.Fatalf("recovery: %+v %v", got, err)
			}
			if fmt.Sprint(reader.calls) != "[repository:901 observe:renamed/project:2]" {
				t.Fatal("wrong provider scope", reader.calls)
			}
			if recoveryRuntimeCount(t, f, "review_hosted_observations") != 1 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 {
				t.Fatal("journal/settlement mismatch")
			}
			var title, head, prState, attemptState string
			if err := f.store.DB().QueryRow(`SELECT p.title,p.head_sha,p.state,a.state FROM review_workspace w JOIN review_pulls p ON p.project_id=w.project_id AND p.snapshot_id=w.snapshot_id JOIN review_attempts a ON a.project_id=w.project_id AND a.snapshot_id=w.snapshot_id WHERE w.project_id=?`, f.project).Scan(&title, &head, &prState, &attemptState); err != nil {
				t.Fatal(err)
			}
			if title != "Current stored facts" || head != strings.Repeat("9", 40) || prState != "merged" || attemptState != "completed" {
				t.Fatal("old source overwrote current facts", title, head, prState, attemptState)
			}
			before := recoveryRuntimeCount(t, f, "review_execution_transitions")
			again, err := recoverRuntime(t, f, captures, a, reader)
			if err != nil || again.State != "completed" || again.Reason != "already_completed" || len(reader.calls) != 2 || recoveryRuntimeCount(t, f, "review_execution_transitions") != before {
				t.Fatal("settled replay performed work", again, err, reader.calls)
			}
			state, err := f.engine.ReadReviewAdjudication(t.Context(), f.scope(t, f.writer, false), got.EvidenceID)
			if err != nil {
				t.Fatal(err)
			}
			if state.Finalized {
				t.Fatal("recovery finalized undecided findings")
			}
		})
	}
}

func TestRecoverHostedRetainedEvidenceRepairsPublicationBeforeNewObservation(t *testing.T) {
	f, captures, a, reader := recoveryRuntimeFixture(t, "uncertain")
	recoveryRuntimeMetadata(t, f, a)
	// A genuine non-missing projection failure must retain the final evidence
	// and reservation for a later local retry, even after the exception below.
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_hosted_publication BEFORE INSERT ON review_runs BEGIN SELECT RAISE(ABORT,'synthetic publication failure'); END`); err != nil {
		t.Fatal(err)
	}
	first, err := recoverRuntime(t, f, captures, a, reader)
	if err == nil || first.Reason != "publication_pending" || first.EvidenceID == "" || first.State != "active" {
		t.Fatal("non-missing projection failure was silently settled", first, err)
	}
	if recoveryRuntimeCount(t, f, "review_execution_reservations") != 1 {
		t.Fatal("reservation lost before publication")
	}
	// A fresh engine and reopened store recover possible execution. The final
	// capture, raw reference and reservation must survive, without provider I/O.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = core.Open(t.Context(), f.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	f.engine, err = reviewexec.New(f.store, []reviewexec.Capability{{ID: "runtime-capability", Provider: "coderabbit", Account: "runtime-account", Channels: []string{"cli", "hosted"}}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.engine.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	bound, err := f.engine.ReadHostedRecovery(t.Context(), f.project, a.ID, "runtime-capability")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER fail_hosted_publication`); err != nil {
		t.Fatal(err)
	}
	if bound.EvidenceID != first.EvidenceID {
		t.Fatal("retained final evidence lost on reopen", bound)
	}
	// No provider must be needed after the final capture was committed.
	reader.failure = errors.New("provider offline")
	second, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || second.State != "completed" || second.EvidenceID != first.EvidenceID || len(reader.calls) != 2 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_hosted_observations") != 1 {
		t.Fatal("retained evidence was recollected", second, err, reader.calls)
	}
}

func TestRecoverHostedLaterCompleteObservationDoesNotRepostTrigger(t *testing.T) {
	f, captures, a, reader := recoveryRuntimeFixture(t, "posting")
	recoveryRuntimeMetadata(t, f, a)
	complete := reader.record.Snapshot.Reviews[0].Body
	reader.record.Snapshot.Reviews[0].Body = "**Actionable comments posted: 1**\n\n" + hostedRetainPublicationMarker()
	first, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || first.State != "active" || first.Reason != "comparison_unproven" || first.EvidenceID != "" {
		t.Fatal("incomplete observation was not retained pending", first, err)
	}
	reader.record.Snapshot.Reviews[0].Body = complete
	second, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || second.State != "completed" || second.TriggerCommentID != first.TriggerCommentID || recoveryRuntimeCount(t, f, "review_hosted_observations") != 2 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 {
		t.Fatal("late complete observation failed", second, err)
	}
	var adopted int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_execution_transitions WHERE attempt_id=? AND reason='trigger_recovered'`, a.ID).Scan(&adopted); err != nil || adopted != 1 {
		t.Fatal("trigger adoption was repeated", adopted, err)
	}
}

func TestRecoverHostedWrongCapabilityAndCancellationReadNoProvider(t *testing.T) {
	f, captures, a, reader := recoveryRuntimeFixture(t, "posting")
	_, err := RecoverHosted(t.Context(), reader, captures, f.engine, f.project, a.ID, "missing-capability", reader.record.PostingActor, reader.record.ProviderActor)
	if err == nil || len(reader.calls) != 0 {
		t.Fatal("wrong capability reached provider", err, reader.calls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = RecoverHosted(ctx, reader, captures, f.engine, f.project, a.ID, "runtime-capability", reader.record.PostingActor, reader.record.ProviderActor)
	if err == nil || len(reader.calls) != 0 || recoveryRuntimeCount(t, f, "review_hosted_observations") != 0 {
		t.Fatal("cancellation performed provider/state work", err, reader.calls)
	}
}

func TestRecoverHostedIncompleteOrUnattributedSourceNeverSettles(t *testing.T) {
	for name, change := range map[string]func(*hostedRecoveryReader){
		"partial":         func(r *hostedRecoveryReader) { r.record.Snapshot.Complete = false },
		"missing trigger": func(r *hostedRecoveryReader) { r.record.Snapshot.IssueComments = nil },
		"edited trigger": func(r *hostedRecoveryReader) {
			r.record.Snapshot.IssueComments[0].UpdatedAt = r.record.Snapshot.IssueComments[0].UpdatedAt.Add(time.Second)
		},
		"wrong trigger id": func(r *hostedRecoveryReader) { r.record.Snapshot.IssueComments[0].ID = 999 },
		"missing comparison": func(r *hostedRecoveryReader) {
			r.record.Snapshot.Reviews[0].Body = "**Actionable comments posted: 1**\n\n" + hostedRetainPublicationMarker()
		},
		"wrong repository": func(r *hostedRecoveryReader) { r.repositoryID = 902 },
		"provider failure": func(r *hostedRecoveryReader) { r.failure = errors.New("synthetic offline") },
	} {
		t.Run(name, func(t *testing.T) {
			f, captures, a, reader := recoveryRuntimeFixture(t, "active")
			change(reader)
			got, _ := recoverRuntime(t, f, captures, a, reader)
			if got.State == "completed" || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 1 {
				t.Fatal("unsupported source settled or granted evidence", got)
			}
			want := 1
			if name == "partial" || name == "wrong repository" || name == "provider failure" {
				want = 0
			}
			if recoveryRuntimeCount(t, f, "review_hosted_observations") != want {
				t.Fatal("raw source retention mismatch", got)
			}
		})
	}
}

// Retiring the PR through the ordinary administrator import is a valid current
// observation. It must not turn authoritative retained execution into a second
// provider action or require fabricated browser metadata to release ownership.
func TestRecoverHostedMissingCurrentPullIsLedgerOnly(t *testing.T) {
	for _, settle := range []bool{false, true} {
		for _, retained := range []bool{false, true} {
			t.Run(fmt.Sprintf("settle=%t/retained=%t", settle, retained), func(t *testing.T) {
				f, captures, a, reader := recoveryRuntimeFixture(t, "active")
				recoveryRuntimeMetadata(t, f, a)
				var evidenceID string
				if retained {
					capture, err := RetainHosted(t.Context(), captures, f.engine, a, reader.record)
					if err != nil || !capture.Complete {
						t.Fatal("retain authoritative capture", capture, err)
					}
					evidenceID = capture.Receipt.ID
					reader.failure = errors.New("provider must remain offline")
				}
				var revision int64
				if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, f.project).Scan(&revision); err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 901, Source: "synthetic-retirement", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{}, Retired: []reviews.RetiredPull{{Number: 2, Reason: "defunct"}}})
				if err != nil {
					t.Fatal(err)
				}
				mux := http.NewServeMux()
				reviews.Register(mux, f.store)
				r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", bytes.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+f.admin)
				r.Header.Set("X-FC-Instance", f.store.InstanceID())
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Idempotency-Key", "remove-current-hosted-pull")
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				if w.Code != http.StatusCreated {
					t.Fatalf("administrator retirement: %d %s", w.Code, w.Body.String())
				}
				snapshots := recoveryRuntimeCount(t, f, "review_snapshots")
				got, err := observeHostedAttempt(t.Context(), reader, captures, f.engine, f.project, a.ID, "runtime-capability", reader.record.PostingActor, reader.record.ProviderActor, settle)
				if err != nil || got.State != "completed" || got.EvidenceID == "" || retained && got.EvidenceID != evidenceID {
					t.Fatal("missing current PR prevented proven execution settlement", got, err)
				}
				if retained && len(reader.calls) != 0 {
					t.Fatal("retained final evidence caused provider reads", reader.calls)
				}
				bound, err := f.engine.ReadHostedRecovery(t.Context(), f.project, a.ID, "runtime-capability")
				if err != nil {
					t.Fatal(err)
				}
				if !settle {
					if bound.Attempt.State != "active" || recoveryRuntimeCount(t, f, "review_execution_reservations") != 1 {
						t.Fatal("live observation settled outside the supervisor", bound)
					}
					if _, err := f.engine.Advance(t.Context(), f.project, a.ID, bound.Attempt.Revision, "completed", "synthetic_supervisor_completion", bound.Attempt.ExternalID); err != nil {
						t.Fatal("supervisor could not settle ledger-only result", err)
					}
				}
				bound, err = f.engine.ReadHostedRecovery(t.Context(), f.project, a.ID, "runtime-capability")
				if err != nil || bound.Attempt.State != "completed" || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 {
					t.Fatal("completed execution retained reservation", bound, err)
				}
				adjudication, err := f.engine.ReadReviewAdjudication(t.Context(), f.scope(t, f.writer, false), got.EvidenceID)
				if err != nil || adjudication.Finalized || adjudication.FrozenCounts != nil {
					t.Fatal("execution granted finalization", adjudication, err)
				}
				plan := reviewexec.SourcePlan{Repository: reviewprovider.Repository{ID: 901, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{RepositoryID: 901, Number: 2, HeadSHA: f.headSHA, BaseSHA: f.baseSHA, BaseRef: "main", State: "open", ChangedFiles: 1}, ParentSHA: f.baseSHA, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
				comparison, err := f.engine.Comparison(t.Context(), f.project, plan, "hosted")
				if err != nil || comparison.Covered || !comparison.Pending {
					t.Fatal("unadjudicated ledger-only execution granted coverage", comparison, err)
				}
				var currentPulls int
				if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_workspace w JOIN review_pulls p ON p.project_id=w.project_id AND p.snapshot_id=w.snapshot_id WHERE w.project_id=?`, f.project).Scan(&currentPulls); err != nil {
					t.Fatal(err)
				}
				if currentPulls != 0 || recoveryRuntimeCount(t, f, "review_snapshots") != snapshots || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 {
					t.Fatal("ledger-only finish invented metadata/snapshot or duplicate evidence")
				}
			})
		}
	}
}
