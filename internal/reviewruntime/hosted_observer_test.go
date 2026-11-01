package reviewruntime

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func readHostedObserver(t *testing.T, f *hostedDriverFixture, a reviewexec.Attempt) reviewexec.HostedObserverStatus {
	t.Helper()
	status, err := f.engine.ReadOperationStatus(t.Context(), f.scopeWithAccess(t, f.readerToken(), false, "read"), a.OperationID)
	if err != nil || status.Observer == nil {
		t.Fatal("missing pending Hosted observer", status, err)
	}
	return *status.Observer
}

func TestHostedObserverScheduledFreshnessDedupFailureAndStop(t *testing.T) {
	f, a, p := schedulerUncertainFixture(t)
	if got := readHostedObserver(t, f, a); !reflect.DeepEqual(got, reviewexec.HostedObserverStatus{State: "unknown"}) {
		t.Fatal("old durable capture invented observer health", got)
	}
	f.provider.partial = true
	var providerTime, freshSource time.Time
	p.alterSnapshot = func(s *reviewprovider.HostedPublicationSnapshot) {
		if providerTime.IsZero() {
			providerTime = s.Reviews[0].SubmittedAt
		}
		s.Reviews[0].SubmittedAt = providerTime
		s.ReviewComments[0].CreatedAt = providerTime
		s.ReviewComments[0].UpdatedAt = providerTime
		freshSource = s.ObservedAt
	}
	gate := newSchedulerGate(f.hosted)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gate.interval(t, 30*time.Second)
	first := readHostedObserver(t, f, a)
	if first.State != "waiting" || first.LastCheckAt == "" || first.LastSourceAt != freshSource.Format(time.RFC3339Nano) || first.NextCheckAt == "" || first.Error != "" || first.Reason != "publication_not_proven" {
		t.Fatal(first)
	}
	before, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	files := hostedCapacityArtifacts(t, f.hostedCaptures)
	events := cliCount(t, f.store, `SELECT count(*) FROM events`)
	gate.resume(t)
	gate.interval(t, 30*time.Second)
	after, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
	second := readHostedObserver(t, f, a)
	if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(files, hostedCapacityArtifacts(t, f.hostedCaptures)) || cliCount(t, f.store, `SELECT count(*) FROM events`) != events {
		t.Fatal("observer freshness changed immutable source/journal", err)
	}
	if second.LastCheckAt == first.LastCheckAt || second.LastSourceAt == first.LastSourceAt || second.LastSourceAt != freshSource.Format(time.RFC3339Nano) {
		t.Fatal("dedup hid successful fresh poll", first, second)
	}
	// The source read is complete even though edited prior human comments close
	// attribution. This must not imply that the provider started or completed.
	p.alterSnapshot = func(s *reviewprovider.HostedPublicationSnapshot) {
		for i := range s.IssueComments {
			if s.IssueComments[i].ID == 100 {
				s.IssueComments[i].UpdatedAt = s.ObservedAt
			}
		}
		freshSource = s.ObservedAt
	}
	gate.resume(t)
	gate.interval(t, 30*time.Second)
	attributed := readHostedObserver(t, f, a)
	if attributed.State != "waiting" || attributed.Reason != "attribution_window_closed" || attributed.Error != "" || attributed.LastSourceAt != freshSource.Format(time.RFC3339Nano) {
		t.Fatal("semantic uncertainty reported as source failure", attributed)
	}
	// A nil error with an incomplete collection remains a diagnostic error even
	// when the scheduler's outer pass closes after the inner observation pass.
	p.alterSnapshot = func(s *reviewprovider.HostedPublicationSnapshot) { s.Complete = false }
	gate.resume(t)
	gate.interval(t, 30*time.Second)
	incomplete := readHostedObserver(t, f, a)
	if incomplete.State != "retrying" || incomplete.Error != "provider_invalid_response" || incomplete.Reason != "observation_incomplete" || incomplete.LastSourceAt != attributed.LastSourceAt {
		t.Fatal("outer scheduler erased incomplete-collection error", incomplete)
	}
	p.alterSnapshot = nil
	p.repositoryError = &reviewprovider.Error{Code: "provider_rate_limited", Status: 429, RetryAfterSeconds: 120}
	gate.resume(t)
	gate.interval(t, 120*time.Second)
	failed := readHostedObserver(t, f, a)
	if failed.State != "retrying" || failed.Error != "provider_rate_limited" || failed.LastSourceAt != attributed.LastSourceAt || failed.LastCheckAt == incomplete.LastCheckAt || failed.NextCheckAt == "" {
		t.Fatal(failed)
	}
	// Invalid fresh timing fails capture validation before source freshness can
	// be claimed, even if the payload is otherwise a complete collection.
	p.repositoryError = nil
	p.alterSnapshot = func(s *reviewprovider.HostedPublicationSnapshot) {
		s.ObservedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	gate.resume(t)
	gate.interval(t, 60*time.Second)
	invalid := readHostedObserver(t, f, a)
	if invalid.State != "retrying" || invalid.Error != "observer_unavailable" || invalid.LastSourceAt != attributed.LastSourceAt {
		t.Fatal("invalid clock claimed a successful source read", invalid)
	}
	p.alterSnapshot = nil
	p.repositoryStarted = make(chan struct{})
	gate.resume(t)
	select {
	case <-p.repositoryStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not enter live check")
	}
	checking := readHostedObserver(t, f, a)
	if checking.State != "checking" || checking.NextCheckAt != "" || checking.LastSourceAt != attributed.LastSourceAt {
		t.Fatal(checking)
	}
	cancel()
	schedulerDone(t, done, false)
	stopped := readHostedObserver(t, f, a)
	if stopped.State != "stopped" || stopped.NextCheckAt != "" || stopped.LastSourceAt != attributed.LastSourceAt {
		t.Fatal(stopped)
	}
	counts := [3]int64{p.repositories.Load(), int64(f.provider.observations), int64(cliCount(t, f.store, `SELECT count(*) FROM events`))}
	_ = readHostedObserver(t, f, a)
	if counts != [3]int64{p.repositories.Load(), int64(f.provider.observations), int64(cliCount(t, f.store, `SELECT count(*) FROM events`))} {
		t.Fatal("status read performed provider/journal work")
	}
	if f.provider.queries != 1 || f.provider.reviews != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
		t.Fatal("diagnostics changed admission or accounting")
	}
}

