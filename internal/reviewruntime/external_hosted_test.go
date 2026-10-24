package reviewruntime

import (
	"context"
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
