package reviewruntime

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// The existing command fixture uses one display name. This adapter supplies a
// second real repository binding while retaining its exact synthetic commands.
type schedulerRepositoryProvider struct {
	*driverHostedProvider
}

func (p *schedulerRepositoryProvider) local(t *testing.T, repo string) string {
	t.Helper()
	if repo != p.repository.FullName {
		t.Fatal("wrong repository binding", repo)
	}
	return "owner/repo"
}
func (p *schedulerRepositoryProvider) PostIssueComment(ctx context.Context, repo string, number int64, body string) (reviewprovider.IssueComment, error) {
	return p.driverHostedProvider.PostIssueComment(ctx, p.local(p.t, repo), number, body)
}
func (p *schedulerRepositoryProvider) IssueComments(ctx context.Context, repo string, number int64) ([]reviewprovider.IssueComment, error) {
	return p.driverHostedProvider.IssueComments(ctx, p.local(p.t, repo), number)
}
func (p *schedulerRepositoryProvider) ObserveHosted(ctx context.Context, repo string, number int64) (reviewprovider.HostedPublicationSnapshot, error) {
	return p.driverHostedProvider.ObserveHosted(ctx, p.local(p.t, repo), number)
}
func (p *schedulerRepositoryProvider) ObserveHostedActivity(ctx context.Context, repo, head string, selector reviewprovider.HostedActivitySelector) (reviewprovider.HostedActivity, error) {
	return p.driverHostedProvider.ObserveHostedActivity(ctx, p.local(p.t, repo), head, selector)
}

type schedulerRepositories struct {
	*driverHostedProvider // posting methods are never used by recovery
	targets               map[int64]*schedulerRepositoryProvider
	failures              map[int64]error
	calls                 []int64
}

func (p *schedulerRepositories) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	p.calls = append(p.calls, id)
	if err := p.failures[id]; err != nil {
		return reviewprovider.Repository{}, err
	}
	if target := p.targets[id]; target != nil {
		return target.Repository(ctx, id)
	}
	return reviewprovider.Repository{}, errors.New("unbound synthetic repository")
}
func (p *schedulerRepositories) ObserveHosted(ctx context.Context, repo string, number int64) (reviewprovider.HostedPublicationSnapshot, error) {
	for _, target := range p.targets {
		if target.repository.FullName == repo {
			return target.ObserveHosted(ctx, repo, number)
		}
	}
	return reviewprovider.HostedPublicationSnapshot{}, errors.New("unbound synthetic repository")
}

