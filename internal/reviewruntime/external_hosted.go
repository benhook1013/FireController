package reviewruntime

import (
	"context"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type hostedArchiveSource struct {
	provider HostedProvider
	actor    reviewprovider.Author
}

func (r *hostedArchiveSource) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	return r.provider.Repository(ctx, id)
}
func (r *hostedArchiveSource) Pull(ctx context.Context, repository string, number int64) (reviewprovider.Pull, error) {
	return r.provider.Pull(ctx, repository, number)
}
func (r *hostedArchiveSource) ObserveHosted(ctx context.Context, repository string, number int64) (reviewprovider.HostedPublicationSnapshot, error) {
	return r.provider.ObserveHosted(ctx, repository, number)
}
func (r *hostedArchiveSource) TrustedHostedActor() reviewprovider.Author { return r.actor }
func (r *hostedArchiveSource) ObserveRequiredCI(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
	if reader, ok := r.provider.(interface {
		ObserveRequiredCI(context.Context, reviewprovider.CITarget) (reviewprovider.CIObservation, error)
	}); ok {
		return reader.ObserveRequiredCI(ctx, target)
	}
	return reviewprovider.CIObservation{}, &reviewprovider.Error{Code: "provider_unavailable", Message: "Required CI observation is unavailable for this source adapter."}
}

func (r *hostedArchiveSource) ObserveCIGate(ctx context.Context, target reviewprovider.CITarget, gateRef string) (reviewprovider.CIObservation, error) {
	if reader, ok := r.provider.(interface {
		ObserveCIGate(context.Context, reviewprovider.CITarget, string) (reviewprovider.CIObservation, error)
	}); ok {
		return reader.ObserveCIGate(ctx, target, gateRef)
	}
	return reviewprovider.CIObservation{}, &reviewprovider.Error{Code: "provider_unavailable", Message: "CI gate observation is unavailable for this source adapter."}
}
