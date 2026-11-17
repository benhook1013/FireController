package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func frontierFilterSeed(t *testing.T, f *attemptFixture, source string, at time.Time) reviews.Receipt {
	t.Helper()
	run := reviews.RunInput{SourceRunID: source, Channel: "hosted", HeadSHA: strings.Repeat("a", 40), Reviewer: "Retained provider", Scope: "Imported hosted completion", Outcome: "completed", Attributable: true, Complete: true, StartedAt: at.Format(time.RFC3339Nano), FinishedAt: at.Format(time.RFC3339Nano), Findings: []reviews.Finding{}}
	return projectionPublish(t, f, reviews.NativePublication{Pull: projectionPull(2), Run: &run})
}

func TestExternalHostedFrontierFilterBoundaries(t *testing.T) {
	floor := time.Date(2026, 7, 8, 9, 10, 11, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		cutoff   bool
		finished time.Time
		complete bool
		findings int
		wantHeld bool
		wantRuns int
	}{
		{name: "unsupported-before", cutoff: true, finished: floor.Add(-time.Nanosecond)},
		{name: "unsupported-equal", cutoff: true, finished: floor},
		{name: "unsupported-after", cutoff: true, finished: floor.Add(time.Nanosecond), wantHeld: true},
		{name: "unsupported-empty-cutoff", finished: floor, wantHeld: true},
		{name: "unsupported-zero-finish", cutoff: true, wantHeld: true},
		{name: "complete-zero-finish", cutoff: true, complete: true, wantHeld: true},
		{name: "oversized-before", cutoff: true, finished: floor.Add(-time.Nanosecond), complete: true, findings: 101},
		{name: "oversized-equal", cutoff: true, finished: floor, complete: true, findings: 101},
		{name: "oversized-after", cutoff: true, finished: floor.Add(time.Nanosecond), complete: true, findings: 101, wantHeld: true},
		{name: "oversized-empty-cutoff", finished: floor, complete: true, findings: 101, wantHeld: true},
		{name: "supported-before", cutoff: true, finished: floor.Add(-time.Nanosecond), complete: true},
		{name: "supported-equal", cutoff: true, finished: floor, complete: true},
		{name: "supported-after-at-finding-limit", cutoff: true, finished: floor.Add(time.Nanosecond), complete: true, findings: 100, wantRuns: 1},
		{name: "supported-empty-cutoff", finished: floor, complete: true, wantRuns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, _ := lifecycleFixture(t)
			if tc.cutoff {
				frontierFilterSeed(t, f, "imported-hosted-before-observation", floor)
			}
			run := reviewprovider.ExternalHostedRun{SourceID: "publication:11111111-1111-1111-1111-111111111111", HeadSHA: strings.Repeat("a", 40), FinishedAt: tc.finished, Inventory: reviewprovider.HostedFindingInventory{Complete: tc.complete, Findings: make([]reviewprovider.HostedFinding, tc.findings), Publication: reviewprovider.HostedPublication{PublicationID: "11111111-1111-1111-1111-111111111111"}}}
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				inputs, held, err := f.e.externalHostedInputs(ctx, tx, lifecycleTarget{project: attemptProject, repository: 801, number: 2}, []reviewprovider.ExternalHostedRun{run}, 100)
				if err == nil && (held != tc.wantHeld || len(inputs) != tc.wantRuns) {
					return fmt.Errorf("got inputs=%d held=%t; want inputs=%d held=%t", len(inputs), held, tc.wantRuns, tc.wantHeld)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExternalHostedFrontierFilterPreservesKnownIdentityHolds(t *testing.T) {
	for _, mode := range []string{"unsupported", "oversized", "zero-finish", "different-head"} {
		t.Run(mode, func(t *testing.T) {
			f, _, _ := lifecycleFixture(t)
			at := time.Date(2026, 7, 8, 9, 10, 11, 0, time.UTC)
			const identity = "publication:11111111-1111-1111-1111-111111111111"
			frontierFilterSeed(t, f, identity, at)
			run := reviewprovider.ExternalHostedRun{SourceID: identity, HeadSHA: strings.Repeat("a", 40), FinishedAt: at, Inventory: reviewprovider.HostedFindingInventory{Complete: true}}
			switch mode {
			case "unsupported":
				run.Inventory.Complete = false
			case "oversized":
				run.Inventory.Findings = make([]reviewprovider.HostedFinding, 101)
			case "zero-finish":
				run.FinishedAt = time.Time{}
			case "different-head":
				run.HeadSHA = strings.Repeat("b", 40)
			}
			before := projectionDigest(t, f, "")
			if count, held := reconciliationObserve(t, f, lifecycleTarget{project: attemptProject, repository: 801, number: 2}, run, 100); count != 0 || !held {
				t.Fatal("known malformed or conflicting identity was ignored", count, held)
			}
			if after := projectionDigest(t, f, ""); before != after {
				t.Fatal("held known identity altered retained history")
			}
		})
	}
}

func TestExternalHostedFrontierFilterReplayRestartAndDiagnostics(t *testing.T) {
	f, schedule, clock := lifecycleFixture(t)
	snapshot := externalSnapshot(3, strings.Repeat("a", 40))
	floor := snapshot.Reviews[1].SubmittedAt
	original := frontierFilterSeed(t, f, "imported-hosted-before-observation", floor)
	originalDigest := projectionDigest(t, f, original.ID)
	for i := 0; i < 2; i++ {
		snapshot.Reviews[i].Body = strings.Replace(snapshot.Reviews[i].Body, "batch=1/1", "batch=1/2", 1)
	}
	reader := externalReader(snapshot)
	credits := attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`)
	lifecycleTick(t, f, schedule, reader)
	if externalArchiveCount(t, f) != 1 {
		t.Fatal("historical unsupported groups hid newer complete result")
	}
	retained := projectionDigest(t, f, "")
	var initialized string
	for i := 0; i < 3; i++ {
		if i > 0 {
			clock.Add(time.Minute)
			if i == 2 {
				schedule = newLifecycleSchedule(schedule.options)
			}
			lifecycleTick(t, f, schedule, reader)
		}
		health := schedule.due[lifecycleKey{attemptProject, 2}]
		if health.hostedError != "" || health.enrichmentError != "" || health.hostedCheckedAt.IsZero() {
			t.Fatal("out-of-scope historical groups caused retry diagnostic", health)
		}
		var cutoff, gotInitialized string
		if err := f.store.DB().QueryRow(`SELECT cutoff_at,initialized_at FROM review_hosted_discovery_state WHERE project_id=? AND pull_number=2`, attemptProject).Scan(&cutoff, &gotInitialized); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			initialized = gotInitialized
		}
		if cutoff != floor.Format(time.RFC3339Nano) || initialized != gotInitialized || projectionDigest(t, f, "") != retained || projectionDigest(t, f, original.ID) != originalDigest || attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`) != credits {
			t.Fatal("replay/restart changed frontier, history or round credit")
		}
	}
}
