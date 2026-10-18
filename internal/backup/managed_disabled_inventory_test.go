package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func addManagedDisabledExtension(t *testing.T, f *restoreFixture) {
	t.Helper()
	// A separately owned component can be current for the daemon while the
	// backup inventory has not yet learned how to preserve its schema.
	if err := f.store.EnsureSchema(t.Context(), "coordination_extension", []string{
		`CREATE TABLE coordination_extension(id TEXT PRIMARY KEY,body TEXT NOT NULL)`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO coordination_extension VALUES('retained','unchanged')`); err != nil {
		t.Fatal(err)
	}
}

func TestManagedDisabledDoesNotRequireBackupInventory(t *testing.T) {
	f := newRestoreFixture(t)
	addManagedDisabledExtension(t, f)
	m, err := NewManager(f.store, f.dir, ScheduleConfig{})
	if err != nil {
		t.Fatal("disabled startup required an unrelated backup inventory", err)
	}
	m.create = func(context.Context, *core.Store, string, string) (createdBundle, error) {
		t.Fatal("disabled manager attempted a backup")
		return createdBundle{}, nil
	}
	m.wait = func(context.Context, time.Duration) error { t.Fatal("disabled manager scheduled work"); return nil }
	if err := m.Run(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	record, err := loadManaged(t.Context(), f.store.DB())
	if err != nil || record == nil {
		t.Fatal("disabled record", record, err)
	}
	if status := record.Status; status.State != "disabled" || status.LastAttempt != nil || status.NextAttemptAt != "" {
		t.Fatal("disabled status", status)
	}
	before := backupRows(t, f.store.DB())
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || !reflect.DeepEqual(status, record.Status) || !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("read-only disabled status required an unrelated backup inventory or changed rows", status, err)
	}
	cfg := ScheduleConfig{Destination: filepath.Join(t.TempDir(), "enabled"), Interval: time.Minute, Keep: 1}
	_, err = NewManager(f.store, f.dir, cfg)
	backupCode(t, err, "backup_invalid")
	backupCode(t, ValidateSource(t.Context(), f.dir), "backup_invalid")
	manual := filepath.Join(t.TempDir(), "manual")
	_, err = Create(t.Context(), f.store, f.dir, manual)
	backupCode(t, err, "backup_invalid")
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("rejected backup or enabled startup changed retained rows")
	}
	if _, err := os.Lstat(cfg.Destination); !os.IsNotExist(err) {
		t.Fatal("rejected enabled startup created a destination", err)
	}
	if entries, err := os.ReadDir(manual); !os.IsNotExist(err) && (err != nil || len(entries) != 0) {
		t.Fatal("rejected manual backup left a bundle or partial files", entries, err)
	}
}

func TestManagedDisabledReconcilesInterruptedWorkBeyondBackupInventory(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	if reports := runManaged(t, m, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	prior, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	bundle := restoreTree(t, prior.LastSuccess.Path)
	if err := m.update(t.Context(), func(r *managedRecord, tx *sql.Tx) error {
		r.Status.State, r.Status.NextAttemptAt = "running", ""
		r.Status.LastAttempt = &AttemptStatus{StartedAt: managedStamp(time.Now().UTC().Add(-time.Minute)), State: "running", SourceInstanceID: f.store.InstanceID()}
		_, err := tx.ExecContext(t.Context(), `UPDATE managed_backup_bundles SET state='deleting'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	addManagedDisabledExtension(t, f)
	m, err = NewManager(f.store, f.dir, ScheduleConfig{})
	if err != nil {
		t.Fatal("disabled reconciliation required backup inventory", err)
	}
	record, err := loadManaged(t.Context(), f.store.DB())
	if err != nil || record == nil {
		t.Fatal("disabled reconciled record", record, err)
	}
	if status := record.Status; status.State != "disabled" || status.NextAttemptAt != "" || status.LastAttempt == nil || status.LastAttempt.State != "interrupted" || status.LastAttempt.Reason != "backup_interrupted" || status.LastAttempt.SourceInstanceID != f.store.InstanceID() || !reflect.DeepEqual(status.LastSuccess, prior.LastSuccess) {
		t.Fatal("disabled reconciliation lost retained history", status)
	}
	beforeStatus := backupRows(t, f.store.DB())
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || !reflect.DeepEqual(status, record.Status) || !reflect.DeepEqual(beforeStatus, backupRows(t, f.store.DB())) {
		t.Fatal("disabled status lost retained attempt/success or required unrelated inventory", status, err)
	}
	var state string
	if err := f.store.DB().QueryRow(`SELECT state FROM managed_backup_bundles WHERE id=?`, prior.LastSuccess.ID).Scan(&state); err != nil || state != "cleanup_failed" {
		t.Fatal("interrupted deletion not retained", state, err)
	}
	if !reflect.DeepEqual(bundle, restoreTree(t, prior.LastSuccess.Path)) {
		t.Fatal("disabled reconciliation changed the retained bundle")
	}
	if err := m.Run(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestManagedDisabledStillRequiresInstallationGuards(t *testing.T) {
	for _, scenario := range []string{"incomplete", "inactive", "different_store"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRestoreFixture(t)
			store := f.store
			code := "restore_inactive"
			switch scenario {
			case "incomplete":
				if err := os.WriteFile(filepath.Join(f.dir, restoreMarker), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "inactive":
				if _, err := f.store.DB().Exec(`INSERT INTO metadata(key,value) VALUES('restore:state','inactive')`); err != nil {
					t.Fatal(err)
				}
			case "different_store":
				store, code = newRestoreFixture(t).store, "backup_invalid"
			}
			_, err := NewManager(store, f.dir, ScheduleConfig{})
			backupCode(t, err, code)
		})
	}
}
