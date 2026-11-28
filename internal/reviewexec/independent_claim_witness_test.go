package reviewexec

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
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
	w, err := observeIndependentClaim(t.Context(), reader, 701, c, 3, []Operation{p}, o)
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
	checkRefusal := func(source witnessSource, label string) {
		t.Helper()
		skipped, err := observeIndependentClaim(t.Context(), source, 701, c, 3, []Operation{p}, o)
		checkedAt := time.Now().UTC()
		if skipped.matches(checkedAt, 701, c, 3, p, o) || !reflect.DeepEqual(skipped, independentClaimWitness{}) {
			t.Fatalf("%s returned a candidate witness: %+v (error: %v)", label, skipped, err)
		}
	}
	dependent := reader
	dependent.pull.BaseRef = "branch-2"
	checkRefusal(dependent, "non-default base")
	incomplete := reader
	incomplete.pull.IsDraft = nil
	checkRefusal(incomplete, "unknown draft state")
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
	probe := &queuePriorityPreflight{predecessors: []Operation{p}, configuration: c, revision: 3, repositoryID: 701}
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
	pending := &queuePriorityPreflight{predecessors: []Operation{p}, configuration: c, revision: 3, repositoryID: 701}
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

func addWitnessQueue(t *testing.T, f *attemptFixture, project string, pulls ...int64) []Receipt {
	t.Helper()
	seedLegacyExecutionConfiguration(t, f, project, "witness-queue", enrollmentConfiguration(0, pulls, []string{"hosted"}))
	var receipts []Receipt
	for _, pull := range pulls {
		receipts = append(receipts, f.enqueue(t, project, fmt.Sprintf("witness-pull-%d", pull), "hosted", pull, 1))
	}
	return receipts
}

func corruptWitnessPull(t *testing.T, f *attemptFixture, project string, pull int64) {
	t.Helper()
	if _, err := f.store.DB().Exec(`INSERT INTO review_pull_scheduling(project_id,pull_number,revision,paused,reason,actor_id,updated_at)
		VALUES(?,?,1,0,'','synthetic-writer','invalid-time')`, project, pull); err != nil {
		t.Fatal(err)
	}
}

type witnessQueueSource struct {
	probed []int64
}

func (s *witnessQueueSource) Repository(_ context.Context, id int64) (reviewprovider.Repository, error) {
	return reviewprovider.Repository{ID: id, FullName: fmt.Sprintf("owner/repo-%d", id), DefaultBranch: "main"}, nil
}

func (s *witnessQueueSource) Pull(_ context.Context, repository string, pull int64) (reviewprovider.Pull, error) {
	var id int64
	if _, err := fmt.Sscanf(repository, "owner/repo-%d", &id); err != nil {
		return reviewprovider.Pull{}, err
	}
	s.probed = append(s.probed, pull)
	base := "main"
	if pull == 4 {
		base = "non-default"
	}
	draft := false
	return reviewprovider.Pull{Number: pull, RepositoryID: id, State: "open", HeadSHA: strings.Repeat("a", 40),
		BaseSHA: strings.Repeat("b", 40), BaseRef: base, IsDraft: &draft, ChangedFiles: 1}, nil
}

