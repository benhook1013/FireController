package reviewruntime

import (
	"context"
	"slices"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// SourceReader exposes only the operator-bound read adapter for explicit source
// refresh work. Selecting it does not prepare or execute a review.
func (d *CLIDriver) SourceReader() reviewexec.TargetReader {
	if d == nil {
		return nil
	}
	return d.reader
}

func (d *HostedDriver) SourceReader() reviewexec.TargetReader {
	if d == nil {
		return nil
	}
	return d.provider
}

func reviewHold(reason string) error {
	return &core.Error{Code: "review_held", Message: "The selected native review is held before execution.", Status: 409, Details: map[string]string{"reason": reason}}
}

func sameOperation(left, right reviewexec.Operation) bool {
	return left.ID == right.ID && left.ProjectID == right.ProjectID && left.ActorID == right.ActorID && left.WorkerID == right.WorkerID && left.ConfigurationRevision == right.ConfigurationRevision && left.PolicyRevision == right.PolicyRevision && left.CapabilityFingerprint == right.CapabilityFingerprint && left.PullNumber == right.PullNumber && left.HeadSHA == right.HeadSHA && left.Channel == right.Channel
}

func operationRequest(operation reviewexec.Operation) reviewexec.Request {
	return reviewexec.Request{ExpectedConfigurationRevision: operation.ConfigurationRevision, PullNumber: operation.PullNumber, HeadSHA: operation.HeadSHA, Channel: operation.Channel}
}

// Each phase has its own memoizer. Repeated stack-resolution reads within one
// phase share facts, while Dispatch cannot reuse Prepare's provider observation.
type sourceReads struct {
	reader       reviewexec.TargetReader
	repositories map[int64]reviewprovider.Repository
	pulls        map[sourcePullKey]reviewprovider.Pull
}
type sourcePullKey struct {
	repository string
	number     int64
}

func newSourceReads(reader reviewexec.TargetReader) *sourceReads {
	return &sourceReads{reader: reader, repositories: map[int64]reviewprovider.Repository{}, pulls: map[sourcePullKey]reviewprovider.Pull{}}
}
func (r *sourceReads) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	if value, ok := r.repositories[id]; ok {
		return value, nil
	}
	value, err := r.reader.Repository(ctx, id)
	if err == nil {
		r.repositories[id] = value
	}
	return value, err
}
func (r *sourceReads) Pull(ctx context.Context, repository string, number int64) (reviewprovider.Pull, error) {
	key := sourcePullKey{repository, number}
	if value, ok := r.pulls[key]; ok {
		return value, nil
	}
	value, err := r.reader.Pull(ctx, repository, number)
	if err == nil {
		r.pulls[key] = value
	}
	return value, err
}

func resolveReviewSource(ctx context.Context, engine *reviewexec.Engine, reads *sourceReads, repositoryID int64, configuration reviewexec.Configuration, operation reviewexec.Operation) (reviewexec.SourcePlan, []reviewexec.SourcePlan, error) {
	plan, err := reviewexec.ResolveSource(ctx, reads, repositoryID, configuration, operationRequest(operation))
	if err != nil {
		return plan, nil, err
	}
	comparison, err := engine.Comparison(ctx, operation.ProjectID, plan, operation.Channel)
	if err != nil {
		return plan, nil, err
	}
	if comparison.Pending {
		return plan, nil, core.Fail("review_evidence_pending", "Resolve retained native evidence in the selected channel before starting another review.", 409)
	}
	if comparison.Covered {
		return plan, nil, reviewHold("target_already_covered")
	}
	if configuration.Mode != "stacked" {
		return plan, nil, nil
	}
	position := slices.Index(configuration.Queue, operation.PullNumber)
	predecessors := make([]reviewexec.SourcePlan, 0, position)
	for _, number := range configuration.Queue[:position] {
		pull, err := reads.Pull(ctx, plan.Repository.FullName, number)
		if err != nil {
			return plan, nil, err
		}
		if pull.RepositoryID != repositoryID || pull.Number != number {
			return plan, nil, reviewHold("predecessor_identity_changed")
		}
		if pull.Merged {
			predecessors = append(predecessors, reviewexec.SourcePlan{Repository: plan.Repository, Pull: pull, ObservedAt: plan.ObservedAt})
			continue
		}
		if pull.State != "open" {
			return plan, nil, reviewHold("predecessor_unavailable")
		}
		request := operationRequest(operation)
		request.PullNumber, request.HeadSHA = number, pull.HeadSHA
		preceding, err := reviewexec.ResolveSource(ctx, reads, repositoryID, configuration, request)
		if err != nil {
			return plan, nil, err
		}
		covered, err := engine.Coverage(ctx, operation.ProjectID, preceding, operation.Channel)
		if err != nil {
			return plan, nil, err
		}
		if !covered {
			return plan, nil, reviewHold("predecessor_unfinished")
		}
		predecessors = append(predecessors, preceding)
	}
	return plan, predecessors, nil
}
