package reviewexec

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type witnessSource struct {
	repo reviewprovider.Repository
	pull reviewprovider.Pull
}

func (s witnessSource) Repository(context.Context, int64) (reviewprovider.Repository, error) {
	return s.repo, nil
}
func (s witnessSource) Pull(context.Context, string, int64) (reviewprovider.Pull, error) {
	return s.pull, nil
}

func TestIndependentClaimWitnessPinsFreshTrustedDefaultBranch(t *testing.T) {
	previous := int64(2)
	c := Configuration{ExpectedRevision: &previous, Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli"}}
	p := Operation{ID: "op_prior", ProjectID: "fixture", PullNumber: 2, Channel: "cli", State: "queued", HeadSHA: strings.Repeat("a", 40), ConfigurationRevision: 3, Revision: 2}
	o := p
	o.ID = "op_current"
	o.PullNumber = 4
	o.HeadSHA = strings.Repeat("b", 40)
	nonDraft := false
	reader := witnessSource{repo: reviewprovider.Repository{ID: 701, FullName: "owner/canonical", DefaultBranch: "main"},
		pull: reviewprovider.Pull{Number: 4, RepositoryID: 701, State: "open", HeadSHA: o.HeadSHA, BaseSHA: strings.Repeat("c", 40), BaseRef: "main", IsDraft: &nonDraft, ChangedFiles: 1}}
	w, err := observeIndependentClaim(t.Context(), reader, 701, c, 3, p, o)
	now := time.Now().UTC()
	if err != nil || !w.matches(now, 701, c, 3, p, o) {
		t.Fatalf("fresh exact ordinary default-base observation refused: %v", err)
	}
	stale := w.observed.Add(independentClaimFreshness + time.Nanosecond)
	if w.matches(stale, 701, c, 3, p, o) || w.matches(now, 702, c, 3, p, o) || w.matches(now, 701, c, 4, p, o) {
		t.Fatal("wrong scope, revision or stale observation accepted")
	}
	skip := p
	skip.Revision++
	if w.matches(now, 701, c, 3, skip, o) {
		t.Fatal("different predecessor revision accepted")
	}
	target := o
	target.HeadSHA = strings.Repeat("d", 40)
	if w.matches(now, 701, c, 3, p, target) {
		t.Fatal("different successor head accepted")
	}
	dependent := reader
	dependent.pull.BaseRef = "branch-2"
	skipped, _ := observeIndependentClaim(t.Context(), dependent, 701, c, 3, p, o)
	if skipped.matches(now, 701, c, 3, p, o) {
		t.Fatal("related source accepted as independent")
	}
	incomplete := reader
	incomplete.pull.IsDraft = nil
	skipped, _ = observeIndependentClaim(t.Context(), incomplete, 701, c, 3, p, o)
	if skipped.matches(now, 701, c, 3, p, o) {
		t.Fatal("unknown lifecycle accepted as exact")
	}
	anchor := Anchor{RepositoryID: 701, PullNumber: 4, CandidateSHA: o.HeadSHA, BaseSHA: reader.pull.BaseSHA,
		ParentSHA: reader.pull.BaseSHA, BaseRef: "main"}
	if !w.matchesPreparedAnchor(anchor) {
		t.Fatal("ordinary fresh Prepare anchor was not bound to the witness")
	}
	refMoved := anchor
	refMoved.BaseSHA = strings.Repeat("e", 40)
	refMoved.ParentSHA = refMoved.BaseSHA
	dependencyAdded := anchor
	dependencyAdded.ParentPullNumber = 2
	if w.matchesPreparedAnchor(refMoved) || w.matchesPreparedAnchor(dependencyAdded) ||
		w.matches(w.observed.Add(-time.Nanosecond), 701, c, 3, p, o) {
		t.Fatal("changed comparison or non-monotonic wall time accepted")
	}

	active := o
	active.independentClaim = &w
	plan, err := ResolveSource(t.Context(), reader, 701, c, Request{PullNumber: 4, HeadSHA: o.HeadSHA, Channel: "cli"})
	if err != nil || !active.MatchesIndependentSourcePlan(plan) {
		t.Fatal("fresh source did not reproduce the bound pre-reservation comparison", err)
	}
	plan.Repository.DefaultBranch = "changed-default"
	if active.MatchesIndependentSourcePlan(plan) {
		t.Fatal("changed default branch can no longer use the provisional witness")
	}
	if !o.MatchesIndependentSourcePlan(plan) {
		t.Fatal("ordinary operation accidentally acquired the additional witness condition")
	}

}

type witnessOnlyDriver struct{ reader TargetReader }

func (d witnessOnlyDriver) IndependentQueueTargetReader() TargetReader { return d.reader }
func (d witnessOnlyDriver) Prepare(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
	panic("read-only probe must not prepare")
}
func (d witnessOnlyDriver) Dispatch(context.Context, Operation, Attempt, func(context.Context, string) error) (Result, error) {
	panic("read-only probe must not dispatch")
}

type blockedWitnessSource struct {
	witnessSource
	calls    atomic.Int32
	started  chan struct{}
	release  chan struct{}
	deadline chan time.Time
}

func (s *blockedWitnessSource) Repository(ctx context.Context, repo int64) (reviewprovider.Repository, error) {
	if s.calls.Add(1) == 1 {
		close(s.started)
		if s.deadline != nil {
			deadline, _ := ctx.Deadline()
			s.deadline <- deadline
		}
	}
	select {
	case <-ctx.Done():
		return reviewprovider.Repository{}, ctx.Err()
	case <-s.release:
	}
	return s.witnessSource.Repository(ctx, repo)
}

func TestIndependentClaimWitnessProbeSingleReadAndNoPreparation(t *testing.T) {
	previous := int64(2)
	c := Configuration{ExpectedRevision: &previous, Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli"}, CapabilityID: "authorized-adapter"}
	p := Operation{ID: "op_prior", ProjectID: "fixture", PullNumber: 2, Channel: "cli", State: "queued", HeadSHA: strings.Repeat("a", 40), ConfigurationRevision: 3, Revision: 2}
	o := p
	o.ID = "op_current"
	o.PullNumber = 4
	o.HeadSHA = strings.Repeat("b", 40)
	isDraft := false
	reader := &blockedWitnessSource{witnessSource: witnessSource{
		repo: reviewprovider.Repository{ID: 701, FullName: "owner/canonical", DefaultBranch: "main"},
		pull: reviewprovider.Pull{Number: 4, RepositoryID: 701, State: "open", HeadSHA: o.HeadSHA, BaseSHA: strings.Repeat("c", 40), BaseRef: "main", IsDraft: &isDraft, ChangedFiles: 1}},
		started: make(chan struct{}), release: make(chan struct{})}
	drivers := map[string]Driver{c.CapabilityID: witnessOnlyDriver{reader: reader}}
	probe := &queuePriorityPreflight{predecessor: p, configuration: c, revision: 3, repositoryID: 701}
	e := &Engine{}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result := make(chan *independentClaimWitness, 1)
	go func() { result <- e.probeIndependentClaim(ctx, drivers, probe, o) }()
	select {
	case <-reader.started:
	case <-ctx.Done():
		t.Fatal("trusted source observation did not start")
	}
	if second := e.probeIndependentClaim(ctx, drivers, probe, o); second != nil || reader.calls.Load() != 1 {
		t.Fatal("another worker used the bounded read slot before it was released")
	}
	close(reader.release)
	select {
	case w := <-result:
		if w == nil {
			t.Fatal("genuine source observation lost witness")
		}
	case <-ctx.Done():
		t.Fatal("trusted source observation did not finish")
	}
	if throttled := e.probeIndependentClaim(ctx, drivers, probe, o); throttled != nil || reader.calls.Load() != 1 {
		t.Fatal("immediately repeated probe bypassed its shared throttle")
	}
}

func TestIndependentClaimWitnessProbeOwnDeadlineAndCancellation(t *testing.T) {
	previous := int64(2)
	c := Configuration{ExpectedRevision: &previous, Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli"}, CapabilityID: "authorized-adapter"}
	p := Operation{ID: "op_prior", ProjectID: "fixture", PullNumber: 2, Channel: "cli", State: "queued", HeadSHA: strings.Repeat("a", 40), ConfigurationRevision: 3, Revision: 2}
	o := p
	o.ID, o.PullNumber, o.HeadSHA = "op_current", 4, strings.Repeat("b", 40)
	reader := &blockedWitnessSource{started: make(chan struct{}), release: make(chan struct{}), deadline: make(chan time.Time, 1)}
	drivers := map[string]Driver{c.CapabilityID: witnessOnlyDriver{reader: reader}}
	pending := &queuePriorityPreflight{predecessor: p, configuration: c, revision: 3, repositoryID: 701}
	e := &Engine{}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	result := make(chan *independentClaimWitness, 1)
	go func() { result <- e.probeIndependentClaim(ctx, drivers, pending, o) }()
	select {
	case deadline := <-reader.deadline:
		callerDeadline, _ := ctx.Deadline()
		if deadline.IsZero() || !deadline.Before(callerDeadline) || time.Until(deadline) > independentProbeTimeout {
			t.Fatal("trusted source reader inherited caller deadline instead of dedicated probe budget")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("trusted source read did not receive a bounded context")
	}
	cancel()
	select {
	case got := <-result:
		if got != nil {
			t.Fatal("a cancelled source read manufactured a witness")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("trusted read did not stop on cancellation")
	}
	e.independentProbeMu.Lock()
	defer e.independentProbeMu.Unlock()
	if e.independentProbeActive {
		t.Fatal("cancelled probe did not release shared slot")
	}
}
