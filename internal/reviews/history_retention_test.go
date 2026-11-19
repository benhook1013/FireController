package reviews

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/retention"
)

func TestReviewHistoryRetentionGoldenFloorAndCurrent(t *testing.T) {
	f := newAPIFixture(t)
	if err := jobs.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := f.store.EnsureIncrementalVacuum(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`CREATE TABLE review_selection_decision_history(sequence INTEGER PRIMARY KEY,last_seen_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	pull := nativePull(1)
	run := nativeRun("native-retention-floor")
	first := mustNativePublish(t, f, NativePublication{Pull: pull, Run: &run})
	publishTitle := func(title string) Receipt {
		t.Helper()
		r, err := sourcePublish(f, sourcePins(t, f, 1), sourcePull(1, title))
		if err != nil || r.ID == "" {
			t.Fatal(r, err)
		}
		return r.Receipt
	}
	second := publishTitle("Superseded metadata")
	floor := publishTitle("Floor metadata")
	latest := publishTitle("Current metadata")
	now := time.Now().UTC().Truncate(time.Second)
	for i, r := range []Receipt{first, second, floor, latest} {
		age := 31 * 24 * time.Hour
		if i == 2 {
			age = 30 * 24 * time.Hour
		} // inclusive cutoff, no fractional suffix
		if i == 3 {
			age = 1 * time.Hour
		}
		if _, err := f.store.DB().Exec(`UPDATE review_snapshots SET recorded_at=? WHERE project_id=? AND revision=?`, now.Add(-age).Format(time.RFC3339Nano), f.project, r.Revision); err != nil {
			t.Fatal(err)
		}
	}
	route := func(snap string) string { return "/pulls/1?snapshot=" + url.QueryEscape(snap) }
	var result PullResponse
	floorBytes := nativeRead(t, f, route(floor.ID), &result)
	currentBytes := nativeRead(t, f, "/pulls/1", &result)
	var detail RunDetailResponse
	detailRoute := "/runs/" + runOpaqueID(f.project, nativeSource, run.SourceRunID) + "?snapshot=" + url.QueryEscape(floor.ID)
	detailBytes := nativeRead(t, f, detailRoute, &detail)

	cfg := retention.Config{Interval: retention.DefaultInterval, ReviewHistory: retention.DefaultHistory, JobHistory: retention.DefaultHistory, Receipts: retention.DefaultHistory, Diagnostics: retention.DefaultDiagnostics, DryRun: true}
	dry, skip, err := retention.RunPass(t.Context(), f.store, cfg, now)
	if err != nil || skip {
		t.Fatal(dry, skip, err)
	}
	if dry.Deleted["review_snapshots"] != 1 || dry.Deleted["events"] != 1 || dry.Deleted["review_pull_versions"] == 0 {
		t.Fatal("unexpected dry counts", dry.Deleted)
	}
	nativeRead(t, f, route(first.ID), &result)
	if _, err = f.store.DB().Exec(`CREATE TRIGGER test_interrupt_history BEFORE DELETE ON review_pull_versions BEGIN SELECT RAISE(ABORT,'synthetic interruption'); END`); err != nil {
		t.Fatal(err)
	}
	cfg.DryRun = false
	failed, skip, err := retention.RunPass(t.Context(), f.store, cfg, now)
	if err == nil || skip || failed.Error != "review_history_failed" {
		t.Fatal("missing interruption", failed, skip, err)
	}
	for _, snap := range []string{first.ID, second.ID} {
		w := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews"+route(snap), f.readToken, "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("older existing snapshot returned %d: %s", w.Code, w.Body.String())
		}
	}
	if err = ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal("interrupted fence made graph invalid", err)
	}
	if !bytes.Equal(floorBytes, nativeRead(t, f, route(floor.ID), &result)) || !bytes.Equal(currentBytes, nativeRead(t, f, "/pulls/1", &result)) {
		t.Fatal("interrupted pass changed visible state")
	}
	if _, err = f.store.DB().Exec(`DROP TRIGGER test_interrupt_history`); err != nil {
		t.Fatal(err)
	}
	actual, skip, err := retention.RunPass(t.Context(), f.store, cfg, now)
	if err != nil || skip {
		t.Fatal(actual, skip, err)
	}
	for _, table := range []string{"review_pull_memberships", "review_pull_versions", "review_snapshots", "events"} {
		if actual.Deleted[table]+failed.Deleted[table] != dry.Deleted[table] {
			t.Fatalf("%s dry=%d actual=%d interrupted=%d", table, dry.Deleted[table], actual.Deleted[table], failed.Deleted[table])
		}
	}
	if !bytes.Equal(floorBytes, nativeRead(t, f, route(floor.ID), &result)) || !bytes.Equal(currentBytes, nativeRead(t, f, "/pulls/1", &result)) || !bytes.Equal(detailBytes, nativeRead(t, f, detailRoute, &detail)) {
		t.Fatal("retained API bytes changed")
	}
	for _, validate := range []func() error{
		func() error { return ValidateHistoryReferences(t.Context(), f.store.DB()) },
		func() error { return ValidateCurrentSummaries(t.Context(), f.store.DB()) },
	} {
		if err = validate(); err != nil {
			t.Fatal(err)
		}
	}
	var retainedAnchor, deletedSnapshot, deletedEvent int
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM review_snapshots WHERE snapshot_id=?`, first.ID).Scan(&retainedAnchor); err != nil || retainedAnchor != 1 {
		t.Fatal("proof snapshot lost", retainedAnchor, err)
	}
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM review_snapshots WHERE snapshot_id=?`, second.ID).Scan(&deletedSnapshot); err != nil || deletedSnapshot != 0 {
		t.Fatal("orphan snapshot remained", deletedSnapshot, err)
	}
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='review.observed' AND resource_id=?`, second.ID).Scan(&deletedEvent); err != nil || deletedEvent != 0 {
		t.Fatal("orphan snapshot event remained", deletedEvent, err)
	}
	again, skip, err := retention.RunPass(t.Context(), f.store, cfg, now)
	if err != nil || skip {
		t.Fatal(again, skip, err)
	}
	for table, n := range again.Deleted {
		if n != 0 {
			t.Fatal("second pass still deletes", table, n)
		}
	}
}

