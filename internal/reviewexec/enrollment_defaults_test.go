package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/roundadoptionmodel"
)

// Existing lifecycle tests start from the authentic predecessor format: one
// configuration receipt/event without adding per-channel round controls. New product
// enrollment tests below always call the real Engine.Configure directly.
func seedLegacyExecutionConfiguration(t *testing.T, f *attemptFixture, project, key string, c Configuration) {
	t.Helper()
	if c.ExpectedRevision == nil || *c.ExpectedRevision != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_config WHERE project_id=?`, project) != 0 {
		t.Fatal("legacy execution seed requires a fresh predecessor fixture")
	}
	if err := f.e.validateConfiguration(c); err != nil {
		t.Fatal("legacy execution seed requires a valid predecessor configuration", err)
	}
	scope := f.scope(t, project, f.admin, true)
	_, err := f.store.Mutate(t.Context(), scope, core.Mutation{Operation: "reviews.configure", Target: "configuration", Key: key, Request: c}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_execution_config(project_id,revision,body_json) VALUES(?,1,?)`, project, encoded(c)); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "reviews.configured", project, 1, map[string]any{"revision": int64(1), "paused": c.Paused}); err != nil {
			return nil, err
		}
		return Receipt{ID: project, State: "configured", Revision: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func enrollmentConfiguration(previous int64, queue []int64, channels []string) Configuration {
	return Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "independent", Queue: queue, Channels: channels}
}

func enrollmentState(t *testing.T, f *attemptFixture) string {
	t.Helper()
	rows, err := f.store.DB().Query(`SELECT project_id,revision,body_json FROM review_execution_config ORDER BY project_id,revision`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var records []string
	for rows.Next() {
		var p, b string
		var r int64
		if err := rows.Scan(&p, &r, &b); err != nil {
			t.Fatal(err)
		}
		records = append(records, fmt.Sprintf("%s/%d/%s", p, r, b))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded(records) + roundTables(t, f)
}

func assertEnrollmentDefault(t *testing.T, f *attemptFixture, pull int64, channel string) RoundControl {
	t.Helper()
	c := roundRead(t, f, pull, channel)
	maximum, taper := int64(6), int64(3)
	if channel == "hosted" {
		maximum, taper = 3, 1
	}
	if !c.Configured || c.Revision != 1 || c.Epoch != 1 || c.Minimum != 2 || c.Maximum != maximum || c.Taper != taper || c.Used != 0 || c.Dry != 0 || c.Closed || c.Tapered || c.OriginSource != "" {
		t.Fatalf("incorrect new enrollment default %d/%s: %+v", pull, channel, c)
	}
	return c
}

func TestEnrollmentDefaultsAreAtomicImmediateAndReplayDoesNotGrantAgain(t *testing.T) {
	f := newAttemptFixture(t)
	c := enrollmentConfiguration(0, []int64{2, 4}, []string{"hosted", "cli"})
	scope := f.scope(t, attemptProject, f.admin, true)
	first, err := f.e.Configure(t.Context(), scope, "new-enrollment", c)
	if err != nil {
		t.Fatal(err)
	}
	for _, pull := range c.Queue {
		for _, channel := range c.Channels {
			assertEnrollmentDefault(t, f, pull, channel)
		}
	}
	if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("new enrollment retained invalid round evidence", err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_round_changes`) != 4 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='reviews.rounds_configured'`) != 4 {
		t.Fatal("enrollment omitted or duplicated channel allocations")
	}
	before := enrollmentState(t, f)
	replay, err := f.e.Configure(t.Context(), scope, "new-enrollment", c)
	if err != nil || !bytes.Equal(first, replay) || enrollmentState(t, f) != before {
		t.Fatal("exact enrollment replay changed state", err)
	}
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_evidence", "review_round_operation_bindings", "review_round_completions"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("enrollment performed provider/review work", table)
		}
	}
}