func TestIndependentClaimWitnessCoversEveryUnavailablePredecessor(t *testing.T) {
	f := newAttemptFixture(t)
	queue := addWitnessQueue(t, f, attemptProject, 2, 4, 6)
	corruptWitnessPull(t, f, attemptProject, 2)
	ops, err := f.e.pending(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// An earlier healthy request keeps ordinary priority, even when another
	// predecessor cannot be read.
	if slices.ContainsFunc(ops, func(op Operation) bool { return op.ID == queue[2].ID }) {
		t.Fatal("healthy predecessor lost priority")
	}
	corruptWitnessPull(t, f, attemptProject, 4)
	ops, err = f.e.pending(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ops, func(op Operation) bool { return op.ID == queue[2].ID })
	if i < 0 {
		t.Fatal("successor was omitted after both earlier requests became unreadable")
	}
	successor := ops[i]
	preflight := successor.independentPreflight
	if preflight == nil || len(preflight.predecessors) != 2 ||
		preflight.predecessors[0].ID != queue[0].ID || preflight.predecessors[1].ID != queue[1].ID {
		t.Fatalf("preflight did not retain both exact predecessors: %+v", preflight)
	}
	reader := &witnessQueueSource{}
	drivers := map[string]Driver{"test-capability": witnessOnlyDriver{reader: reader}}
	witness := f.e.probeIndependentClaim(t.Context(), drivers, preflight, successor)
	if witness == nil || !slices.Equal(reader.probed, []int64{6}) {
		t.Fatalf("wanted one trusted source observation for both pairs: %v", reader.probed)
	}
	partial := *witness
	partial.predecessors = partial.predecessors[:1]
	a, err := f.e.startAttemptWithWitness(t.Context(), attemptProject, successor.ID, 0, successor.Revision, &partial)
	var missing *queuePriorityPreflight
	if a.ID != "" || !errors.As(err, &missing) || len(missing.predecessors) != 1 || missing.predecessors[0].ID != queue[1].ID ||
		attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatalf("partial witness did not leave the second predecessor fenced: %+v %v", a, err)
	}

	a, err = f.e.startAttemptWithWitness(t.Context(), attemptProject, successor.ID, 0, successor.Revision, witness)
	if err != nil || a.OperationID != successor.ID || a.State != "preparing" ||
		attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 {
		t.Fatalf("complete witness did not claim just the successor: %+v %v", a, err)
	}
	for _, earlier := range queue[:2] {
		if supervisorOperationState(t, f, earlier.ID) != "queued" {
			t.Fatal("source observation modified a predecessor")
		}
	}
}

type witnessQueueDriver struct {
	*controlledDriver
	reader TargetReader
}

func (d witnessQueueDriver) IndependentQueueTargetReader() TargetReader { return d.reader }

func TestIndependentClaimWitnessRejectedProbeYieldsSharedSlot(t *testing.T) {
	f := newAttemptFixture(t)
	blocked := addWitnessQueue(t, f, attemptProject, 2, 4)[1]
	later := addWitnessQueue(t, f, otherAttemptProject, 2, 6)[1]
	corruptWitnessPull(t, f, attemptProject, 2)
	corruptWitnessPull(t, f, otherAttemptProject, 2)
	reader := &witnessQueueSource{}
	driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
		if o.ID != later.ID || o.independentClaim == nil {
			t.Fatalf("the wrong candidate obtained an attempt: %+v", o)
		}
		return Preparation{}, errors.New("stop after proving local claim")
	}}
	drivers := map[string]Driver{"test-capability": witnessQueueDriver{controlledDriver: driver, reader: reader}}
	runBatch := func() {
		t.Helper()
		ops, err := f.e.pending(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := f.e.processPendingBatch(t.Context(), drivers, func() {}, 0, ops, pendingExhaustion{}); err != nil {
			t.Fatal(err)
		}
	}
	runBatch()
	if !slices.Equal(reader.probed, []int64{4}) || driver.prepares.Load() != 0 {
		t.Fatalf("the first nondefault candidate should consume just one source slot: %v", reader.probed)
	}
	// Advance just the shared interval while the failed target's retry
	// remains deferred. No real sleep or dispatch is needed.
	f.e.independentProbeMu.Lock()
	f.e.independentProbeLast = f.e.independentProbeLast.Add(-independentProbeInterval)
	f.e.independentProbeMu.Unlock()
	runBatch()
	if !slices.Equal(reader.probed, []int64{4, 6}) || driver.prepares.Load() != 1 || driver.calls.Load() != 0 ||
		supervisorOperationState(t, f, blocked.ID) != "queued" {
		t.Fatalf("failed target took a second slot instead of yielding to the later candidate: %v", reader.probed)
	}
}