func TestReviewHistoryRetentionKeepsNativeAndHostedAuthorityAnchors(t *testing.T) {
	f := newAPIFixture(t)
	if err := jobs.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := f.store.EnsureIncrementalVacuum(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`CREATE TABLE review_selection_decision_history(sequence INTEGER PRIMARY KEY,last_seen_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	pull := nativePull(1)
	run := nativeRun("native-authority-anchor")
	start := mustNativePublish(t, f, NativePublication{Pull: pull, Run: &run})
	observed := externalRun(31)
	externalInit(t, f)
	origin := externalMustPublish(t, f, observed)
	extID := ExternalHostedRunID(f.project, observed.SourceRunID)
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		ob := observed.Findings[0]
		ob.Disposition, ob.DecisionReason, ob.Actor, ob.DecidedAt = "rejected", "reviewed", "fixture", "2026-10-20T00:02:00Z"
		return FinalizeExternalHostedRun(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "fixture"}, NativeAdjudication{SourceRunID: observed.SourceRunID, Finalized: true, FrozenCounts: &NativeCounts{Found: 1}, Findings: []NativeFindingUpdate{{SourceFindingID: ob.SourceFindingID, Disposition: ob.Disposition, DecisionReason: ob.DecisionReason, Actor: ob.Actor, DecidedAt: ob.DecidedAt}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	var finalized int64
	if err = f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, f.project).Scan(&finalized); err != nil {
		t.Fatal(err)
	}
	var finalCount int64
	finalCount, err = ExternalHostedFinalizedRunAccepted(t.Context(), f.store.DB(), f.project, 1, extID)
	if err != nil || finalCount != 0 {
		t.Fatal(finalCount, err)
	}
	r, err := sourcePublish(f, sourcePins(t, f, 1), sourcePull(1, "History floor"))
	if err != nil || r.ID == "" {
		t.Fatal(r, err)
	}
	floor := r.Receipt
	r, err = sourcePublish(f, sourcePins(t, f, 1), sourcePull(1, "History latest"))
	if err != nil || r.ID == "" {
		t.Fatal(r, err)
	}
	latest := r.Receipt

	nativeID := runOpaqueID(f.project, nativeSource, run.SourceRunID)
	// Insert intermediate versions using the exact current physical schema.
	// One version has an artificial inbound child using implicit PK columns;
	// the other can be reclaimed. The first finalized original remains pinned.
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON`); err != nil {
			return err
		}
		runBody := strings.Join(historyRunRecord.body, ",")
		findBody := strings.Join(append([]string{"pull_number"}, historyFindingColumns...), ",")
		for _, rev := range []int64{origin.Revision, finalized, floor.Revision} {
			if _, err := tx.ExecContext(ctx, "INSERT INTO review_run_versions(project_id,id,version_revision,"+runBody+") SELECT project_id,id,?,"+runBody+" FROM review_run_versions WHERE project_id=? AND id=? AND version_revision=?", rev, f.project, nativeID, start.Revision); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO review_finding_versions(project_id,run_id,run_version_revision,"+findBody+") SELECT project_id,run_id,?,"+findBody+" FROM review_finding_versions WHERE project_id=? AND run_id=? AND run_version_revision=?", rev, f.project, nativeID, start.Revision); err != nil {
				return err
			}
		}
		for _, step := range []struct {
			q    string
			args []any
		}{
			{`UPDATE review_run_memberships SET end_revision=? WHERE project_id=? AND id=? AND start_revision=?`, []any{origin.Revision, f.project, nativeID, start.Revision}},
			{`INSERT INTO review_run_memberships VALUES(?,?,?,?,?,1,0),(?,?,?,?,?,1,0),(?,?,?,NULL,?,1,0)`, []any{f.project, nativeID, origin.Revision, finalized, origin.Revision, f.project, nativeID, finalized, floor.Revision, finalized, f.project, nativeID, floor.Revision, floor.Revision}},
			{`UPDATE review_current_runs SET start_revision=? WHERE project_id=? AND id=?`, []any{floor.Revision, f.project, nativeID}},
			{`CREATE TABLE test_extra_finding_reference(project_id TEXT NOT NULL,run_id TEXT NOT NULL,version INT NOT NULL,finding TEXT NOT NULL,FOREIGN KEY(project_id,run_id,version,finding) REFERENCES review_finding_versions)`, nil},
			{`INSERT INTO test_extra_finding_reference VALUES(?,?,?,?)`, []any{f.project, nativeID, origin.Revision, "finding-" + run.SourceRunID}},
		} {
			if _, err := tx.ExecContext(ctx, step.q, step.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err = f.store.DB().Exec(`UPDATE review_snapshots SET recorded_at=CASE WHEN revision<=? THEN ? ELSE ? END WHERE project_id=?`, floor.Revision, now.Add(-31*24*time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), f.project); err != nil {
		t.Fatal(err)
	}
	var history RunsResponse
	route := "/pulls/1/runs?view=timeline&snapshot=" + url.QueryEscape(latest.ID)
	baseline := nativeRead(t, f, route, &history)
	cfg := retention.Config{Interval: retention.DefaultInterval, ReviewHistory: retention.DefaultHistory, JobHistory: retention.DefaultHistory, Receipts: retention.DefaultHistory, Diagnostics: retention.DefaultDiagnostics, DryRun: true}
	dry, skip, err := retention.RunPass(t.Context(), f.store, cfg, now)
	if err != nil || skip {
		t.Fatal(dry, skip, err)
	}
	cfg.DryRun = false
	actual, skip, err := retention.RunPass(t.Context(), f.store, cfg, now)
	if err != nil || skip {
		t.Fatal(actual, skip, err)
	}
	for _, table := range []string{"review_run_versions", "review_finding_versions"} {
		if dry.Deleted[table] != 1 || actual.Deleted[table] != 1 {
			t.Fatal("expected exactly one dead version+finding", table, dry.Deleted, actual.Deleted)
		}
	}
	if !bytes.Equal(baseline, nativeRead(t, f, route, &history)) {
		t.Fatal("retention altered native finalized timeline response")
	}
	if got, err := ExternalHostedFinalizedRunAccepted(t.Context(), f.store.DB(), f.project, 1, extID); err != nil || got != finalCount {
		t.Fatal("Hosted finalized authority changed", got, err)
	}
	var event int
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='review.observed' AND resource_id=?`, origin.ID).Scan(&event); err != nil || event != 1 {
		t.Fatal("external origin event was deleted", event, err)
	}
}
