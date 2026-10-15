package reviewruntime

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// A gate publishes the completed pass before the test inspects/mutates fixture
// facts. No real 15-second timer or unbounded busy loop is needed.
type schedulerGate struct {
	calls chan time.Duration
	next  chan struct{}
}

func newSchedulerGate(d *HostedDriver) *schedulerGate {
	gate := &schedulerGate{calls: make(chan time.Duration), next: make(chan struct{})}
	d.wait = func(ctx context.Context, delay time.Duration) error {
		select {
		case gate.calls <- delay:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-gate.next:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return gate
}
func (g *schedulerGate) interval(t *testing.T, want time.Duration) {
	t.Helper()
	select {
	case delay := <-g.calls:
		if delay != want {
			t.Fatalf("recovery interval: got %v want %v", delay, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recovery did not reach bounded wait")
	}
}
func (g *schedulerGate) resume(t *testing.T) {
	t.Helper()
	select {
	case g.next <- struct{}{}:
	case <-time.After(3 * time.Second):
		t.Fatal("recovery wait did not resume")
	}
}
func schedulerDone(t *testing.T, done <-chan error, wantError bool) {
	t.Helper()
	select {
	case err, ok := <-done:
		if !ok || (err != nil) != wantError {
			t.Fatal("recovery terminal result", err, ok)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recovery did not stop")
	}
	select {
	case _, ok := <-done:
		if ok {
			t.Fatal("multiple terminal recovery results")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recovery result channel did not close")
	}
}

type schedulerProvider struct {
	*driverHostedProvider
	repositoryError   error
	alterSnapshot     func(*reviewprovider.HostedPublicationSnapshot)
	repositoryStarted chan struct{}
	repositories      atomic.Int64
	currentActors     atomic.Int64
}

func (p *schedulerProvider) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	p.repositories.Add(1)
	if p.repositoryStarted != nil {
		close(p.repositoryStarted)
		<-ctx.Done()
		return reviewprovider.Repository{}, ctx.Err()
	}
	if p.repositoryError != nil {
		return reviewprovider.Repository{}, p.repositoryError
	}
	return p.cliDriverReader.Repository(ctx, id)
}
func (p *schedulerProvider) ObserveHosted(ctx context.Context, repo string, number int64) (reviewprovider.HostedPublicationSnapshot, error) {
	snapshot, err := p.driverHostedProvider.ObserveHosted(ctx, repo, number)
	if err == nil && p.alterSnapshot != nil {
		p.alterSnapshot(&snapshot)
	}
	return snapshot, err
}
func (p *schedulerProvider) CurrentActor(context.Context) (reviewprovider.Author, error) {
	p.currentActors.Add(1)
	return reviewprovider.Author{ID: 999, Login: "replacement-actor", Kind: "User"}, nil
}

func schedulerUncertainFixture(t *testing.T) (*hostedDriverFixture, reviewexec.Attempt, *schedulerProvider) {
	t.Helper()
	f := newHostedDriverFixture(t)
	op, a := f.prepareHosted(t)
	f.provider.lost = true
	result, err := f.hosted.Dispatch(t.Context(), op, a, f.callback(t, &a))
	if err != nil || result.State != "uncertain" || result.Reason != "trigger_post_unproven" || f.provider.reviews != 1 || hostedDriverJournal(t, f, a.ID) != "posting" {
		t.Fatal("fixture did not lose its one posted review response", result, err)
	}
	if err = f.engine.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	bound, err := f.engine.ReadHostedRecovery(t.Context(), f.project, a.ID, f.capability.ID)
	if err != nil || bound.Attempt.State != "uncertain" {
		t.Fatal("fixture was not recovered uncertain", err)
	}
	p := &schedulerProvider{driverHostedProvider: f.provider}
	f.hosted.provider = p
	return f, bound.Attempt, p
}

func TestHostedSchedulerAdoptsPendingThenCompletesWithOriginalActor(t *testing.T) {
	f, a, p := schedulerUncertainFixture(t)
	// A paused/reconfigured project and revoked initiating actor cannot erase an
	// already issued review. Recovery must use original query identity, not /user.
	one := int64(1)
	if _, err := f.engine.ConfigurePolicy(t.Context(), f.scope(t, f.admin, true), "scheduler-pause-policy", reviewexec.PolicyConfiguration{ExpectedRevision: &one, RequiredChannels: []string{"hosted"}, PausedChannels: []string{"hosted"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Configure(t.Context(), f.scope(t, f.admin, true), "scheduler-pause-config", reviewexec.Configuration{ExpectedRevision: &one, CapabilityID: f.capability.ID, Mode: "independent", Queue: []int64{2}, Channels: []string{"hosted"}, Paused: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='runtime-evidence-writer'`); err != nil {
		t.Fatal(err)
	}
	original, err := f.engine.ReadHostedAllowanceQuery(t.Context(), f.project, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.provider.actor = reviewprovider.Author{ID: 999, Login: "replacement-actor", Kind: "User"}
	f.provider.partial = true
	gate := newSchedulerGate(f.hosted)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gate.interval(t, 15*time.Second)
	if hostedDriverJournal(t, f, a.ID) != "active" || f.provider.observations != 1 || p.currentActors.Load() != 0 || p.repositories.Load() != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
		t.Fatal("pending adoption lost observation ownership/reservation")
	}
	candidate, err := f.engine.PendingHostedRecovery(t.Context(), f.capability.ID)
	if err != nil || candidate == nil || candidate.AttemptID != a.ID {
		t.Fatal("adopted active work disappeared from recovery candidates", err)
	}
	duplicate, err := f.hosted.StartRecovery(ctx)
	assertCoreErrorCode(t, err, "validation_failed")
	if duplicate != nil {
		t.Fatal("duplicate scheduler returned another worker")
	}
	f.provider.partial = false
	gate.resume(t)
	gate.interval(t, 15*time.Second)
	if hostedDriverJournal(t, f, a.ID) != "completed" || f.provider.observations != 2 || p.currentActors.Load() != 0 || p.repositories.Load() != 2 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || f.provider.queries != 1 || f.provider.reviews != 1 {
		t.Fatal("recovered active review did not complete exactly once")
	}
	retained, err := f.engine.ReadHostedAllowanceQuery(t.Context(), f.project, a.ID)
	if err != nil || retained.PostingActor != original.PostingActor || retained.Intent != original.Intent || retained.ID != original.ID {
		t.Fatal("recovery changed original query binding", err)
	}
	var complete, finalized, found int
	if err := f.store.DB().QueryRow(`SELECT r.complete,r.finalized,r.found FROM review_runs r JOIN review_workspace w ON w.project_id=r.project_id AND w.snapshot_id=r.snapshot_id WHERE r.source='native-executor'`).Scan(&complete, &finalized, &found); err != nil || complete != 1 || finalized != 0 || found != 1 {
		t.Fatal("completed evidence was not projected without finalization", err)
	}
	// The next empty local tick cannot refresh source, capture another artifact,
	// post a command, or append another event.
	events := cliCount(t, f.store, `SELECT count(*) FROM events`)
	references, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range references {
		if _, err := f.hostedCaptures.Read(t.Context(), a.ID, CaptureReference{ID: ref.Input.CaptureID, SHA256: ref.Input.CaptureSHA256, Bytes: ref.Input.CaptureBytes}); err != nil {
			t.Fatal("recovery lost immutable source", err)
		}
	}
	gate.resume(t)
	gate.interval(t, 15*time.Second)
	if p.repositories.Load() != 2 || f.provider.observations != 2 || cliCount(t, f.store, `SELECT count(*) FROM events`) != events {
		t.Fatal("no-candidate tick performed provider/domain work")
	}
	cancel()
	schedulerDone(t, done, false)
	// The previous terminal result is delivered after ownership has been released.
	nextCtx, nextCancel := context.WithCancel(t.Context())
	defer nextCancel()
	next, err := f.hosted.StartRecovery(nextCtx)
	if err != nil {
		t.Fatal("stopped scheduler could not be restarted", err)
	}
	gate.interval(t, 15*time.Second)
	nextCancel()
	schedulerDone(t, next, false)
}

func TestHostedSchedulerNoCandidateAndLiveActiveDoNotReadProviders(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "live active"}[live], func(t *testing.T) {
			f := newHostedDriverFixture(t)
			if live {
				_, a := f.prepareHosted(t)
				comment, err := f.provider.PostIssueComment(t.Context(), "owner/repo", 2, a.Intent)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, "active", "trigger_posted", "200"); err != nil || comment.ID != 200 {
					t.Fatal(err)
				}
			}
			p := &schedulerProvider{driverHostedProvider: f.provider}
			f.hosted.provider = p
			gate := newSchedulerGate(f.hosted)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done, err := f.hosted.StartRecovery(ctx)
			if err != nil {
				t.Fatal(err)
			}
			gate.interval(t, 15*time.Second)
			events := cliCount(t, f.store, `SELECT count(*) FROM events`)
			gate.resume(t)
			gate.interval(t, 15*time.Second)
			if p.repositories.Load() != 0 || p.currentActors.Load() != 0 || f.provider.observations != 0 || cliCount(t, f.store, `SELECT count(*) FROM events`) != events {
				t.Fatal("empty/live-owned candidate tick performed work")
			}
			cancel()
			schedulerDone(t, done, false)
		})
	}
}

func TestHostedSchedulerMissingQueryOrChangedProviderIdentityBacksOffWithoutIO(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed provider", true: "missing query"}[missing], func(t *testing.T) {
			f, a, p := schedulerUncertainFixture(t)
			if missing {
				if _, err := f.store.DB().Exec(`DELETE FROM review_hosted_allowance_queries WHERE attempt_id=?`, a.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				f.hosted.capability.Hosted.ProviderActorID++
			}
			gate := newSchedulerGate(f.hosted)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			events := cliCount(t, f.store, `SELECT count(*) FROM events`)
			done, err := f.hosted.StartRecovery(ctx)
			if err != nil {
				t.Fatal(err)
			}
			gate.interval(t, 30*time.Second)
			gate.resume(t)
			gate.interval(t, 60*time.Second)
			if p.repositories.Load() != 0 || p.currentActors.Load() != 0 || f.provider.observations != 0 || hostedDriverJournal(t, f, a.ID) != "uncertain" || cliCount(t, f.store, `SELECT count(*) FROM events`) != events || f.provider.queries != 1 || f.provider.reviews != 1 {
				t.Fatal("unsafe recovery binding caused provider work or journal change")
			}
			cancel()
			schedulerDone(t, done, false)
		})
	}
}

func TestHostedSchedulerProviderBackoffCapsAndHonorsRetryAfter(t *testing.T) {
	f, _, p := schedulerUncertainFixture(t)
	p.repositoryError = &reviewprovider.Error{Code: "provider_unavailable", Message: "synthetic safe provider failure"}
	gate := newSchedulerGate(f.hosted)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, seconds := range []int{30, 60, 120, 240, 300, 300} {
		if i > 0 {
			gate.resume(t)
		}
		gate.interval(t, time.Duration(seconds)*time.Second)
	}
	p.repositoryError = &reviewprovider.Error{Code: "provider_rate_limited", Message: "synthetic rate limit", RetryAfterSeconds: 90000}
	gate.resume(t)
	gate.interval(t, 24*time.Hour)
	p.repositoryError = &reviewprovider.Error{Code: "provider_rate_limited", Message: "synthetic shorter retry", RetryAfterSeconds: 1}
	gate.resume(t)
	gate.interval(t, 5*time.Minute)
	p.repositoryError = nil
	f.provider.partial = true
	gate.resume(t)
	gate.interval(t, 15*time.Second)
	p.repositoryError = &reviewprovider.Error{Code: "provider_rate_limited", Message: "synthetic reset retry", RetryAfterSeconds: 120}
	gate.resume(t)
	gate.interval(t, 2*time.Minute)
	p.repositoryError = &reviewprovider.Error{Code: "provider_unavailable", Message: "synthetic continued failure"}
	gate.resume(t)
	gate.interval(t, 60*time.Second)
	if f.provider.queries != 1 || f.provider.reviews != 1 || p.currentActors.Load() != 0 {
		t.Fatal("provider backoff invoked another action")
	}
	cancel()
	schedulerDone(t, done, false)
}

func TestHostedSchedulerAmbiguousAndScopeUnprovenUseFailureBackoff(t *testing.T) {
	for _, scope := range []bool{false, true} {
		t.Run(map[bool]string{false: "conflicting trigger", true: "comparison unproven"}[scope], func(t *testing.T) {
			f, a, p := schedulerUncertainFixture(t)
			p.alterSnapshot = func(snapshot *reviewprovider.HostedPublicationSnapshot) {
				if scope {
					snapshot.Reviews[0].Body = strings.Replace(snapshot.Reviews[0].Body, hostedRetainScope(f.baseSHA, f.headSHA), "", 1)
				} else {
					for _, comment := range snapshot.IssueComments {
						if comment.ID == 200 {
							comment.ID = 201
							snapshot.IssueComments = append(snapshot.IssueComments, comment)
							break
						}
					}
				}
			}
			gate := newSchedulerGate(f.hosted)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done, err := f.hosted.StartRecovery(ctx)
			if err != nil {
				t.Fatal(err)
			}
			gate.interval(t, 30*time.Second)
			gate.resume(t)
			gate.interval(t, 60*time.Second)
			state := hostedDriverJournal(t, f, a.ID)
			if scope && state != "active" || !scope && state != "uncertain" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 || f.provider.queries != 1 || f.provider.reviews != 1 || p.currentActors.Load() != 0 {
				t.Fatal("unproven recovery created evidence/action or lost reservation", state)
			}
			cancel()
			schedulerDone(t, done, false)
		})
	}
}

func TestHostedSchedulerCandidateReadFailureReachesOwner(t *testing.T) {
	f := newHostedDriverFixture(t)
	gate := newSchedulerGate(f.hosted)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gate.interval(t, 15*time.Second)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	gate.resume(t)
	schedulerDone(t, done, true)
	if f.hosted.recovering.Load() {
		t.Fatal("failed scheduler retained worker ownership")
	}
}

func TestHostedSchedulerCancellationDuringProviderAndValidation(t *testing.T) {
	f, a, p := schedulerUncertainFixture(t)
	p.repositoryStarted = make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := cliCount(t, f.store, `SELECT count(*) FROM events`)
	done, err := f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.repositoryStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("recovery did not begin immediate provider read")
	}
	cancel()
	schedulerDone(t, done, false)
	if hostedDriverJournal(t, f, a.ID) != "uncertain" || cliCount(t, f.store, `SELECT count(*) FROM events`) != events || f.provider.observations != 0 || f.provider.queries != 1 || f.provider.reviews != 1 {
		t.Fatal("cancelled provider read changed issued action")
	}
	var absent *HostedDriver
	invalid, err := absent.StartRecovery(t.Context())
	assertCoreErrorCode(t, err, "validation_failed")
	if invalid != nil {
		t.Fatal("nil driver started")
	}
	invalid, err = f.hosted.StartRecovery(nil)
	assertCoreErrorCode(t, err, "validation_failed")
	if invalid != nil {
		t.Fatal("nil context started")
	}
	// Already-cancelled owners exit normally without even selecting a candidate.
	stopped, stop := context.WithCancel(t.Context())
	stop()
	next, err := f.hosted.StartRecovery(stopped)
	if err != nil {
		t.Fatal(err)
	}
	schedulerDone(t, next, false)
	if p.repositories.Load() != 1 {
		t.Fatal("cancelled startup read provider")
	}
}

func TestHostedSchedulerWaitFailureReachesOwner(t *testing.T) {
	f := newHostedDriverFixture(t)
	failure := errors.New("synthetic wait failure")
	f.hosted.wait = func(context.Context, time.Duration) error { return failure }
	done, err := f.hosted.StartRecovery(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatal("wait failure hidden", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait failure did not terminate worker")
	}
}

func TestHostedSchedulerObservationCapacityBacksOffWithoutProviderWork(t *testing.T) {
	f, a, p := schedulerUncertainFixture(t)
	// Reuse the guard's trusted-reference fixture; these are bounded retained
	// journal facts, not a claim that 200 provider captures were downloaded.
	fillHostedRecoveryObservations(t, f.cliEvidenceFixture, a)
	files := hostedCapacityArtifacts(t, f.hostedCaptures)
	events := cliCount(t, f.store, `SELECT count(*) FROM events`)
	gate := newSchedulerGate(f.hosted)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gate.interval(t, 30*time.Second)
	gate.resume(t)
	gate.interval(t, 60*time.Second)
	if p.repositories.Load() != 0 || f.provider.observations != 0 || p.currentActors.Load() != 0 || f.provider.queries != 1 || f.provider.reviews != 1 || cliCount(t, f.store, `SELECT count(*) FROM events`) != events || cliCount(t, f.store, `SELECT count(*) FROM review_hosted_observations`) != 200 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 || strings.Join(hostedCapacityArtifacts(t, f.hostedCaptures), "\n") != strings.Join(files, "\n") {
		t.Fatal("full retained journal caused provider/artifact work or lost its reservation")
	}
	cancel()
	schedulerDone(t, done, false)
}
