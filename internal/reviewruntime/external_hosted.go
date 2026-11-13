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
func (r *hostedArchiveSource) PullWithRepository(ctx context.Context, repository string, number int64) (reviewprovider.Pull, reviewprovider.Repository, error) {
	if reader, ok := r.provider.(interface {
		PullWithRepository(context.Context, string, int64) (reviewprovider.Pull, reviewprovider.Repository, error)
	}); ok {
		return reader.PullWithRepository(ctx, repository, number)
	}
	return reviewprovider.Pull{}, reviewprovider.Repository{}, reviewprovider.ErrCombinedPullUnsupported
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

func (r *hostedArchiveSource) SupportsCIGate() bool {
	_, ok := r.provider.(interface {
		ObserveCIGate(context.Context, reviewprovider.CITarget, string) (reviewprovider.CIObservation, error)
	})
	return ok
}

func (r *hostedArchiveSource) ObserveCIGate(ctx context.Context, target reviewprovider.CITarget, gateRef string) (reviewprovider.CIObservation, error) {
	if reader, ok := r.provider.(interface {
		ObserveCIGate(context.Context, reviewprovider.CITarget, string) (reviewprovider.CIObservation, error)
	}); ok {
		return reader.ObserveCIGate(ctx, target, gateRef)
	}
	return reviewprovider.CIObservation{}, &reviewprovider.Error{Code: "provider_unavailable", Message: "CI gate observation is unavailable for this source adapter."}
}

func (r *hostedArchiveSource) SupportsCIWorkflowFailures() bool {
	_, ok := r.provider.(interface {
		ObserveCIGateWithWorkflows(context.Context, reviewprovider.CITarget, string, []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error)
	})
	_, attention := r.provider.(interface {
		ObserveCIGateWithWorkflowAttention(context.Context, reviewprovider.CITarget, string, []int64, []int64, []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error)
	})
	return ok || attention
}

func (r *hostedArchiveSource) SupportsCIWorkflowAttention() bool {
	_, ok := r.provider.(interface {
		ObserveCIGateWithWorkflowAttention(context.Context, reviewprovider.CITarget, string, []int64, []int64, []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error)
	})
	return ok
}

func (r *hostedArchiveSource) ObserveCIGateWithWorkflows(ctx context.Context, target reviewprovider.CITarget, gateRef string, ids []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error) {
	if reader, ok := r.provider.(interface {
		ObserveCIGateWithWorkflows(context.Context, reviewprovider.CITarget, string, []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error)
	}); ok {
		return reader.ObserveCIGateWithWorkflows(ctx, target, gateRef, ids)
	}
	if reader, ok := r.provider.(interface {
		ObserveCIGateWithWorkflowAttention(context.Context, reviewprovider.CITarget, string, []int64, []int64, []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error)
	}); ok {
		return reader.ObserveCIGateWithWorkflowAttention(ctx, target, gateRef, ids, nil, nil)
	}
	return reviewprovider.CIObservation{}, nil, &reviewprovider.Error{Code: "provider_unavailable", Message: "Workflow failure observation is unavailable for this source adapter."}
}

func (r *hostedArchiveSource) ObserveCIGateWithWorkflowAttention(ctx context.Context, target reviewprovider.CITarget, gateRef string, ids, headIDs, historicalIDs []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error) {
	if reader, ok := r.provider.(interface {
		ObserveCIGateWithWorkflowAttention(context.Context, reviewprovider.CITarget, string, []int64, []int64, []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error)
	}); ok {
		return reader.ObserveCIGateWithWorkflowAttention(ctx, target, gateRef, ids, headIDs, historicalIDs)
	}
	if len(headIDs) > 0 || len(historicalIDs) > 0 {
		return reviewprovider.CIObservation{}, nil, &reviewprovider.Error{Code: "provider_unavailable", Message: "Advanced workflow attention is unavailable for this source adapter."}
	}
	return r.ObserveCIGateWithWorkflows(ctx, target, gateRef, ids)
}
