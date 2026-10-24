package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func externalRun(n int) RunInput {
	return RunInput{SourceRunID: fmt.Sprintf("run:00000000-0000-0000-0000-%012x", n), Channel: "hosted", HeadSHA: strings.Repeat("d", 40), Reviewer: "Configured CodeRabbit", Scope: "Original provider comparison", Outcome: "completed", Attributable: true, Complete: true, StartedAt: "2026-10-20T00:00:00Z", FinishedAt: "2026-10-20T00:01:00Z", Found: 1, Findings: []Finding{{SourceFindingID: fmt.Sprintf("provider-comment-%d", n), Title: "Original provider finding", Detail: "Retained literal provider detail", Severity: "medium", Disposition: "unadjudicated"}}}
}
func externalInit(t *testing.T, f *apiFixture) string {
	t.Helper()
	var cutoff string
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		cutoff, err = InitializeExternalHostedFrontier(ctx, tx, f.project, 701, 1)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return cutoff
}
func externalPublish(f *apiFixture, runs ...RunInput) (Receipt, bool, error) {
	var receipt Receipt
	var changed bool
	err := f.store.BackgroundWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		receipt, changed, err = PublishExternalHosted(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "synthetic-observer"}, 701, 1, runs)
		return err
	})
	return receipt, changed, err
}
func externalMustPublish(t *testing.T, f *apiFixture, runs ...RunInput) Receipt {
	t.Helper()
	receipt, changed, err := externalPublish(f, runs...)
	if err != nil || !changed {
		t.Fatal(receipt, changed, err)
	}
	return receipt
}
func TestExternalHostedAdditiveArchiveReplayAndCapturedSnapshot(t *testing.T) {
	f := newAPIFixture(t)
	pull := mixedChannelPull()
	first := historyObservationPost(t, f, reviewRequest(0, []PullInput{pull}), "external-initial")
	initial := channelCountsPull(t, f, "")
	original := nativeSnapshotBytes(t, f, first.ID)
	if externalInit(t, f) != "" {
		t.Fatal("unfinished legacy runs should not create a finish frontier")
	}
	run := externalRun(1)
	second := externalMustPublish(t, f, run)
	got := channelCountsPull(t, f, "")
	if got.Pull.RunCounts != (ChannelRunCounts{Hosted: 7, CLI: 6, Subagent: 6, Manual: 6}) || got.Pull.AttemptCount != initial.Pull.AttemptCount || got.Pull.HeadSHA != initial.Pull.HeadSHA || got.Pull.Title != initial.Pull.Title || got.Pull.State != initial.Pull.State {
		t.Fatal(got)
	}
	if channelCountsPull(t, f, first.ID).Pull.RunCounts != initial.Pull.RunCounts || nativeSnapshotBytes(t, f, first.ID) != original {
		t.Fatal("old snapshot changed")
	}
	var archive RunsResponse
	nativeRead(t, f, "/pulls/1/runs?limit=100", &archive)
	var id string
	for _, r := range archive.Items {
		if r.SourceRunID == run.SourceRunID {
			id = r.ID
			if r.HeadSHA != run.HeadSHA || r.Finalized || r.Accepted != 0 {
				t.Fatal(r)
			}
		}
	}
	if id == "" {
		t.Fatal("external source missing")
	}
	var detail RunDetailResponse
	nativeRead(t, f, "/runs/"+id, &detail)
	if len(detail.Findings) != 1 || detail.Findings[0].Detail != run.Findings[0].Detail || detail.Findings[0].Disposition != "unadjudicated" {
		t.Fatal(detail)
	}
	before := historyMigrationDatabaseDigest(t, f)
	changedSource := run
	changedSource.Findings = append([]Finding(nil), run.Findings...)
	changedSource.Findings[0].Detail = "Edited provider text must not overwrite capture"
	for i := 0; i < 2; i++ {
		if r, changed, err := externalPublish(f, changedSource); err != nil || changed || r != second {
			t.Fatal(r, changed, err)
		}
		if externalInit(t, f) != "" {
			t.Fatal("frontier advanced on restart")
		}
		if err := EnsureSchema(t.Context(), f.store); err != nil {
			t.Fatal(err)
		}
	}
	if before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("replay or restart wrote records")
	}
	nativeRead(t, f, "/runs/"+id, &detail)
	if detail.Findings[0].Detail != run.Findings[0].Detail {
		t.Fatal(detail)
	}
	zero := externalRun(2)
	zero.SourceRunID = "publication:00000000-0000-0000-0000-000000000002"
	zero.Found = 0
	zero.Findings = nil
	externalMustPublish(t, f, zero)
	assertCurrentSummaryReduction(t, f, 1)
}
func TestExternalHostedImmutableFrontierUsesRetainedOriginalHistory(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	r := externalRun(9)
	r.SourceRunID = "imported-run"
	r.StartedAt = "2026-10-20T00:00:00Z"
	r.FinishedAt = "2026-10-20T13:01:00.000000002+13:00"
	p.Runs = []RunInput{r}
	historyObservationPost(t, f, reviewRequest(0, []PullInput{p}), "frontier-original")
	// Complete observation omits the old run, but retained source history remains.
	historyObservationPost(t, f, reviewRequest(1, []PullInput{nativePull(1)}), "frontier-omitted")
	want := "2026-10-20T00:01:00.000000002Z"
	if got := externalInit(t, f); got != want {
		t.Fatal(got)
	}
	older := externalRun(1)
	if _, changed, err := externalPublish(f, older); err != nil || changed {
		t.Fatal(changed, err)
	}
	newer := externalRun(2)
	newer.FinishedAt = "2026-10-20T00:01:00.000000003Z"
	externalMustPublish(t, f, newer)
	if got := externalInit(t, f); got != want {
		t.Fatal("moving frontier", got)
	}
	if err := ValidateExternalHostedFrontiers(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}
func TestExternalHostedExactImportedIdentityAndRefusals(t *testing.T) {
	f := newAPIFixture(t)
	imported := externalRun(1)
	p := nativePull(1)
	p.Runs = []RunInput{imported}
	historyObservationPost(t, f, reviewRequest(0, []PullInput{p}), "external-imported")
	externalInit(t, f)
	// Advance the genuine finish only; exact imported identity must still dedup.
	imported.FinishedAt = "2026-10-21T00:00:00Z"
	before := historyMigrationDatabaseDigest(t, f)
	if _, changed, err := externalPublish(f, imported); err != nil || changed || before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal(changed, err)
	}
	imported.HeadSHA = strings.Repeat("e", 40)
	_, _, err := externalPublish(f, imported)
	nativeErrorCode(t, err, "observation_conflict")
	for _, mutate := range []func(*RunInput){func(r *RunInput) { r.Finalized = true }, func(r *RunInput) { r.Complete = false }, func(r *RunInput) { r.Outcome = "uncertain" }, func(r *RunInput) { r.Accepted = 1 }, func(r *RunInput) { r.SourceRunID = "unprefixed" }, func(r *RunInput) { r.Findings[0].Disposition = "rejected" }, func(r *RunInput) { r.FinishedAt = "0001-01-01T00:00:00Z"; r.StartedAt = "" }, func(r *RunInput) { r.Findings[0].Detail = strings.Repeat("x", (16<<10)+1) }} {
		r := externalRun(3)
		mutate(&r)
		_, _, err := externalPublish(f, r)
		if err == nil {
			t.Fatal("invalid external source accepted", r)
		}
	}
	if _, _, err := externalPublish(f, make([]RunInput, maxPullRuns+1)...); err == nil {
		t.Fatal("unbounded batch")
	}
	tx := historyWriterTx(t, f)
	if _, err := InitializeExternalHostedFrontier(t.Context(), tx, f.project, 999, 1); err == nil {
		t.Fatal("repository rebind accepted")
	}
	_ = tx.Rollback()
	if before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("refused observations wrote records")
	}
}
func TestExternalHostedRollbackMissingRetiredAndConcurrentNative(t *testing.T) {
	f := newAPIFixture(t)
	historyObservationPost(t, f, reviewRequest(0, []PullInput{nativePull(1)}), "external-original")
	if _, _, err := externalPublish(f, externalRun(1)); err == nil {
		t.Fatal("publisher skipped durable initialization")
	}
	externalInit(t, f)
	before := historyMigrationDatabaseDigest(t, f)
	tx := historyWriterTx(t, f)
	if _, changed, err := PublishExternalHosted(t.Context(), tx, core.Scope{ProjectID: f.project, PrincipalID: "synthetic-observer"}, 701, 1, []RunInput{externalRun(1)}); err != nil || !changed {
		t.Fatal(changed, err)
	}
	_ = tx.Rollback()
	if before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("caller rollback leaked archive")
	}
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_external_event BEFORE INSERT ON events WHEN NEW.event_type='review.observed' BEGIN SELECT RAISE(ABORT,'synthetic external failure'); END`); err != nil {
		t.Fatal(err)
	}
	before = historyMigrationDatabaseDigest(t, f)
	if _, _, err := externalPublish(f, externalRun(1)); err == nil || before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("failed event leaked partial archive", err)
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER reject_external_event`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, _, err := externalPublish(f, externalRun(1)); errs <- err }()
	go func() {
		defer wg.Done()
		r := nativeRun("native-alongside-external")
		_, err := nativePublish(f, NativePublication{Pull: nativePull(1), Run: &r})
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := channelCountsPull(t, f, "")
	if got.Pull.RunCounts != (ChannelRunCounts{Hosted: 1, CLI: 1}) || got.Pull.AttemptCount != 0 {
		t.Fatal(got)
	}
	assertCurrentSummaryReduction(t, f, 1)
	var retained RunDetailResponse
	nativeRead(t, f, "/runs/"+runOpaqueID(f.project, nativeSource, "native-alongside-external"), &retained)
	if len(retained.Findings) != 1 || retained.Findings[0].Disposition != "accepted" || retained.Findings[0].ProofNote != "RETAINED_NATIVE_FIX" {
		t.Fatal("external append changed native decisions", retained)
	}
	var rev int64
	f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, f.project).Scan(&rev)
	req := reviewRequest(rev, nil)
	req.Retired = []RetiredPull{{Number: 1, Reason: "defunct"}}
	historyObservationPost(t, f, req, "external-retired")
	before = historyMigrationDatabaseDigest(t, f)
	if _, changed, err := externalPublish(f, externalRun(2)); err != nil || changed || before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("retired pull reactivated", changed, err)
	}
	if _, err := f.store.DB().Exec(`UPDATE review_hosted_discovery_state SET cutoff_at='not-a-time'`); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExternalHostedFrontiers(t.Context(), f.store.DB()); err == nil {
		t.Fatal("corrupt frontier accepted")
	}
}
func TestExternalHostedSchemaNineMigrationAndIdentityIndex(t *testing.T) {
	f := newAPIFixture(t)
	historyObservationPost(t, f, reviewRequest(0, []PullInput{mixedChannelPull()}), "external-old-nine")
	before := channelCountsPull(t, f, "")
	if _, err := f.store.DB().Exec(`DROP INDEX review_run_versions_external_source; DROP TABLE review_hosted_discovery_state; UPDATE metadata SET value='9' WHERE key='schema:reviews'`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, channelCountsPull(t, f, "")) {
		t.Fatal("v9 migration altered current snapshot")
	}
	var version string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil || version != "10" {
		t.Fatal(version, err)
	}
	externalInit(t, f)
	beforeDigest := historyMigrationDatabaseDigest(t, f)
	if err := EnsureSchema(t.Context(), f.store); err != nil || beforeDigest != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("startup advanced frontier", err)
	}
	rows, err := f.store.DB().Query(`EXPLAIN QUERY PLAN SELECT pull_number,channel,head_sha FROM review_run_versions INDEXED BY review_run_versions_external_source WHERE project_id=? AND source_run_id=? LIMIT 1`, f.project, "run:00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err = rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail
	}
	if rows.Err() != nil || !strings.Contains(plan, "SEARCH review_run_versions USING INDEX review_run_versions_external_source (project_id=? AND source_run_id=?)") {
		t.Fatal(plan, rows.Err())
	}
}

