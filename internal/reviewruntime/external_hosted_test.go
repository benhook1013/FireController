package reviewruntime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestExternalHostedReaderUsesSameBoundProviderWithoutCommands(t *testing.T) {
	f := newHostedDriverFixture(t)
	reader := f.hosted.HostedObservationReader()
	if reader == nil {
		t.Fatal("no observer")
	}
	if reader.TrustedHostedActor() != f.provider.bot {
		t.Fatal("numeric bot binding lost")
	}
	repository, err := reader.Repository(t.Context(), 901)
	if err != nil {
		t.Fatal(err)
	}
	if repository.ID != 901 {
		t.Fatal("wrong source", repository)
	}
	if _, err = reader.Pull(t.Context(), "owner/repo", 2); err != nil {
		t.Fatal(err)
	}
	snapshot, err := reader.ObserveHosted(t.Context(), "owner/repo", 2)
	if err != nil || !snapshot.Complete {
		t.Fatal(snapshot, err)
	}
	if f.provider.queries != 0 || f.provider.reviews != 0 || f.provider.histories != 0 || f.provider.activities != 0 || f.provider.baselines != 1 {
		t.Fatal("passive adapter invoked command/allowance/activity methods")
	}
	if cliCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatal("read adapter fabricated native execution")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = reader.ObserveHosted(ctx, "owner/repo", 2); err == nil {
		t.Fatal("cancellation ignored")
	}
}

type externalCIProvider struct {
	*driverHostedProvider
	calls int
}

func (p *externalCIProvider) ObserveRequiredCI(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
	p.calls++
	return reviewprovider.CIObservation{Target: target, State: "unproven", Reason: "ci_unknown", StartedAt: time.Now().UTC(), ObservedAt: time.Now().UTC()}, ctx.Err()
}
func TestExternalHostedReaderPreservesOptionalCIObserver(t *testing.T) {
	f := newHostedDriverFixture(t)
	p := &externalCIProvider{driverHostedProvider: f.provider}
	f.hosted.provider = p
	reader := f.hosted.HostedObservationReader().(interface {
		ObserveRequiredCI(context.Context, reviewprovider.CITarget) (reviewprovider.CIObservation, error)
	})
	got, err := reader.ObserveRequiredCI(t.Context(), reviewprovider.CITarget{})
	if err != nil || p.calls != 1 || got.State != "unproven" {
		t.Fatal("CI adapter lost", got, err)
	}
	var _ reviewexec.ExternalHostedReader = (*hostedArchiveSource)(nil)
}

type externalArchiveProvider struct {
	*driverHostedProvider
	calls      int
	repository string
	number     int64
	actor      reviewprovider.Author
}

