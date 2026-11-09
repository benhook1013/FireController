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
	if archive, ok := r.provider.(interface {
		ObserveHostedArchive(context.Context, string, int64, reviewprovider.Author) (reviewprovider.HostedPublicationSnapshot, error)
	}); ok {
		return archive.ObserveHostedArchive(ctx, repository, number, r.actor)
	}
	return r.provider.ObserveHosted(ctx, repository, number)
}
func (r *hostedArchiveSource) TrustedHostedActor() reviewprovider.Author { return r.actor }

// Keep optional capabilities optional: an unsupported CI adapter is not a
// provider outage and must not throttle every PR sharing the provider account.
type hostedCIReader interface {
	ObserveRequiredCI(context.Context, reviewprovider.CITarget) (reviewprovider.CIObservation, error)
}

type hostedArchiveCISource struct {
	*hostedArchiveSource
	ci hostedCIReader
}

func (r *hostedArchiveCISource) ObserveRequiredCI(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
	return r.ci.ObserveRequiredCI(ctx, target)
}