func TestExternalHostedConflictingRetainedIdentityAndEncodedBound(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	r := externalRun(1)
	p.Runs = []RunInput{r}
	historyObservationPost(t, f, reviewRequest(0, []PullInput{p}), "identity-original")
	p.Runs = append([]RunInput(nil), p.Runs...)
	p.Runs[0].HeadSHA = strings.Repeat("e", 40)
	otherSource := reviewRequest(1, []PullInput{p})
	otherSource.Source = "independent-original-import"
	historyObservationPost(t, f, otherSource, "identity-reused")
	externalInit(t, f)
	r.FinishedAt = "2026-10-21T00:00:00Z"
	before := historyMigrationDatabaseDigest(t, f)
	if _, _, err := externalPublish(f, r); err == nil {
		t.Fatal("ambiguous prior source ID accepted")
	}
	oversized := externalRun(2)
	oversized.Found = 100
	oversized.Findings = nil
	for i := 0; i < 100; i++ {
		oversized.Findings = append(oversized.Findings, Finding{SourceFindingID: fmt.Sprint(i), Title: "Valid finding", Detail: strings.Repeat("<", 16<<10), Severity: "low"})
	}
	if _, _, err := externalPublish(f, oversized); err == nil {
		t.Fatal("individually valid escaped findings bypassed trusted detail limit")
	}
	if before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("conflicting or oversized input changed retained history")
	}
}

