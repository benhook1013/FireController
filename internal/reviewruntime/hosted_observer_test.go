package reviewruntime

import (
	"context"
	"reflect"
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
	result, err := f.hosted.Dispatch(t.Context(), op, a, f.callback(t, &a))
	if err != nil || result.State != "completed" {
		t.Fatal(result, err)
	}
	live := readHostedObserver(t, f, a)
	if live.State != "waiting" || live.LastCheckAt == "" || live.LastSourceAt == "" || live.Error != "" {
		t.Fatal("immediate pass omitted source freshness", live)
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
	if offline.State != "waiting" || offline.LastCheckAt == "" || offline.LastSourceAt != "" || f.provider.observations != calls {
		t.Fatal("offline settlement fabricated source freshness", offline)
	}
}
