package reviews

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/publicsite"
)

func TestCurrentSummaryStagedChannelReplacementsRemovalAndTies(t *testing.T) {
	f := newAPIFixture(t)
	pull, other := nativePull(1), nativePull(2)
	channels := []string{"hosted", "cli", "manual", "subagent"}
	for _, channel := range channels {
		for _, suffix := range []string{"first", "second"} {
			pull.Attempts = append(pull.Attempts, currentSummaryAttempt(channel+"-"+suffix, channel, "2026-01-01T00:00:00Z", ""))
		}
	}
	other.Attempts = []Attempt{currentSummaryAttempt("untouched-second-pr", "cli", "2026-01-01T00:00:00Z", "")}
	check := func(suffix string) {
		t.Helper()
		got := assertCurrentSummaryReduction(t, f, 1).Pull.LatestAttempts
		if len(got) != len(channels) {
			t.Fatal("changed candidates lost a supported channel")
		}
		for i, channel := range channels {
			if got[i].Channel != channel || got[i].SourceAttemptID != channel+"-"+suffix {
				t.Fatal("staged channel selection or position tie changed", channel, got[i])
			}
		}
		otherSummary := assertCurrentSummaryReduction(t, f, 2)
		if len(otherSummary.Pull.LatestAttempts) != 1 || otherSummary.Pull.LatestAttempts[0].SourceAttemptID != "untouched-second-pr" {
			t.Fatal("scoped staged candidates changed another PR")
		}
	}
	f.observe(t, reviewRequest(0, []PullInput{pull, other}), "all-channel-initial-ties")
	check("second")
	for i := 0; i < len(pull.Attempts); i += 2 {
		pull.Attempts[i].FinishedAt = "2026-01-01T01:00:00Z"
	}
	f.observe(t, reviewRequest(1, []PullInput{pull, other}), "all-channel-replacements")
	check("first")
	removed := pull.Attempts
	pull.Attempts = nil
	for i := 1; i < len(removed); i += 2 {
		pull.Attempts = append(pull.Attempts, removed[i])
	}
	f.observe(t, reviewRequest(2, []PullInput{pull, other}), "all-channel-winner-removal")
	check("second")
	// Active finish corrections and returning membership may regain a later
	// position. Immutable producer/PR/channel/head and start times stay fixed.
	for i, channel := range channels {
		a := removed[2*i]
		a.FinishedAt = ""
		pull.Attempts = append(pull.Attempts, a)
		if a.SourceAttemptID != fmt.Sprintf("%s-first", channel) {
			t.Fatal("fixture order changed")
		}
	}
	f.observe(t, reviewRequest(3, []PullInput{pull, other}), "all-channel-return-ties")
	check("first")
}