func TestExternalHostedTrustedLargeInventoryRetainsExactText(t *testing.T) {
	f := newAPIFixture(t)
	initial := historyObservationPost(t, f, reviewRequest(0, []PullInput{nativePull(1)}), "large-external-initial")
	old := nativeSnapshotBytes(t, f, initial.ID)
	externalInit(t, f)
	run := externalRun(20)
	run.Found = 20
	run.Findings = []Finding{}
	for i := 0; i < 20; i++ {
		run.Findings = append(run.Findings, Finding{SourceFindingID: fmt.Sprintf("large-provider-%d", i), Title: "Original complete finding", Detail: fmt.Sprintf("Source %d: ", i) + strings.Repeat("x", 14<<10), Severity: "medium", Disposition: "unadjudicated"})
	}
	body, err := json.Marshal([]RunInput{run})
	if err != nil || len(body) <= maxObservationBytes || len(body) >= maxRunDetailBytes {
		t.Fatal("fixture must exceed import cap and fit trusted detail", len(body), err)
	}
	externalMustPublish(t, f, run)
	var detail RunDetailResponse
	nativeRead(t, f, "/runs/"+runOpaqueID(f.project, externalHostedSource, run.SourceRunID), &detail)
	if detail.Run.Found != 20 || detail.Run.Finalized || len(detail.Findings) != 20 {
		t.Fatal("large external source truncated", detail.Run, len(detail.Findings))
	}
	for i, finding := range detail.Findings {
		if finding.Detail != run.Findings[i].Detail || finding.SourceFindingID != run.Findings[i].SourceFindingID {
			t.Fatal("large source text changed", i)
		}
	}
	if old != nativeSnapshotBytes(t, f, initial.ID) {
		t.Fatal("large publication changed retained snapshot")
	}
	if counts := channelCountsPull(t, f, "").Pull.RunCounts; counts != (ChannelRunCounts{Hosted: 1}) {
		t.Fatal("large run not counted", counts)
	}
}

