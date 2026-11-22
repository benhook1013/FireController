package reviewexec

import (
	"context"
	"slices"
	"time"
)

// independentClaimWitness is one short-lived, internal observation. It is not
// a client input, persisted source cache, or permission to reserve a slot.
// The supervisor constructs it only for an exact typed per-pull refusal,
// and the serialized claim rechecks those identities before any exemption.
type independentClaimWitness struct {
	predecessor    Operation
	successor      Operation
	configRevision int64
	repositoryID   int64
	defaultRef     string
	baseSHA        string
	observed       time.Time
}

const independentClaimFreshness = 15 * time.Second

func observeIndependentClaim(ctx context.Context, reader TargetReader, repositoryID int64, c Configuration, currentRevision int64, predecessor, successor Operation) (independentClaimWitness, error) {
	if !independentClaimPair(c, currentRevision, predecessor, successor) {
		return independentClaimWitness{}, nil
	}
	plan, err := ResolveSource(ctx, reader, repositoryID, c, Request{PullNumber: successor.PullNumber, HeadSHA: successor.HeadSHA, Channel: successor.Channel})
	if err != nil {
		return independentClaimWitness{}, err
	}
	if plan.SourceReason != "" || plan.ParentPullNumber != 0 || plan.ParentSHA != plan.Pull.BaseSHA ||
		plan.Repository.DefaultBranch == "" || plan.Pull.BaseRef != plan.Repository.DefaultBranch ||
		plan.Pull.IsDraft == nil || *plan.Pull.IsDraft {
		return independentClaimWitness{}, nil
	}
	at, err := time.Parse(time.RFC3339Nano, plan.ObservedAt)
	if err != nil || at.IsZero() {
		return independentClaimWitness{}, nil
	}
	return independentClaimWitness{predecessor: predecessor, successor: successor, configRevision: currentRevision,
		repositoryID: repositoryID, defaultRef: plan.Repository.DefaultBranch, baseSHA: plan.Pull.BaseSHA, observed: at}, nil
}

func independentClaimPair(c Configuration, currentRevision int64, predecessor, successor Operation) bool {
	i, j := slices.Index(c.Queue, predecessor.PullNumber), slices.Index(c.Queue, successor.PullNumber)
	return c.Mode == "independent" && !c.Paused && c.ExpectedRevision != nil &&
		currentRevision > 0 && *c.ExpectedRevision == currentRevision-1 && predecessor.ConfigurationRevision == currentRevision && successor.ConfigurationRevision == currentRevision &&
		predecessor.State == "queued" && successor.State == "queued" &&
		predecessor.ProjectID == successor.ProjectID && identifier.MatchString(successor.ProjectID) &&
		predecessor.Channel == successor.Channel && validChannel(successor.Channel) && slices.Contains(c.Channels, successor.Channel) &&
		predecessor.CapabilityFingerprint == successor.CapabilityFingerprint && predecessor.PolicyRevision == successor.PolicyRevision &&
		identifier.MatchString(predecessor.ID) && identifier.MatchString(successor.ID) && predecessor.ID != successor.ID && predecessor.Revision > 0 && successor.Revision > 0 &&
		i >= 0 && j > i && sha.MatchString(predecessor.HeadSHA) && sha.MatchString(successor.HeadSHA)
}

// matches pins observations to the current operation and project revision in
// the eventual claim. The caller owns normal authorization and order checks
// and must have freshly re-observed this exact predecessor's typed refusal.
func (w independentClaimWitness) matches(now time.Time, repoID int64, c Configuration, rev int64, predecessor, successor Operation) bool {
	return !w.observed.IsZero() && !now.Before(w.observed) && now.Sub(w.observed) <= independentClaimFreshness &&
		repoID > 0 && repoID == w.repositoryID && w.defaultRef != "" && sha.MatchString(w.baseSHA) &&
		w.configRevision == rev && independentClaimPair(c, rev, predecessor, successor) &&
		sameIndependentClaimOperation(predecessor, w.predecessor) && sameIndependentClaimOperation(successor, w.successor)
}