func TestHostedObserverImmediatePassAndOfflineRestart(t *testing.T) {
	f := newHostedDriverFixture(t)
	op, a := f.prepareHosted(t)
	f.provider.partial = true
	result, err := f.hosted.Dispatch(t.Context(), op, a, f.callback(t, &a))
	if err != nil || result.State != "uncertain" {
		t.Fatal(result, err)
	}
	live := readHostedObserver(t, f, a)
	if live.State != "waiting" || live.LastCheckAt == "" || live.LastSourceAt == "" || live.Error != "" {
		t.Fatal("immediate pass omitted source freshness", live)
	}
	f.provider.partial = false
	completed, err := observeHostedAttempt(t.Context(), f.provider, f.hostedCaptures, f.engine, f.project, a.ID, f.capability.ID, f.provider.actor, f.provider.bot, false)
	if err != nil || completed.State != "completed" {
		t.Fatal(completed, err)
	}
	// The supervisor still owns the active journal, but completed collection has
	// no ongoing observation intent and must not consume a health slot forever.
	if got := readHostedObserver(t, f, a); !reflect.DeepEqual(got, reviewexec.HostedObserverStatus{State: "unknown"}) {
		t.Fatal("ended live collection retained a health slot", got)
	}
	// Durable evidence must not seed observer health in a fresh process.
	f.engine, err = reviewexec.New(f.store, []reviewexec.Capability{{ID: f.capability.ID, Provider: "coderabbit", Account: f.capability.Account, Channels: f.capability.Channels, Fingerprint: f.capability.Fingerprint}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	if got := readHostedObserver(t, f, a); !reflect.DeepEqual(got, reviewexec.HostedObserverStatus{State: "unknown"}) {
		t.Fatal("restart fabricated runtime activity", got)
	}
	calls := f.provider.observations
	resultObserved, err := observeHostedAttempt(t.Context(), f.provider, f.hostedCaptures, f.engine, f.project, a.ID, f.capability.ID, f.provider.actor, f.provider.bot, false)
	if err != nil || resultObserved.State != "completed" {
		t.Fatal(resultObserved, err)
	}
	offline := readHostedObserver(t, f, a)
	if !reflect.DeepEqual(offline, reviewexec.HostedObserverStatus{State: "unknown"}) || f.provider.observations != calls {
		t.Fatal("offline completion retained health or fabricated source freshness", offline)
	}
}

func TestHostedObserverRejectionCleanupRetainsFailedSettlement(t *testing.T) {
	f, captures, attempt, reader := recoveryRuntimeFixture(t, "active")
	trigger := reader.record.Snapshot.IssueComments[0]
	if trigger.ID != 100 {
		t.Fatal("fixture trigger changed")
	}
	at := time.Now().UTC()
	reader.record.Snapshot.Reviews = []reviewprovider.HostedReview{}
	reader.record.Snapshot.ReviewComments = []reviewprovider.ReviewComment{}
	reader.record.Snapshot.IssueComments = []reviewprovider.IssueComment{trigger, {
		ID: 5002, Author: reader.record.ProviderActor, CreatedAt: at, UpdatedAt: at,
		Body: "<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("c", 64) + " -->\n<details>\n<summary>⚠️ Action not completed</summary>\n<dl>\n<dd>\nReview rate limited.\n</dd>\n</dl>\n</details>",
	}}
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_rate_limit_settlement BEFORE UPDATE ON review_execution_attempts WHEN NEW.state='rate_limited' BEGIN SELECT RAISE(ABORT,'synthetic settlement refusal'); END`); err != nil {
		t.Fatal(err)
	}
	failed, err := recoverRuntime(t, f, captures, attempt, reader)
	if err == nil || failed.State != "rate_limited" {
		t.Fatal("fixture did not fail terminal settlement", failed, err)
	}
	status, err := f.engine.ReadOperationStatus(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), attempt.OperationID)
	if err != nil || status.State != "active" || status.Observer == nil || status.Observer.State != "retrying" || status.Observer.Error == "" {
		t.Fatal("failed settlement erased live retry diagnostics", status, err)
	}
	// The live path leaves journal settlement to its supervisor. Successful
	// terminal collection nevertheless frees its process-local health entry.
	ended, err := observeHostedAttempt(t.Context(), reader, captures, f.engine, f.project, attempt.ID, "runtime-capability", reader.record.PostingActor, reader.record.ProviderActor, false)
	if err != nil || ended.State != "rate_limited" {
		t.Fatal(ended, err)
	}
	status, err = f.engine.ReadOperationStatus(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), attempt.OperationID)
	if err != nil || status.State != "active" || status.Observer == nil || !reflect.DeepEqual(*status.Observer, reviewexec.HostedObserverStatus{State: "unknown"}) || recoveryRuntimeCount(t, f, "review_execution_reservations") != 1 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 {
		t.Fatal("ended rejection retained health or changed native accounting", status, err)
	}
}
