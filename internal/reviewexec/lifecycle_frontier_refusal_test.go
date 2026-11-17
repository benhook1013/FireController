package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
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
			// Metadata establishes the comparison independently of Hosted history.
			metadata, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleMetadata)
			if err != nil || metadata == nil {
				t.Fatal(metadata, err)
			}
			if err := f.e.observeLifecycle(t.Context(), *metadata, s); err != nil {
				t.Fatal(err)
			}
			for pass := 0; pass < 2; pass++ {
				target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
				if err != nil || target == nil {
					t.Fatal("Hosted refusal dropped independent CI", target, err)
				}
				if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
					t.Fatal(err)
				}
				d := s.due[lifecycleKey{attemptProject, 2}]
				if d.hostedError != want || !d.enrichmentNext.After(clock.Now()) || d.ciCheckedAt.IsZero() {
					t.Fatal("missing isolated refusal/cadence", d)
				}
				if r.archiveCalls != 0 || r.ciCalls.Load() != int64(pass+1) || lifecycleRead(t, f).CIState != "success" {
					t.Fatal("independent CI not published", r.archiveCalls, r.ciCalls.Load(), lifecycleRead(t, f))
				}
				if a := s.accounts[target.account]; !a.until.IsZero() || a.lastErrorCode != "" {
					t.Fatal("Hosted refusal poisoned account", a)
				}
				if again, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment); err != nil || again != nil {
					t.Fatal("refusal busy loop", again, err)
				}
				clock.Add(s.options.CIInterval)
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
	if target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment); err != nil || target == nil {
		t.Fatal(target, err)
	} else if err := f.e.observeLifecycle(t.Context(), *target, s); err != nil {
		t.Fatal(err)
	}
	target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lifecycleEnrichment)
	if err != nil || target == nil || target.number != 4 {
		t.Fatal("held target starved the next pull", target, err)
	}
}

func TestLifecycleFrontierRefusalPreservesCIComparisonAndBinding(t *testing.T) {
	for _, change := range []string{"head", "repository"} {
		t.Run(change, func(t *testing.T) {
			f, s, _ := lifecycleFixture(t)
			r := externalReader(externalSnapshot(0, strings.Repeat("a", 40)))
			metadata, err := f.e.claimLifecycleLane(t.Context(), map[string]TargetReader{"test-capability": r}, s, lifecycleMetadata)
			if err != nil || metadata == nil {
				t.Fatal(metadata, err)
			}
			if err := f.e.observeLifecycle(t.Context(), *metadata, s); err != nil {
				t.Fatal(err)
			}
			admissionSQL(t, f, `INSERT INTO review_hosted_discovery_state(project_id,repository_id,pull_number,cutoff_at,initialized_at) VALUES(?,999,2,'','2026-01-01T00:00:00Z')`, attemptProject)
			target, err := f.e.claimLifecycleLane(t.Context(), map[string]TargetReader{"test-capability": r}, s, lifecycleEnrichment)
			if err != nil || target == nil {
				t.Fatal(target, err)
			}
			if change == "head" {
				r.pull = func(context.Context, string, int64) (reviewprovider.Pull, error) {
					p := lifecyclePull(2)
					p.HeadSHA = strings.Repeat("b", 40)
					return p, nil
				}
			} else {
				admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
			}
			if err := f.e.observeLifecycle(t.Context(), *target, s); err != nil {
				t.Fatal(err)
			}
			if r.ciCalls.Load() != 0 || r.archiveCalls != 0 {
				t.Fatal("stale claim collected evidence")
			}
		})
	}
}