func sameIndependentClaimOperation(a, b Operation) bool {
	return a.ID == b.ID && a.Revision == b.Revision && a.ProjectID == b.ProjectID &&
		a.ConfigurationRevision == b.ConfigurationRevision && a.PolicyRevision == b.PolicyRevision &&
		a.ActorID == b.ActorID && a.WorkerID == b.WorkerID && a.Channel == b.Channel &&
		a.PullNumber == b.PullNumber && a.HeadSHA == b.HeadSHA && a.CapabilityFingerprint == b.CapabilityFingerprint
}

// IndependentQueueSource is implemented only by trusted in-process drivers.
// It exposes an observation reader, not a preparation or provider trigger.
type IndependentQueueSource interface {
	IndependentQueueTargetReader() TargetReader
}

const (
	independentProbeInterval = 5 * time.Second
	independentProbeTimeout  = 10 * time.Second
)

func (e *Engine) probeIndependentClaim(ctx context.Context, drivers map[string]Driver, p *queuePriorityPreflight, successor Operation) *independentClaimWitness {
	driver, ok := drivers[p.configuration.CapabilityID].(IndependentQueueSource)
	if !ok {
		return nil
	}
	reader := driver.IndependentQueueTargetReader()
	if reader == nil {
		return nil
	}
	// One optional source probe process-wide. Provider I/O is outside all
	// transactions; the interval also bounds repeated unavailable reads.
	now := time.Now()
	e.independentProbeMu.Lock()
	if e.independentProbeActive || !e.independentProbeLast.IsZero() && now.Sub(e.independentProbeLast) < independentProbeInterval {
		e.independentProbeMu.Unlock()
		return nil
	}
	e.independentProbeActive, e.independentProbeLast = true, now
	e.independentProbeMu.Unlock()
	defer func() {
		e.independentProbeMu.Lock()
		e.independentProbeActive = false
		e.independentProbeMu.Unlock()
	}()
	probeCtx, cancel := context.WithTimeout(ctx, independentProbeTimeout)
	defer cancel()
	w, err := observeIndependentClaim(probeCtx, reader, p.repositoryID, p.configuration, p.revision, p.predecessor, successor)
	if err != nil || probeCtx.Err() != nil || !w.matches(e.sampleExecutionTime(), p.repositoryID, p.configuration, p.revision, p.predecessor, successor) {
		return nil
	}
	return &w
}

func (w independentClaimWitness) matchesPreparedAnchor(a Anchor) bool {
	return a.RepositoryID == w.repositoryID && a.PullNumber == w.successor.PullNumber &&
		a.CandidateSHA == w.successor.HeadSHA && a.BaseSHA == w.baseSHA && a.ParentSHA == w.baseSHA &&
		a.BaseRef == w.defaultRef && a.ParentPullNumber == 0
}

// MatchesIndependentSourcePlan compares the immediately fresh trusted source
// read with the pre-reservation witness. Ordinary operations have no witness.
// Drivers use this before any provider work that could have side effects.
func (o Operation) MatchesIndependentSourcePlan(plan SourcePlan) bool {
	w := o.independentClaim
	if w == nil {
		return true
	}
	return plan.SourceReason == "" && plan.Repository.DefaultBranch == w.defaultRef &&
		plan.Pull.IsDraft != nil && !*plan.Pull.IsDraft &&
		w.matchesPreparedAnchor(Anchor{RepositoryID: plan.Repository.ID, PullNumber: plan.Pull.Number,
			CandidateSHA: plan.Pull.HeadSHA, BaseSHA: plan.Pull.BaseSHA, ParentSHA: plan.ParentSHA,
			BaseRef: plan.Pull.BaseRef, ParentPullNumber: plan.ParentPullNumber})
}