func TestCurrentRunOrderTracksEligibilityReorderAndOmission(t *testing.T) {
	f := newAPIFixture(t)
	pull := nativePull(1)
	first := publicWindowRun(1, "cli", 0)
	second := publicWindowRun(1, "cli", 1)
	draft := publicWindowRun(1, "cli", 2)
	draft.Outcome, draft.Finalized = "active", false
	pull.Runs = []RunInput{first, second, draft}
	publicWindowArchive(t, f, 1, []PullInput{pull})
	if err := ValidateCurrentRunOrder(t.Context(), f.store.DB()); err != nil {
		t.Fatal("initial current order cache invalid", err)
	}
	manifest := publicsite.Manifest{Reviews: []publicsite.EnrolledReview{{Number: 1}}}
	initial := publicWindowRead(t, f, manifest)
	if len(initial) != 1 || len(initial[0].CLI) != 2 || initial[0].CLI[0].Key != publicWindowKey(f.project, 1, second) {
		t.Fatal("eligible current rows did not produce newest-first channel window", initial,
			"expected newest", publicWindowKey(f.project, 1, second), "then", publicWindowKey(f.project, 1, first))
	}

	// Reordering existing identities updates the compact index while retaining
	// the same immutable run versions.
	pull.Runs = []RunInput{second, draft, first}
	publicWindowArchive(t, f, 2, []PullInput{pull})
	if err := ValidateCurrentRunOrder(t.Context(), f.store.DB()); err != nil {
		t.Fatal("reordered current order cache invalid", err)
	}
	reordered := publicWindowRead(t, f, manifest)
	if len(reordered[0].CLI) != 2 || reordered[0].CLI[0].Key != publicWindowKey(f.project, 1, first) {
		t.Fatal("public ordering did not follow current run positions", reordered)
	}

	// Completing and finalizing a draft is a supported eligibility transition;
	// it must replace cached source metadata without changing current positions.
	draft.Finalized = true
	draft.Outcome = "completed"
	pull.Runs = []RunInput{second, draft, first}
	publicWindowArchive(t, f, 3, []PullInput{pull})
	if err := ValidateCurrentRunOrder(t.Context(), f.store.DB()); err != nil {
		t.Fatal("eligibility update left stale current order metadata", err)
	}
	updated := publicWindowRead(t, f, manifest)
	if len(updated) != 1 || len(updated[0].CLI) != 3 || updated[0].CLI[0].Key != publicWindowKey(f.project, 1, first) || updated[0].CLI[1].Key != publicWindowKey(f.project, 1, draft) {
		t.Fatal("finalized draft missing or unrelated public order changed", updated)
	}

	// Omission removes the current cache row but leaves historical detail.
	publicWindowArchive(t, f, 4, []PullInput{nativePull(1)})
	if err := ValidateCurrentRunOrder(t.Context(), f.store.DB()); err != nil {
		t.Fatal("omitted run left stale current order row", err)
	}
	var current, retained int
	if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM review_current_run_order WHERE project_id=?),(SELECT count(*) FROM review_runs WHERE project_id=? AND source_run_id IN (?,?))`, f.project, f.project, first.SourceRunID, second.SourceRunID).Scan(&current, &retained); err != nil || current != 0 || retained != 6 {
		t.Fatal("current omission was not reflected", current, retained, err)
	}
}

func TestCurrentRunOrderUpgradeBackfillAndCorruptSourceRollback(t *testing.T) {
	t.Run("valid v5 upgrade", func(t *testing.T) {
		f := newAPIFixture(t)
		pull := nativePull(1)
		pull.Runs = []RunInput{publicWindowRun(1, "cli", 0), publicWindowRun(1, "hosted", 0)}
		f.observe(t, reviewRequest(0, []PullInput{pull}), "run-order-upgrade-one")
		f.observe(t, reviewRequest(1, []PullInput{pull}), "run-order-upgrade-two")
		manifest := publicsite.Manifest{Reviews: []publicsite.EnrolledReview{{Number: 1}}}
		beforePublic := publicWindowRead(t, f, manifest)
		var before PullResponse
		beforeWire := nativeRead(t, f, "/pulls/1", &before)
		if _, err := f.store.DB().Exec(`DROP TABLE review_current_run_order`); err != nil {
			t.Fatal(err)
		}
		removeMetadataForPre7Fixture(t, f.store)
		if _, err := f.store.DB().Exec(`UPDATE metadata SET value='5' WHERE key='schema:reviews'`); err != nil {
			t.Fatal(err)
		}
		if err := EnsureSchema(t.Context(), f.store); err != nil {
			t.Fatal("v5 current-order migration failed", err)
		}
		var version string
		if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil || version != "8" {
			t.Fatal("new review schema version not installed", version, err)
		}
		if err := ValidateCurrentRunOrder(t.Context(), f.store.DB()); err != nil {
			t.Fatal("backfilled order cache invalid", err)
		}
		if got := publicWindowRead(t, f, manifest); !reflect.DeepEqual(got, beforePublic) {
			t.Fatal("backfill changed public window", got, beforePublic)
		}
		if after := nativeRead(t, f, "/pulls/1", &before); string(after) != string(beforeWire) {
			t.Fatal("backfill changed existing private wire bytes")
		}
		digest := historyMigrationDatabaseDigest(t, f)
		if err := EnsureSchema(t.Context(), f.store); err != nil || historyMigrationDatabaseDigest(t, f) != digest {
			t.Fatal("current startup repeated cache backfill", err)
		}
	})

	t.Run("invalid retained reference rolls migration back", func(t *testing.T) {
		f := newAPIFixture(t)
		pull := nativePull(1)
		pull.Runs = []RunInput{publicWindowRun(1, "cli", 0)}
		f.observe(t, reviewRequest(0, []PullInput{pull}), "run-order-rollback-one")
		f.observe(t, reviewRequest(1, []PullInput{pull}), "run-order-rollback-two")
		if _, err := f.store.DB().Exec(`DROP TABLE review_current_run_order`); err != nil {
			t.Fatal(err)
		}
		removeMetadataForPre7Fixture(t, f.store)
		if _, err := f.store.DB().Exec(`UPDATE metadata SET value='5' WHERE key='schema:reviews'`); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.DB().Exec(`UPDATE review_run_memberships SET end_revision=2 WHERE project_id=? AND id=(SELECT id FROM review_current_runs WHERE project_id=? LIMIT 1) AND start_revision=1`, f.project, f.project); err != nil {
			t.Fatal("could not stage supported-schema corruption fixture", err)
		}
		before := historyMigrationDatabaseDigest(t, f)
		if err := EnsureSchema(t.Context(), f.store); err == nil {
			t.Fatal("migration accepted current reference outside its history interval")
		}
		if after := historyMigrationDatabaseDigest(t, f); after != before {
			t.Fatal("failed v6 migration leaked DDL, schema version, or derived changes")
		}
		var version string
		if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil || version != "5" {
			t.Fatal("failed migration advanced schema version", version, err)
		}
		if _, err := f.store.DB().Exec(`UPDATE review_run_memberships SET end_revision=NULL WHERE project_id=? AND id=(SELECT id FROM review_current_runs WHERE project_id=? LIMIT 1) AND start_revision=1`, f.project, f.project); err != nil {
			t.Fatal("could not restore intentionally corrupted fixture", err)
		}
		if err := EnsureSchema(t.Context(), f.store); err != nil {
			t.Fatal("corrected migration retry failed", err)
		}
		if err := ValidateCurrentRunOrder(t.Context(), f.store.DB()); err != nil {
			t.Fatal("retry left invalid current order cache", err)
		}
	})
}

func TestPublicRunOrderFailsClosedOnSelectedCacheMismatch(t *testing.T) {
	for _, mode := range []string{"selected position", "overflow probe"} {
		t.Run(mode, func(t *testing.T) {
			f := newAPIFixture(t)
			pull := nativePull(1)
			run := publicWindowRun(1, "cli", 0)
			if mode == "overflow probe" {
				run.Finalized = false
			}
			pull.Runs = []RunInput{run}
			if mode == "overflow probe" {
				// The forged row must be the 101st candidate: already knowing HasMore
				// does not permit skipping its authoritative eligibility check.
				for i := 1; i <= 100; i++ {
					pull.Runs = append(pull.Runs, publicWindowRun(1, "cli", i))
				}
			}
			publicWindowArchive(t, f, 1, []PullInput{pull})
			statement := `UPDATE review_current_run_order SET run_order=run_order+1 WHERE project_id=? AND run_order=0`
			if mode == "overflow probe" {
				statement = `UPDATE review_current_run_order SET finalized=1 WHERE project_id=? AND run_order=0`
			}
			if _, err := f.store.DB().Exec(statement, f.project); err != nil {
				t.Fatal(err)
			}
			tx, err := f.store.DB().BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			_, err = PublicSiteReviewFacts(t.Context(), tx, f.project, publicsite.Manifest{Reviews: []publicsite.EnrolledReview{{Number: 1}}}, publicWindowSalt)
			if err == nil {
				t.Fatal("selected stale cache metadata was published", mode)
			}
		})
	}
}
