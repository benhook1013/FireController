package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviews"
)

func TestLifecycleFrontierDomainRefusalHoldsOnlyTarget(t *testing.T) {
	for _, mode := range []string{"repository", "timestamp"} {
		t.Run(mode, func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			if mode == "repository" {
				// Retained frontiers with an older binding are permitted by backup validation.
				admissionSQL(t, f, `INSERT INTO review_hosted_discovery_state(project_id,repository_id,pull_number,cutoff_at,initialized_at) VALUES(?,999,2,'','2026-01-01T00:00:00Z')`, attemptProject)
			} else {
				// This valid RFC3339 offset timestamp normalizes beyond year 9999.
				run := reviews.RunInput{SourceRunID: "future-hosted", Channel: "hosted", HeadSHA: strings.Repeat("a", 40), Reviewer: "synthetic", Scope: "test", Outcome: "completed", Complete: true, Attributable: true, StartedAt: "9999-12-31T23:00:00-01:00", FinishedAt: "9999-12-31T23:59:59-01:00", Findings: []reviews.Finding{}}
				projectionPublish(t, f, reviews.NativePublication{Pull: projectionPull(2), Run: &run})
			}
			r := externalReader(externalSnapshot(0, strings.Repeat("a", 40)))
			readers := map[string]TargetReader{"test-capability": r}
			want := "observation_conflict"
			if mode == "timestamp" {
				want = "validation_failed"
			}
			target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
			if err != nil || target != nil {
				t.Fatal("domain refusal escaped target", target, err)
			}
			d := s.due[lifecycleKey{attemptProject, 2}]
			if d.enrichmentError != want || !d.enrichmentNext.After(clock.Now()) {
				t.Fatal("missing safe hold", d)
			}
			if len(s.accounts) != 0 || len(s.lanes[lifecycleEnrichment].accounts) != 0 || r.archiveCalls != 0 {
				t.Fatal("hold consumed account/provider capacity")
			}
			if !lifecycleTick(t, f, s, r) {
				t.Fatal("metadata lane stopped")
			}
			if !s.due[lifecycleKey{attemptProject, 2}].successAt.Equal(clock.Now()) {
				t.Fatal("metadata did not publish")
			}
			// The changed-comparison shortcut must not turn the hold into a busy loop.
			if target, err = f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment); err != nil || target != nil {
				t.Fatal(target, err)
			}
			// A persistent refusal is retried at ordinary cadence without
			// changing any retained history or escalating to daemon failure.
			clock.Add(time.Minute)
			target, err = f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
			if err != nil || target != nil {
				t.Fatal("persistent refusal escaped target", target, err)
			}
			if !s.due[lifecycleKey{attemptProject, 2}].enrichmentNext.After(clock.Now()) {
				t.Fatal("retry not deferred")
			}

		})
	}
}

func TestLifecycleFrontierStorageFailureRemainsFatal(t *testing.T) {
	f, s, _ := lifecycleFixture(t)
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DROP TABLE review_hosted_discovery_state`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := externalReader(externalSnapshot(0, strings.Repeat("a", 40)))
	if _, err := f.e.claimLifecycleLane(t.Context(), map[string]TargetReader{"test-capability": r}, s, lifecycleEnrichment); err == nil {
		t.Fatal("storage failure swallowed")
	}
}

func TestLifecycleFrontierRefusalDoesNotStarveNextPull(t *testing.T) {
	f, s, _ := lifecycleFixture(t)
	previous := int64(2)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "frontier-two-pulls", Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}, Paused: true}); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `INSERT INTO review_hosted_discovery_state(project_id,repository_id,pull_number,cutoff_at,initialized_at) VALUES(?,999,2,'','2026-01-01T00:00:00Z')`, attemptProject)
	r := externalReader(externalSnapshot(0, strings.Repeat("a", 40)))
	readers := map[string]TargetReader{"test-capability": r}
	if target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment); err != nil || target != nil {
		t.Fatal(target, err)
	}
	target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
	if err != nil || target == nil || target.number != 4 {
		t.Fatal("held target starved the next pull", target, err)
	}
}
