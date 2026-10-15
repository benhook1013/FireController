package reviewruntime

import (
	"context"
	"errors"
	"fmt"
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

func TestRecoverHostedProviderClockSkewRetainsRealTimestamp(t *testing.T) {
	for _, seconds := range []int{1, 5, 6} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			f, captures, a, reader := recoveryRuntimeFixture(t, "uncertain")
			recoveryRuntimeMetadata(t, f, a)
			triggerAt := reader.record.Snapshot.IssueComments[0].CreatedAt
			admitted := triggerAt.Add(time.Duration(seconds) * time.Second)
			if _, err := f.store.DB().Exec(`UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND reason='admitted'`, admitted.Format(time.RFC3339Nano), a.ID); err != nil {
				t.Fatal(err)
			}
			got, err := recoverRuntime(t, f, captures, a, reader)
			if err != nil {
				t.Fatal(err)
			}
			if seconds > 5 {
				if got.State != "uncertain" || got.EvidenceID != "" || recoveryRuntimeCount(t, f, "review_execution_reservations") != 1 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 {
					t.Fatalf("excessive skew settled uncertain action: %+v", got)
				}
				return
			}
			if got.State != "completed" || got.TriggerCommentID != 100 || got.EvidenceID == "" {
				t.Fatalf("bounded provider lag not recovered: %+v", got)
			}
			evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scope(t, f.writer, false), got.EvidenceID)
			if err != nil || evidence.Capture.StartedAt != triggerAt.Format(time.RFC3339Nano) {
				t.Fatal("provider timestamp was relabeled", err)
			}
			calls := len(reader.calls)
			if again, err := recoverRuntime(t, f, captures, a, reader); err != nil || again.State != "completed" || len(reader.calls) != calls {
				t.Fatal("settled skew recovery observed provider again", again, err)
			}
		})
	}
}

func TestRecoverHostedRetainedEvidenceRepairsPublicationBeforeNewObservation(t *testing.T) {
	f, captures, a, reader := recoveryRuntimeFixture(t, "uncertain")
	first, err := recoverRuntime(t, f, captures, a, reader)
	if err == nil || first.Reason != "publication_pending" || first.EvidenceID == "" || first.State != "active" {
		t.Fatal("missing projection was silently settled", first, err)
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
	recoveryRuntimeMetadata(t, f, bound.Attempt)
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