func (p *externalArchiveProvider) ObserveHostedArchive(ctx context.Context, repository string, number int64, actor reviewprovider.Author) (reviewprovider.HostedPublicationSnapshot, error) {
	p.calls++
	p.repository, p.number, p.actor = repository, number, actor
	return reviewprovider.HostedPublicationSnapshot{Complete: true}, ctx.Err()
}
func TestExternalHostedReaderUsesActorBoundPassiveArchive(t *testing.T) {
	f := newHostedDriverFixture(t)
	p := &externalArchiveProvider{driverHostedProvider: f.provider}
	f.hosted.provider = p
	reader := f.hosted.HostedObservationReader()
	snapshot, err := reader.ObserveHosted(t.Context(), "owner/repo", 2)
	if err != nil || !snapshot.Complete || p.calls != 1 || p.repository != "owner/repo" || p.number != 2 || p.actor != f.provider.bot {
		t.Fatal("archive selector or actor binding lost", snapshot, p, err)
	}
	if f.provider.baselines != 0 || f.provider.queries != 0 || f.provider.reviews != 0 || f.provider.histories != 0 || f.provider.activities != 0 {
		t.Fatal("passive archive called native acquisition or command methods")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reader.ObserveHosted(ctx, "owner/repo", 2); err == nil {
		t.Fatal("archive cancellation lost")
	}
}

func TestExternalHostedReaderDoesNotAdvertiseUnsupportedCI(t *testing.T) {
	f := newHostedDriverFixture(t)
	reader := f.hosted.HostedObservationReader()
	if reader.(interface{ SupportsCIGate() bool }).SupportsCIGate() {
		t.Fatal("unsupported gate observer advertised as available")
	}
	if _, ok := reader.(hostedCIReader); ok {
		t.Fatal("unsupported CI must follow the observer's local ci_reader_unavailable path, not account backoff")
	}
	if _, err := reader.ObserveHosted(t.Context(), "owner/repo", 2); err != nil {
		t.Fatal(err)
	}
}

type externalGateProvider struct {
	*driverHostedProvider
	target reviewprovider.CITarget
	gate   string
}

func (p *externalGateProvider) ObserveCIGate(ctx context.Context, target reviewprovider.CITarget, gate string) (reviewprovider.CIObservation, error) {
	p.target, p.gate = target, gate
	return reviewprovider.CIObservation{Target: target, State: "unproven"}, ctx.Err()
}

func TestExternalHostedReaderPreservesDistinctCIGate(t *testing.T) {
	f := newHostedDriverFixture(t)
	p := &externalGateProvider{driverHostedProvider: f.provider}
	f.hosted.provider = p
	if !f.hosted.HostedObservationReader().(interface{ SupportsCIGate() bool }).SupportsCIGate() {
		t.Fatal("available gate observer lost")
	}
	reader := f.hosted.HostedObservationReader().(interface {
		ObserveCIGate(context.Context, reviewprovider.CITarget, string) (reviewprovider.CIObservation, error)
	})
	target := reviewprovider.CITarget{BaseRef: "stack/parent"}
	got, err := reader.ObserveCIGate(t.Context(), target, "develop")
	if err != nil || p.gate != "develop" || p.target != target || got.Target != target {
		t.Fatal("gate adapter changed actual target or gate", got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = reader.ObserveCIGate(ctx, target, "develop"); err == nil {
		t.Fatal("gate adapter lost cancellation")
	}
	f.hosted.provider = f.provider
	if _, err = f.hosted.HostedObservationReader().(interface {
		ObserveCIGate(context.Context, reviewprovider.CITarget, string) (reviewprovider.CIObservation, error)
	}).ObserveCIGate(t.Context(), target, "develop"); err == nil {
		t.Fatal("unsupported provider invented CI gate results")
	}
}

// This provider intentionally omits the older workflow method.
type externalAttentionOnlyProvider struct {
	*externalGateProvider
	ids, headIDs, historicalIDs []int64
	calls                       int
}

func (p *externalAttentionOnlyProvider) ObserveCIGateWithWorkflowAttention(ctx context.Context, target reviewprovider.CITarget, gate string, ids, headIDs, historicalIDs []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error) {
	p.target, p.gate = target, gate
	p.ids, p.headIDs, p.historicalIDs = ids, headIDs, historicalIDs
	p.calls++
	return reviewprovider.CIObservation{Target: target, State: "pending"}, []reviewprovider.CIWorkflowFailure{{WorkflowID: 31}}, ctx.Err()
}

func TestExternalHostedReaderSupportsAttentionOnlyProvider(t *testing.T) {
	p := &externalAttentionOnlyProvider{externalGateProvider: &externalGateProvider{}}
	r := &hostedArchiveSource{provider: p}
	if !r.SupportsCIWorkflowFailures() || !r.SupportsCIGate() {
		t.Fatal("attention-only capability rejected")
	}
	target := reviewprovider.CITarget{BaseRef: "stack/parent"}
	ids := []int64{31}
	got, rows, err := r.ObserveCIGateWithWorkflows(t.Context(), target, "develop", ids)
	if err != nil || got.Target != target || len(rows) != 1 || rows[0].WorkflowID != 31 || p.target != target || p.gate != "develop" || !reflect.DeepEqual(p.ids, ids) || p.headIDs != nil || p.historicalIDs != nil || p.calls != 1 {
		t.Fatal("ordinary workflow fallback changed selectors or results", got, rows, err, p)
	}
	_, _, err = r.ObserveCIGateWithWorkflowAttention(t.Context(), target, "develop", ids, ids, ids)
	if err != nil || !reflect.DeepEqual(p.headIDs, ids) || !reflect.DeepEqual(p.historicalIDs, ids) || p.calls != 2 {
		t.Fatal("attention selectors lost", p, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err = r.ObserveCIGateWithWorkflows(ctx, target, "develop", ids); !errors.Is(err, context.Canceled) {
		t.Fatal("ordinary fallback lost cancellation", err)
	}
	unsupported := &hostedArchiveSource{provider: &driverHostedProvider{}}
	if unsupported.SupportsCIWorkflowFailures() {
		t.Fatal("unsupported workflow capability advertised")
	}
	if _, _, err = unsupported.ObserveCIGateWithWorkflows(t.Context(), target, "develop", ids); err == nil {
		t.Fatal("unsupported provider fabricated workflow results")
	}
}