func schedulerTwoRepositories(t *testing.T) (*hostedDriverFixture, *hostedDriverFixture, reviewexec.Attempt, reviewexec.Attempt, *schedulerRepositories) {
	t.Helper()
	f, first, _ := schedulerUncertainFixture(t)
	local := *f.cliEvidenceFixture
	local.project = "runtime-second-project"
	if _, err := f.store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES(?,?,902,'owner/second')`, local.project, local.project); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES('runtime-evidence-writer',?,1,1)`, local.project); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO review_policy_config(project_id,revision,body_json,actor_id,created_at) SELECT ?,revision,body_json,actor_id,created_at FROM review_policy_config WHERE project_id=?`, local.project, f.project); err != nil {
		t.Fatal(err)
	}
	local.seedLegacyConfiguration(t)
	cache, err := f.manager.CachePath(902)
	if err != nil {
		t.Fatal(err)
	}
	originalCache, err := f.manager.CachePath(901)
	if err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, t.TempDir(), "clone", "--bare", "--template=", originalCache, cache)
	if err := os.Chmod(cache, 0700); err != nil {
		t.Fatal(err)
	}
	reader := &cliDriverReader{repository: reviewprovider.Repository{ID: 902, FullName: "owner/second", DefaultBranch: "main"}, pulls: map[int64]reviewprovider.Pull{}, calls: map[int64]int{}}
	for id, pull := range f.reader.pulls {
		pull.RepositoryID = 902
		reader.pulls[id] = pull
	}
	secondProvider := &driverHostedProvider{cliDriverReader: reader, t: t, actor: f.provider.actor, bot: f.provider.bot, reply: runtimeAllowanceAvailable, activity: "idle", lost: true}
	adapter := &schedulerRepositoryProvider{secondProvider}
	driver, err := NewHostedDriver(f.store, f.engine, f.capability, f.manager, f.hostedCaptures, adapter)
	if err != nil {
		t.Fatal(err)
	}
	driver.fetch = func(ctx context.Context, id int64, repo, _, _, token string) error {
		if id != 902 || repo != "owner/second" || token != f.capability.GitHubCredential() {
			t.Fatal("wrong second workspace identity")
		}
		return ctx.Err()
	}
	driver.wait = f.hosted.wait
	second := &hostedDriverFixture{cliDriverFixture: &cliDriverFixture{cliEvidenceFixture: &local, reader: reader, capability: f.capability}, hosted: driver, provider: secondProvider, hostedCaptures: f.hostedCaptures}
	op, attempt := second.prepareHosted(t)
	result, err := driver.Dispatch(t.Context(), op, attempt, second.callback(t, &attempt))
	if err != nil || result.State != "uncertain" || secondProvider.reviews != 1 {
		t.Fatal("second review was not issued exactly once", result, err)
	}
	if err = f.engine.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	bound, err := f.engine.ReadHostedRecovery(t.Context(), local.project, attempt.ID, f.capability.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := &schedulerRepositories{driverHostedProvider: f.provider, targets: map[int64]*schedulerRepositoryProvider{901: {f.provider}, 902: adapter}, failures: map[int64]error{}}
	f.hosted.provider = p
	return f, second, first, bound.Attempt, p
}

func schedulerNextLocalWait(t *testing.T, gate *schedulerGate) time.Duration {
	t.Helper()
	select {
	case delay := <-gate.calls:
		if delay <= 0 || delay > hostedRecoveryInterval {
			t.Fatal("invalid local due/rescan interval", delay)
		}
		return delay
	case <-time.After(3 * time.Second):
		t.Fatal("recovery did not yield")
		return 0
	}
}

func schedulerAdvanceToCalls(t *testing.T, gate *schedulerGate, provider *schedulerRepositories, want int) {
	t.Helper()
	// The two real passes finish a few milliseconds apart, so their due times
	// need not coincide. Advance only actual requested intervals, not providers.
	for range 10 {
		gate.resume(t)
		schedulerNextLocalWait(t, gate)
		if len(provider.calls) >= want {
			if len(provider.calls) != want {
				t.Fatal("provider checked an extra target", provider.calls)
			}
			return
		}
	}
	t.Fatal("due targets did not progress", provider.calls)
}

func TestHostedSchedulerMultipleRepositoriesFairRecoveryAndRestart(t *testing.T) {
	f, second, firstAttempt, secondAttempt, provider := schedulerTwoRepositories(t)
	f.provider.partial, second.provider.partial = true, true
	targets, err := f.engine.PendingHostedRecoveries(t.Context(), f.capability.ID)
	if err != nil || len(targets) != 2 {
		t.Fatal("both original reviews must be eligible", targets, err)
	}
	firstRepo := int64(901)
	if targets[0].AttemptID == secondAttempt.ID {
		firstRepo = 902
	}
	provider.failures[firstRepo] = errors.New("one target temporarily unavailable")
	gate := newSchedulerGate(f.hosted)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gate.interval(t, 15*time.Second)
	if len(provider.calls) != 2 || f.provider.observations+second.provider.observations != 1 {
		t.Fatal("ordinary target error starved the independent repository", provider.calls)
	}
	delete(provider.failures, firstRepo)
	gate.resume(t)
	schedulerNextLocalWait(t, gate)
	if len(provider.calls) != 2 {
		t.Fatal("local discovery rescan polled providers before their due times", provider.calls)
	}
	schedulerAdvanceToCalls(t, gate, provider, 4)
	if len(provider.calls) != 4 || hostedDriverJournal(t, f, firstAttempt.ID) != "active" || hostedDriverJournal(t, second, secondAttempt.ID) != "active" {
		t.Fatal("per-attempt backoff failed to recover both original triggers", provider.calls)
	}
	// One target completes without making the other disappear from observation.
	second.provider.partial = false
	schedulerAdvanceToCalls(t, gate, provider, 6)
	if hostedDriverJournal(t, second, secondAttempt.ID) != "completed" || hostedDriverJournal(t, f, firstAttempt.ID) != "active" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
		t.Fatal("independent completion lost sibling reservation or invented evidence")
	}
	cancel()
	schedulerDone(t, done, false)
	// Restart observes the remaining issued original, never reissues either command.
	f.provider.partial = false
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	done, err = f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gate.interval(t, 15*time.Second)
	if hostedDriverJournal(t, f, firstAttempt.ID) != "completed" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 2 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || f.provider.queries != 1 || second.provider.queries != 1 || f.provider.reviews != 1 || second.provider.reviews != 1 {
		t.Fatal("restart reposted or lost original completion")
	}
	cancel()
	schedulerDone(t, done, false)
}

func TestHostedSchedulerMultipleRepositoriesShareProviderRetryAfter(t *testing.T) {
	f, second, _, secondAttempt, provider := schedulerTwoRepositories(t)
	f.provider.partial, second.provider.partial = true, true
	targets, err := f.engine.PendingHostedRecoveries(t.Context(), f.capability.ID)
	if err != nil || len(targets) != 2 {
		t.Fatal(targets, err)
	}
	firstRepo := int64(901)
	if targets[0].AttemptID == secondAttempt.ID {
		firstRepo = 902
	}
	provider.failures[firstRepo] = &reviewprovider.Error{Code: "provider_rate_limited", Status: 429, RetryAfterSeconds: 120}
	gate := newSchedulerGate(f.hosted)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gate.interval(t, 120*time.Second)
	if len(provider.calls) != 1 || f.provider.observations+second.provider.observations != 0 {
		t.Fatal("provider account RetryAfter did not pause the other repository", provider.calls)
	}
	delete(provider.failures, firstRepo)
	gate.resume(t)
	gate.interval(t, 15*time.Second)
	if len(provider.calls) != 3 || f.provider.observations != 1 || second.provider.observations != 1 || f.provider.queries != 1 || second.provider.queries != 1 || f.provider.reviews != 1 || second.provider.reviews != 1 {
		t.Fatal("account backoff lost fairness or reposted originals", provider.calls)
	}
	cancel()
	schedulerDone(t, done, false)
}