func TestEnrollmentAddsOnlyNewPairsAndPreservesExistingEdits(t *testing.T) {
	f := newAttemptFixture(t)
	scope := f.scope(t, attemptProject, f.admin, true)
	initial := enrollmentConfiguration(0, []int64{2}, []string{"cli"})
	initialReceipt, err := f.e.Configure(t.Context(), scope, "initial", initial)
	if err != nil {
		t.Fatal(err)
	}
	one, zero, extra := int64(1), int64(0), int64(5)
	if _, err := f.e.ConfigureRoundControl(t.Context(), scope, "explicit-allocation", 2, "cli", RoundControlRequest{ExpectedRevision: &one, Action: "grant", Minimum: &zero, Maximum: &extra}); err != nil {
		t.Fatal(err)
	}
	retained := roundRead(t, f, 2, "cli")
	next := enrollmentConfiguration(1, []int64{4, 2}, []string{"cli", "hosted"})
	next.Paused = true
	next.CapabilityID = "other-account"
	if _, err := f.e.Configure(t.Context(), scope, "late-enrollment", next); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundRead(t, f, 2, "cli"), retained) {
		t.Fatal("late enrollment overwrote explicit existing allocation")
	}
	for _, pair := range []struct {
		pull    int64
		channel string
	}{{4, "cli"}, {4, "hosted"}, {2, "hosted"}} {
		assertEnrollmentDefault(t, f, pair.pull, pair.channel)
	}
	before := roundTables(t, f)
	edit := enrollmentConfiguration(2, []int64{2, 4}, []string{"hosted", "cli"})
	edit.Paused = false
	if _, err := f.e.Configure(t.Context(), scope, "order-pause-capability-edit", edit); err != nil {
		t.Fatal(err)
	}
	// Configuration has its own event/receipt; only allocation rows must remain byte-identical.
	if !reflect.DeepEqual(roundRead(t, f, 2, "cli"), retained) || attemptCount(t, f.store, `SELECT count(*) FROM review_round_changes`) != 5 {
		t.Fatal("ordinary configuration edit granted new rounds")
	}
	if before == roundTables(t, f) {
		t.Fatal("configuration edit was not actually recorded")
	}
	beforeReplay := enrollmentState(t, f)
	replayed, err := f.e.Configure(t.Context(), scope, "initial", initial)
	if err != nil || !bytes.Equal(replayed, initialReceipt) || enrollmentState(t, f) != beforeReplay {
		t.Fatal("original configuration receipt retry changed later allocations", err)
	}
}

func TestEnrollmentPreservesExplicitPreparedAdoptionBeforeExecutionExists(t *testing.T) {
	f := newAttemptFixture(t)
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'adoption-worker','Importer')`, attemptProject); err != nil {
		t.Fatal(err)
	}
	p := adoptionPrepared()
	p.MinimumRemaining = 1
	p.MaximumRemaining = 9
	admin := adoptionScope(t, f, f.admin, true)
	preview, err := f.e.PreviewRoundAdoption(t.Context(), admin, p)
	if err != nil {
		t.Fatal(err)
	}
	input := roundadoptionmodel.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Control: p}
	if _, err := f.e.ApplyRoundAdoption(t.Context(), admin, "before-enrollment", input); err != nil {
		t.Fatal(err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_config`) != 0 {
		t.Fatal("adoption implicitly configured execution")
	}
	retained := roundRead(t, f, 2, "cli")
	zero, minimum, maximum := int64(0), int64(1), int64(9)
	if _, err := f.e.ConfigureRoundControl(t.Context(), admin, "explicit-before-enrollment", 4, "hosted", RoundControlRequest{ExpectedRevision: &zero, Action: "grant", Minimum: &minimum, Maximum: &maximum}); err != nil {
		t.Fatal(err)
	}
	explicit := roundRead(t, f, 4, "hosted")
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "after-import", enrollmentConfiguration(0, []int64{2, 4}, []string{"cli", "hosted"})); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundRead(t, f, 2, "cli"), retained) {
		t.Fatal("enrollment replaced adopted policy/provenance")
	}
	assertEnrollmentDefault(t, f, 2, "hosted")
	assertEnrollmentDefault(t, f, 4, "cli")
	if !reflect.DeepEqual(roundRead(t, f, 4, "hosted"), explicit) {
		t.Fatal("enrollment replaced a preexisting explicit control")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_round_adoptions`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_round_changes`) != 4 {
		t.Fatal("adoption and default were duplicated")
	}
	if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("prepared adoption and native defaults formed invalid retained state", err)
	}
}

func TestEnrollmentDefaultsRollbackWithConfigurationAndRejectNonAdmin(t *testing.T) {
	f := newAttemptFixture(t)
	c := enrollmentConfiguration(0, []int64{2}, []string{"hosted", "cli"})
	before := enrollmentState(t, f)
	_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.writer, false), "not-admin", c)
	attemptCode(t, err, "forbidden")
	if enrollmentState(t, f) != before {
		t.Fatal("non-admin enrollment changed state")
	}
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_enrollment_event BEFORE INSERT ON events WHEN NEW.event_type='reviews.configured' BEGIN SELECT RAISE(ABORT,'fixture');END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "atomic-enrollment", c); err == nil {
		t.Fatal("injected configuration failure succeeded")
	}
	if enrollmentState(t, f) != before {
		t.Fatal("failed configuration retained partial default grants/events/receipt")
	}
}

func TestEnrollmentUpgradeDoesNotBackfillExistingUnconfiguredPairs(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	for _, pull := range []int64{2, 4} {
		for _, channel := range []string{"cli", "hosted"} {
			if roundRead(t, f, pull, channel).Configured {
				t.Fatal("legacy fixture already had new allocations")
			}
		}
	}
	edit := enrollmentConfiguration(1, []int64{4, 2}, []string{"hosted", "cli"})
	edit.Paused = true
	edit.CapabilityID = "other-account"
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "upgrade-edit", edit); err != nil {
		t.Fatal(err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_round_changes`) != 0 {
		t.Fatal("ordinary upgrade edit backfilled existing pair")
	}
}
