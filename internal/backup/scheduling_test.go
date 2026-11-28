package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestSchedulingBackupRetainsCanonicalProjectRow(t *testing.T) {
	f := newRestoreFixture(t)
	// Model previously accepted settings in the native schema. The foundation
	// has no project write path and this fixture does not synthesize a selection.
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var credential string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM credentials WHERE principal_id=? AND is_admin=1`, f.principal).Scan(&credential); err != nil {
			return err
		}
		at := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_scheduling
			(project_id,revision,automatic_hosted,automatic_cli,actor_id,worker_id,credential_id,updated_at)
			VALUES(?,1,1,0,?,?,?,?)`, f.project, f.principal, f.worker, credential, at); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO events(project_id,event_type,resource_id,revision,actor_id,payload_json,created_at)
			VALUES(?,'review.scheduling_updated',?,1,?,'{"automatic_hosted":true,"automatic_cli":false,"hosted_basis_revision":1,"cli_basis_revision":1}',?)`, f.project, f.project, f.principal, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	const controls = `SELECT * FROM review_scheduling`
	const journal = `SELECT * FROM events WHERE event_type='review.scheduling_updated' ORDER BY sequence`
	wantControls, wantJournal := restoreRows(t, f.store.DB(), controls), restoreRows(t, f.store.DB(), journal)
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-project-scheduling")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), destination)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	status, err := ReadRecoveryStatus(t.Context(), store)
	if err != nil || status.State != "inactive" || !status.ProvidersHeld {
		t.Fatal("restored settings unlocked provider work", status, err)
	}
	for _, db := range []*sql.DB{store.DB(), f.store.DB()} {
		if !reflect.DeepEqual(wantControls, restoreRows(t, db, controls)) || !reflect.DeepEqual(wantJournal, restoreRows(t, db, journal)) {
			t.Fatal("backup/restore changed prior scheduling authority")
		}
	}
	if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("restore changed the validated bundle")
	}
}

func TestSchedulingRestorePreservesHistoricalPullResumeRevision(t *testing.T) {
	f := newRestoreFixture(t)
	e := selectionInventoryEngine(t, f.store)
	scope := routePolicyInventoryScope(t, f)
	zero, one := int64(0), int64(1)
	paused, resumed := true, false
	for _, pull := range []int64{2, 4} {
		if _, err := e.ConfigurePullScheduling(t.Context(), scope, "initial-pull-pause", pull,
			reviewexec.PullSchedulingRequest{ExpectedRevision: &zero, Paused: &paused, Reason: "Fixture pause"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.ConfigurePullScheduling(t.Context(), scope, "historical-pull-resume", 4,
		reviewexec.PullSchedulingRequest{ExpectedRevision: &one, Paused: &resumed}); err != nil {
		t.Fatal(err)
	}
	event := restoreRows(t, f.store.DB(), `SELECT project_id,4,revision,0,'',actor_id,created_at
		FROM events WHERE event_type='review.pull_scheduling_updated' AND resource_id='4' AND revision=2`)
	if len(event) != 1 || len(event[0]) != 7 {
		t.Fatalf("expected one historical resume event, got %v", event)
	}
	if _, err := f.store.DB().Exec(`DELETE FROM review_pull_scheduling WHERE project_id=? AND pull_number=4`, f.project); err != nil {
		t.Fatal(err)
	}
	const controls = `SELECT project_id,pull_number,revision,paused,reason,actor_id,updated_at FROM review_pull_scheduling ORDER BY pull_number`
	const journal = `SELECT * FROM events ORDER BY sequence`
	sourceControls, sourceJournal := restoreRows(t, f.store.DB(), controls), restoreRows(t, f.store.DB(), journal)
	if len(sourceControls) != 1 {
		t.Fatalf("expected one untouched control, got %v", sourceControls)
	}
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("valid legacy bundle no longer inspectable", err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-resume")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal("valid legacy bundle no longer restorable", err)
	}
	dst, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	want := [][]any{sourceControls[0], event[0]}
	if got := restoreRows(t, dst, controls); !reflect.DeepEqual(want, got) {
		t.Fatalf("restored controls lost canonical revision or attribution: got %v, want %v", got, want)
	}
	if !reflect.DeepEqual(sourceJournal, restoreRows(t, dst, journal)) {
		t.Fatal("offline restore appended or rewrote historical events")
	}
	if !reflect.DeepEqual(sourceControls, restoreRows(t, f.store.DB(), controls)) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("restore mutated source controls or validated bundle")
	}
}

func TestSchedulingInventoryAcceptsLegacyHostedAndItsActualMigration(t *testing.T) {
	f := newRestoreFixture(t)
	db := f.store.DB()
	if _, err := db.Exec(`DROP TABLE review_scheduling;
		UPDATE metadata SET value='1' WHERE key='schema:review_scheduling';` + reviewexec.SchedulingSchemaSQL()[0]); err != nil {
		t.Fatal(err)
	}
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var credential string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM credentials WHERE principal_id=? AND is_admin=1`, f.principal).Scan(&credential); err != nil {
			return err
		}
		at := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_scheduling
			(project_id,revision,automatic_hosted,actor_id,worker_id,credential_id,updated_at)
			VALUES(?,1,1,?,?,?,?)`, f.project, f.principal, f.worker, credential, at); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO events(project_id,event_type,resource_id,revision,actor_id,payload_json,created_at)
			VALUES(?,'review.scheduling_updated',?,1,?,'{"automatic_hosted":true}',?)`, f.project, f.project, f.principal, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(t.Context(), restoreBundle(t, f).Path); err != nil {
		t.Fatal("legacy Hosted scheduling inventory refused", err)
	}
	if err := reviewexec.EnsureSchedulingSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	var hosted, cli bool
	if err := db.QueryRow(`SELECT automatic_hosted,automatic_cli FROM review_scheduling WHERE project_id=?`, f.project).Scan(&hosted, &cli); err != nil || !hosted || cli {
		t.Fatal("migration changed legacy scheduling authority", hosted, cli, err)
	}
	if _, err := Inspect(t.Context(), restoreBundle(t, f).Path); err != nil {
		t.Fatal("actual migrated scheduling inventory refused", err)
	}
}
