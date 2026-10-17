package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestHostedDispatchPendingHandsOffAfterOneObservation(t *testing.T) {
	f := newHostedDriverFixture(t)
	op, attempt := f.prepareHosted(t)
	prefix, err := f.engine.ReadHostedObservations(t.Context(), f.project, attempt.ID)
	if err != nil || len(prefix) != 2 {
		t.Fatal("query and pre-admission baseline missing", len(prefix), err)
	}
	f.provider.partial = true
	waits := 0
	f.hosted.wait = func(context.Context, time.Duration) error {
		waits++
		return context.Canceled
	}
	result, err := f.hosted.Dispatch(t.Context(), op, attempt, f.callback(t, &attempt))
	if err != nil || result.State != "uncertain" || result.Reason != "hosted_observation_pending" || result.ExternalID != "200" || waits != 0 || f.provider.observations != 1 {
		t.Fatalf("pending dispatch retained a live polling slot: result=%+v err=%v waits=%d observations=%d", result, err, waits, f.provider.observations)
	}
	if hostedDriverJournal(t, f, attempt.ID) != "active" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
		t.Fatal("driver changed supervisor-owned settlement or released uncertain work")
	}
	references, err := f.engine.ReadHostedObservations(t.Context(), f.project, attempt.ID)
	if err != nil || len(references) != len(prefix)+1 || !reflect.DeepEqual(references[:len(prefix)], prefix) {
		t.Fatal("handoff did not append one observation to the unchanged preparation prefix", len(references), err)
	}
	for _, ref := range references {
		if _, err := f.hostedCaptures.Read(t.Context(), attempt.ID, CaptureReference{ID: ref.Input.CaptureID, SHA256: ref.Input.CaptureSHA256, Bytes: ref.Input.CaptureBytes}); err != nil {
			t.Fatal("handoff lost raw source", err)
		}
	}
	if _, err := f.engine.Advance(t.Context(), f.project, attempt.ID, attempt.Revision, result.State, result.Reason, result.ExternalID); err != nil {
		t.Fatal("supervisor could not retain uncertain ownership", err)
	}
	candidate, err := f.engine.PendingHostedRecovery(t.Context(), f.capability.ID)
	if err != nil || candidate == nil || candidate.AttemptID != attempt.ID {
		t.Fatal("handoff is not observable by scheduled recovery", candidate, err)
	}
	gate := newSchedulerGate(f.hosted)
	ctx, cancel := context.WithCancel(t.Context())
	done, err := f.hosted.StartRecovery(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	gate.interval(t, 30*time.Second)
	cancel()
	schedulerDone(t, done, false)
	if hostedDriverJournal(t, f, attempt.ID) != "active" || f.provider.observations != 2 {
		t.Fatal("pending recovery did not adopt the original trigger")
	}
	// Reopen the actual store after stopping its owner. Startup recovers the
	// adopted active journal; the new observer completes that same issued work.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = core.Open(t.Context(), f.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	f.engine, err = reviewexec.New(f.store, []reviewexec.Capability{{ID: f.capability.ID, Provider: "coderabbit", Account: f.capability.Account, Channels: f.capability.Channels, Fingerprint: f.capability.Fingerprint}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.engine.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.hosted, err = NewHostedDriver(f.store, f.engine, f.capability, f.manager, f.hostedCaptures, f.provider)
	if err != nil {
		t.Fatal(err)
	}
	f.provider.partial = false
	gate = newSchedulerGate(f.hosted)
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	done, err = f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gate.interval(t, 15*time.Second)
	if hostedDriverJournal(t, f, attempt.ID) != "completed" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || f.provider.queries != 1 || f.provider.reviews != 1 || f.provider.observations != 3 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 1 {
		t.Fatal("restarted observer lost work, reposted, or failed to settle")
	}
	events := cliCount(t, f.store, `SELECT count(*) FROM events`)
	gate.resume(t)
	gate.interval(t, 15*time.Second)
	if cliCount(t, f.store, `SELECT count(*) FROM events`) != events || f.provider.observations != 3 {
		t.Fatal("completed recovery replay performed more observation work")
	}
	cancel()
	schedulerDone(t, done, false)
	for _, ref := range references {
		if _, err := f.hostedCaptures.Read(t.Context(), attempt.ID, CaptureReference{ID: ref.Input.CaptureID, SHA256: ref.Input.CaptureSHA256, Bytes: ref.Input.CaptureBytes}); err != nil {
			t.Fatal("restart lost the original retained source", err)
		}
	}
}