func externalNearDetailLimit(t *testing.T) RunInput {
	t.Helper()
	run := externalRun(30)
	run.Found = 100
	run.Findings = []Finding{}
	run.CoverageLimits = []string{}
	for i := 0; i < 100; i++ {
		run.Findings = append(run.Findings, Finding{SourceFindingID: fmt.Sprintf("bounded-source-%d", i), Title: "Original finding", Severity: "medium", Disposition: "unadjudicated"})
	}
	body, err := json.Marshal([]RunInput{run})
	if err != nil {
		t.Fatal(err)
	}
	per := (maxRunDetailBytes - 16 - len(body)) / 600
	for i := range run.Findings {
		run.Findings[i].Detail = strings.Repeat("<", per)
	}
	body, err = json.Marshal([]RunInput{run})
	if err != nil {
		t.Fatal(err)
	}
	run.Findings[0].Detail += strings.Repeat("x", maxRunDetailBytes-16-len(body))
	body, err = json.Marshal([]RunInput{run})
	if err != nil || len(body) != maxRunDetailBytes-16 {
		t.Fatal("fixture must pass initial trusted encoding guard", len(body), err)
	}
	return run
}
func TestExternalHostedAuthoritativeDetailCapacityRollsBack(t *testing.T) {
	f := newAPIFixture(t)
	historyObservationPost(t, f, reviewRequest(0, []PullInput{nativePull(1)}), "external-detail-capacity")
	externalInit(t, f)
	before := historyMigrationDatabaseDigest(t, f)
	_, _, err := externalPublish(f, externalNearDetailLimit(t))
	nativeErrorCode(t, err, "workspace_capacity")
	if before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("detail capacity failure leaked snapshots/findings/events")
	}
	externalMustPublish(t, f, externalRun(31))
}
