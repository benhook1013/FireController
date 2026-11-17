package reviewruntime

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestHostedCompletionUnchangedPollingSurvivesJournalLimitAndRestart(t *testing.T) {
	f, a, p := schedulerUncertainFixture(t)
	f.provider.partial = true
	var providerTime time.Time
	p.alterSnapshot = func(s *reviewprovider.HostedPublicationSnapshot) {
		if providerTime.IsZero() {
			providerTime = s.Reviews[0].SubmittedAt
		}
		s.Reviews[0].SubmittedAt = providerTime
		s.ReviewComments[0].CreatedAt = providerTime
		s.ReviewComments[0].UpdatedAt = providerTime
	}
	gate := newSchedulerGate(f.hosted)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gate.interval(t, 30*time.Second)
	before, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	files := hostedCapacityArtifacts(t, f.hostedCaptures)
	for range 205 {
		gate.resume(t)
		gate.interval(t, 30*time.Second)
	}
	after, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
	if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(files, hostedCapacityArtifacts(t, f.hostedCaptures)) {
		t.Fatal("unchanged provider source consumed journal/artifact capacity", len(before), len(after), err)
	}
	cancel()
	schedulerDone(t, done, false)
	// No process-local digest is needed: a fresh worker finds the durable source.
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	done, err = f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gate.interval(t, 30*time.Second)
	after, err = f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("restart duplicated unchanged source", err)
	}
	// A provider timestamp change alone is a genuine source change.
	providerTime = providerTime.Add(time.Nanosecond)
	gate.resume(t)
	gate.interval(t, 30*time.Second)
	after, err = f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
	if err != nil || len(after) != len(before)+1 {
		t.Fatal("provider timestamp change was discarded", err)
	}
	f.provider.partial = false
	gate.resume(t)
	gate.interval(t, 15*time.Second)
	if hostedDriverJournal(t, f, a.ID) != "completed" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 1 || f.provider.queries != 1 || f.provider.reviews != 1 || p.currentActors.Load() != 0 {
		t.Fatal("completion failed or reposted the original review")
	}
	cancel()
	schedulerDone(t, done, false)
}