type handoffObservationProvider struct {
	HostedProvider
	alter func(*reviewprovider.HostedPublicationSnapshot)
	err   error
	calls int
}

func (p *handoffObservationProvider) ObserveHosted(ctx context.Context, repository string, number int64) (reviewprovider.HostedPublicationSnapshot, error) {
	p.calls++
	if p.err != nil {
		return reviewprovider.HostedPublicationSnapshot{}, p.err
	}
	snapshot, err := p.HostedProvider.ObserveHosted(ctx, repository, number)
	if err == nil && p.alter != nil {
		p.alter(&snapshot)
	}
	return snapshot, err
}

func TestHostedDispatchHandoffPreservesCapacityConflictAndReadErrors(t *testing.T) {
	for _, mode := range []string{"capacity", "conflict", "read-error"} {
		t.Run(mode, func(t *testing.T) {
			f := newHostedDriverFixture(t)
			op, attempt := f.prepareHosted(t)
			provider := &handoffObservationProvider{HostedProvider: f.provider}
			f.hosted.provider = provider
			wantReason := "hosted_observation_pending"
			wantCalls := 1
			switch mode {
			case "capacity":
				fillHostedRecoveryObservations(t, f.cliEvidenceFixture, attempt)
				wantReason, wantCalls = "hosted_observation_capacity", 0
			case "conflict":
				provider.alter = func(snapshot *reviewprovider.HostedPublicationSnapshot) {
					for _, comment := range snapshot.IssueComments {
						if comment.ID == 200 {
							comment.ID = 201
							snapshot.IssueComments = append(snapshot.IssueComments, comment)
							break
						}
					}
				}
				wantReason = "hosted_observation_conflict"
			case "read-error":
				provider.err = errors.New("synthetic private provider failure")
			}
			f.hosted.wait = func(context.Context, time.Duration) error {
				t.Error("dispatch waited instead of handing off")
				return context.Canceled
			}
			result, err := f.hosted.Dispatch(t.Context(), op, attempt, f.callback(t, &attempt))
			if err != nil || result.State != "uncertain" || result.Reason != wantReason || result.ExternalID != "200" || provider.calls != wantCalls || f.provider.queries != 1 || f.provider.reviews != 1 || hostedDriverJournal(t, f, attempt.ID) != "active" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
				t.Fatalf("handoff changed a held outcome: result=%+v err=%v calls=%d", result, err, provider.calls)
			}
		})
	}
}

type handoffScopedProvider struct {
	*driverHostedProvider
	pull       int64
	entered    chan struct{}
	release    <-chan struct{}
	first      atomic.Bool
	repository reviewprovider.Repository
}

func (p *handoffScopedProvider) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	if id != p.repository.ID {
		return reviewprovider.Repository{}, fmt.Errorf("wrong synthetic repository identity")
	}
	return p.repository, ctx.Err()
}

func (p *handoffScopedProvider) Pull(ctx context.Context, repository string, number int64) (reviewprovider.Pull, error) {
	if repository != p.repository.FullName || number != p.pull {
		return reviewprovider.Pull{}, fmt.Errorf("wrong synthetic source target")
	}
	pull, err := p.driverHostedProvider.Pull(ctx, "owner/repo", number)
	pull.RepositoryID = p.repository.ID
	return pull, err
}

func (p *handoffScopedProvider) ObserveHostedActivity(ctx context.Context, repository, head string, selector reviewprovider.HostedActivitySelector) (reviewprovider.HostedActivity, error) {
	if repository != p.repository.FullName {
		return reviewprovider.HostedActivity{}, fmt.Errorf("wrong synthetic activity target")
	}
	return p.driverHostedProvider.ObserveHostedActivity(ctx, "owner/repo", head, selector)
}

func (p *handoffScopedProvider) PostIssueComment(ctx context.Context, repository string, number int64, body string) (reviewprovider.IssueComment, error) {
	if number != p.pull || repository != p.repository.FullName {
		return reviewprovider.IssueComment{}, fmt.Errorf("wrong synthetic posting target")
	}
	return p.driverHostedProvider.PostIssueComment(ctx, "owner/repo", 2, body)
}

func (p *handoffScopedProvider) IssueComments(ctx context.Context, repository string, number int64) ([]reviewprovider.IssueComment, error) {
	if number != p.pull || repository != p.repository.FullName {
		return nil, fmt.Errorf("wrong synthetic allowance target")
	}
	return p.driverHostedProvider.IssueComments(ctx, "owner/repo", 2)
}

func (p *handoffScopedProvider) ObserveHosted(ctx context.Context, repository string, number int64) (reviewprovider.HostedPublicationSnapshot, error) {
	if number != p.pull || repository != p.repository.FullName {
		return reviewprovider.HostedPublicationSnapshot{}, fmt.Errorf("wrong synthetic observation target")
	}
	// Preparation reads a complete baseline before the full review is posted.
	// Gate only the post-trigger dispatch observation whose slot is under test.
	if p.reviews > 0 && p.first.CompareAndSwap(false, true) {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return reviewprovider.HostedPublicationSnapshot{}, ctx.Err()
		}
	}
	return p.driverHostedProvider.ObserveHosted(ctx, "owner/repo", 2)
}

func handoffProject(t *testing.T, original *cliEvidenceFixture, engine *reviewexec.Engine, project, capability, channel string, number int64) *cliEvidenceFixture {
	t.Helper()
	f := *original
	f.project, f.engine = project, engine
	if _, err := f.store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES(?,?,?,?)`, project, project, 899+number, fmt.Sprintf("owner/repo%d", number)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) SELECT credential_id,?,can_read,can_write FROM grants WHERE project_id=?`, project, original.project); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO review_policy_config(project_id,revision,body_json,actor_id,created_at) SELECT ?,revision,body_json,actor_id,created_at FROM review_policy_config WHERE project_id=?`, project, original.project); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if _, err := engine.Configure(t.Context(), f.scope(t, f.admin, true), "handoff-config", reviewexec.Configuration{ExpectedRevision: &zero, CapabilityID: capability, Mode: "independent", Queue: []int64{number}, Channels: []string{channel}}); err != nil {
		t.Fatal(err)
	}
	return &f
}

func handoffEnqueue(t *testing.T, f *cliEvidenceFixture, number int64, channel string) string {
	t.Helper()
	raw, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "handoff-request", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: number, Channel: channel, HeadSHA: f.headSHA})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt.ID
}

// Observe supervisor slot acquisition without replacing any real CLI work.
type handoffDispatchSignal struct {
	reviewexec.Driver
	entered chan struct{}
}

func (d *handoffDispatchSignal) Dispatch(ctx context.Context, operation reviewexec.Operation, attempt reviewexec.Attempt, callback func(context.Context, string) error) (reviewexec.Result, error) {
	close(d.entered)
	return d.Driver.Dispatch(ctx, operation, attempt, callback)
}

func TestHostedDispatchHandoffFreesBothSupervisorSlotsForCLI(t *testing.T) {
	f := newHostedDriverFixture(t)
	second, third := f.capability, f.capability
	second.ID, second.Account, second.Channels = "handoff-hosted-capability", "handoff-hosted-account", []string{"hosted"}
	third.ID, third.Account, third.Channels, third.Hosted = "handoff-cli-capability", "handoff-cli-account", []string{"cli"}, nil
	// This third account uses the same real local runner protocol with its own
	// exact repository allowance, rather than granting cross-repository credit.
	script, err := os.ReadFile(f.capability.CLIExecutable)
	if err != nil {
		t.Fatal(err)
	}
	third.CLIExecutable = filepath.Join(t.TempDir(), "synthetic-cli-third-repository")
	if err := os.WriteFile(third.CLIExecutable, []byte(strings.ReplaceAll(string(script), "owner/repo", "owner/repo4")), 0700); err != nil {
		t.Fatal(err)
	}
	runner, err := reviewprocess.New(f.manager, third.CLIExecutable, third.AuthenticationEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	second.Fingerprint, err = capabilityFingerprint(second)
	if err != nil {
		t.Fatal(err)
	}
	third.Fingerprint, err = capabilityFingerprint(third)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := []reviewexec.Capability{}
	for _, capability := range []OperatorCapability{f.capability, second, third} {
		capabilities = append(capabilities, reviewexec.Capability{ID: capability.ID, Provider: "coderabbit", Account: capability.Account, Channels: capability.Channels, Fingerprint: capability.Fingerprint})
	}
	f.engine, err = reviewexec.New(f.store, capabilities, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	f2 := handoffProject(t, f.cliEvidenceFixture, f.engine, "handoff-hosted-project", second.ID, "hosted", 3)
	f3 := handoffProject(t, f.cliEvidenceFixture, f.engine, "handoff-cli-project", third.ID, "cli", 4)
	release := make(chan struct{})
	p1 := &handoffScopedProvider{driverHostedProvider: f.provider, pull: 2, entered: make(chan struct{}), release: release, repository: f.reader.repository}
	p2 := &handoffScopedProvider{driverHostedProvider: &driverHostedProvider{cliDriverReader: f.reader, t: t, actor: f.provider.actor, bot: f.provider.bot, reply: runtimeAllowanceAvailable, activity: "idle"}, pull: 3, entered: make(chan struct{}), release: release, repository: reviewprovider.Repository{ID: 902, FullName: "owner/repo3", DefaultBranch: "main"}}
	p3 := &handoffScopedProvider{driverHostedProvider: f.provider, pull: 4, repository: reviewprovider.Repository{ID: 903, FullName: "owner/repo4", DefaultBranch: "main"}}
	// Give each distinct repository identity its own real Git cache, populated
	// exclusively from the fixture's existing local objects.
	cache, err := f.manager.CachePath(901)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{902, 903} {
		destination, err := f.manager.CachePath(id)
		if err != nil {
			t.Fatal(err)
		}
		gitTestRun(t, t.TempDir(), "clone", "--bare", "--template=", cache, destination)
		if err := os.Chmod(destination, 0700); err != nil {
			t.Fatal(err)
		}
	}
	p1.partial, p2.partial = true, true
	firstDriver, err := NewHostedDriver(f.store, f.engine, f.capability, f.manager, f.hostedCaptures, p1)
	if err != nil {
		t.Fatal(err)
	}
	secondDriver, err := NewHostedDriver(f.store, f.engine, second, f.manager, f.hostedCaptures, p2)
	if err != nil {
		t.Fatal(err)
	}
	cliDriver, err := NewCLIDriver(f.store, f.engine, third, f.manager, f.captures, p3, runner)
	if err != nil {
		t.Fatal(err)
	}
	unexpectedWait := make(chan time.Duration, 2)
	fetch := func(ctx context.Context, id int64, repository, head, base, token string) error {
		if map[int64]string{901: "owner/repo", 902: "owner/repo3", 903: "owner/repo4"}[id] != repository || head != f.headSHA || base != f.baseSHA || token != f.capability.GitHubCredential() {
			return fmt.Errorf("wrong synthetic fetch binding")
		}
		return ctx.Err()
	}
	for _, driver := range []*HostedDriver{firstDriver, secondDriver} {
		driver.fetch = fetch
		driver.wait = func(ctx context.Context, delay time.Duration) error {
			unexpectedWait <- delay
			<-ctx.Done()
			return ctx.Err()
		}
	}
	cliDriver.fetch = fetch
	firstID := handoffEnqueue(t, f.cliEvidenceFixture, 2, "hosted")
	secondID := handoffEnqueue(t, f2, 3, "hosted")
	cliEntered := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done, err := f.engine.Start(ctx, map[string]reviewexec.Driver{f.capability.ID: firstDriver, second.ID: secondDriver, third.ID: &handoffDispatchSignal{Driver: cliDriver, entered: cliEntered}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() {
		cancel()
		if done != nil {
			schedulerDone(t, done, false)
		}
	}()
	for _, entered := range []chan struct{}{p1.entered, p2.entered} {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("two real hosted dispatches did not occupy the supervisor slots")
		}
	}
	prefixes := map[string][]reviewexec.HostedObservation{}
	for _, target := range []struct {
		fixture   *cliEvidenceFixture
		operation string
	}{{f.cliEvidenceFixture, firstID}, {f2, secondID}} {
		op, err := f.engine.ReadOperation(t.Context(), target.fixture.scopeWithAccess(t, target.fixture.reader, false, "read"), target.operation)
		if err != nil {
			t.Fatal(err)
		}
		refs, err := f.engine.ReadHostedObservations(t.Context(), target.fixture.project, op.AttemptID)
		if err != nil || len(refs) != 2 {
			t.Fatal("query and baseline missing before pending observation", len(refs), err)
		}
		prefixes[target.operation] = refs
	}
	thirdID := handoffEnqueue(t, f3, 4, "cli")
	releasedAt := time.Now()
	close(release)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	// Handoff means unrelated CLI work acquires a slot while the hosted
	// outcomes remain retained. It does not require real Git/process execution,
	// capture fsync, browser projection and terminal settlement to finish in the
	// same short interval, particularly under race instrumentation.
	select {
	case <-cliEntered:
	case delay := <-unexpectedWait:
		t.Fatalf("pending hosted dispatch still consumed a supervisor slot for %s", delay)
	case <-deadline.C:
		t.Fatal("unrelated actual CLI dispatch did not acquire a supervisor slot")
	}
	for {
		first, firstErr := f.engine.ReadOperation(t.Context(), f.scopeWithAccess(t, f.readerToken(), false, "read"), firstID)
		second, secondErr := f.engine.ReadOperation(t.Context(), f2.scopeWithAccess(t, f2.reader, false, "read"), secondID)
		if firstErr != nil || secondErr != nil {
			t.Fatal(firstErr, secondErr)
		}
		if first.State == "uncertain" && second.State == "uncertain" {
			break
		}
		select {
		case delay := <-unexpectedWait:
			t.Fatalf("pending hosted dispatch still consumed a supervisor slot for %s", delay)
		case <-deadline.C:
			t.Fatalf("hosted handoff outcomes were not retained: first=%s second=%s", first.State, second.State)
		case <-tick.C:
		}
	}
	handoffElapsed := time.Since(releasedAt)
	// Keep actual end-to-end completion and cleanup as a separate finite
	// functional check. Application admission/transaction deadlines are unchanged.
	completion := time.NewTimer(10 * time.Second)
	defer completion.Stop()
	for {
		operation, err := f.engine.ReadOperation(t.Context(), f3.scopeWithAccess(t, f3.reader, false, "read"), thirdID)
		if err != nil {
			t.Fatal(err)
		}
		if operation.State == "completed" {
			break
		}
		select {
		case delay := <-unexpectedWait:
			t.Fatalf("pending hosted dispatch still consumed a supervisor slot for %s", delay)
		case <-completion.C:
			t.Fatalf("unrelated actual CLI execution did not complete: state=%s reason=%s", operation.State, operation.Reason)
		case <-tick.C:
		}
	}
	t.Logf("actual CLI dispatch with both hosted outcomes retained after %s; actual CLI completed after %s", handoffElapsed, time.Since(releasedAt))
	cancel()
	// Stop/join before inspecting non-atomic fixture counters. Both hosted
	// outcomes are durable and no scheduler was started to mutate them later.
	schedulerDone(t, done, false)
	done = nil
	for _, target := range []struct {
		fixture               *cliEvidenceFixture
		operation, capability string
	}{{f.cliEvidenceFixture, firstID, f.capability.ID}, {f2, secondID, second.ID}} {
		operation, err := f.engine.ReadOperation(t.Context(), target.fixture.scopeWithAccess(t, target.fixture.reader, false, "read"), target.operation)
		if err != nil || operation.State != "uncertain" || operation.Reason != "hosted_observation_pending" {
			t.Fatal("supervisor did not retain pending handoff", operation, err)
		}
		bound, err := f.engine.ReadHostedRecovery(t.Context(), target.fixture.project, operation.AttemptID, target.capability)
		if err != nil || bound.Attempt.ExternalID != "200" || bound.EvidenceID != "" {
			t.Fatal("handoff changed original trigger or invented complete evidence", err)
		}
		refs, err := f.engine.ReadHostedObservations(t.Context(), target.fixture.project, operation.AttemptID)
		prefix := prefixes[target.operation]
		if err != nil || len(refs) != len(prefix)+1 || !reflect.DeepEqual(refs[:len(prefix)], prefix) {
			t.Fatal("one-pass raw observation did not preserve its preparation prefix", len(refs), err)
		}
	}
	if p1.queries != 1 || p2.queries != 1 || p1.reviews != 1 || p2.reviews != 1 || p1.observations != 1 || p2.observations != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 2 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 1 {
		t.Fatal("slot release repeated provider actions or lost reservations")
	}
}
