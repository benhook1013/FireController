package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func TestManagedBlockedAttemptLeavesCoordinationFreeAndCancellationJoins(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	entered := make(chan struct{})
	m.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	m.create = func(ctx context.Context, _ *core.Store, _, _ string) (createdBundle, error) {
		close(entered)
		<-ctx.Done()
		return createdBundle{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	reports := make(chan error, 1)
	go func() { done <- m.Run(ctx, func(err error) { reports <- err }) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("attempt did not enter")
	}
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || status.State != "running" || status.LastAttempt.State != "running" {
		t.Fatal(status, err)
	}
	backupCode(t, m.Run(t.Context(), nil), "backup_running")
	// The snapshot work phase owns no writer transaction or admission slot.
	f.request(t, "POST", "/api/v1/projects/"+f.project+"/messages", map[string]any{"recipient_worker_id": f.worker, "body": "Coordination during a blocked backup"})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not join")
	}
	if len(reports) != 1 {
		t.Fatal("interruption should report once", len(reports))
	}
	status, err = ReadStatus(t.Context(), f.dir)
	if err != nil || status.State != "scheduled" || status.LastAttempt.State != "interrupted" || status.LastSuccess != nil {
		t.Fatal("cancellation invented success", status, err)
	}
}
func TestManagedStatusReadsRetainedFactsWithoutDestinationScan(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	if reports := runManaged(t, m, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	before := backupRows(t, f.store.DB())
	original, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	moved := m.config.Destination + "-offline"
	if err = os.Rename(m.config.Destination, moved); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(moved, m.config.Destination)
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || !reflect.DeepEqual(original, status) || !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("status rescanned/rewrote retained history", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = ReadStatus(ctx, f.dir)
	backupCode(t, err, "backup_unavailable")
}
func TestManagedStatusRejectsCorruptPrivateRecordWithoutRepair(t *testing.T) {
	for _, kind := range []string{"unknown-field", "missing-field", "null-scalar", "invalid-state", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			f, _ := newManagedFixture(t, 1)
			var original string
			if f.store.DB().QueryRow(`SELECT body_json FROM managed_backup_state`).Scan(&original) != nil {
				t.Fatal("fixture")
			}
			var shape map[string]any
			if json.Unmarshal([]byte(original), &shape) != nil {
				t.Fatal("fixture shape")
			}
			s := shape["status"].(map[string]any)
			switch kind {
			case "unknown-field":
				s["private_extra"] = "ignored?"
			case "missing-field":
				delete(s, "keep")
			case "null-scalar":
				s["keep"] = nil
			case "invalid-state":
				s["state"] = "success"
			case "oversized":
				s["destination"] = string(make([]byte, 17000))
			}
			body, _ := json.Marshal(shape)
			if _, err := f.store.DB().Exec(`UPDATE managed_backup_state SET body_json=?`, string(body)); err != nil {
				t.Fatal(err)
			}
			before := backupRows(t, f.store.DB())
			_, err := ReadStatus(t.Context(), f.dir)
			backupCode(t, err, "backup_invalid")
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
				t.Fatal("status repaired corruption")
			}
		})
	}
}
func TestManagedRestoreKeepsScheduleInactive(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	if reports := runManaged(t, m, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "inactive")
	if _, err = Restore(t.Context(), status.LastSuccess.Path, dest); err != nil {
		t.Fatal(err)
	}
	db, err := openDatabase(t.Context(), filepath.Join(dest, "firecontroller.sqlite"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM managed_backup_state`).Scan(&count); err != nil || count != 1 {
		t.Fatal("restored schedule history missing", err)
	}
	// No activation/worker is attempted; trusted startup must reject first.
	restored, err := core.Open(t.Context(), dest)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	_, err = NewManager(restored, dest, m.config)
	backupCode(t, err, "restore_inactive")
}
func TestManagedExplicitSchemaRemainsOptionalAndIdempotent(t *testing.T) {
	f := newRestoreFixture(t)
	if err := EnsureManagedSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	before := backupRows(t, f.store.DB())
	if err := EnsureManagedSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("migration rewrote facts")
	}
	var rows int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM managed_backup_state`).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("schema invented configuration")
	}
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || status.State != "unconfigured" {
		t.Fatal(status, err)
	}
	// Corrupt future schema stays a compatibility refusal, never a migration read.
	if _, err = f.store.DB().Exec(`UPDATE metadata SET value='2' WHERE key='schema:managed_backups'`); err != nil {
		t.Fatal(err)
	}
	_, err = ReadStatus(t.Context(), f.dir)
	backupCode(t, err, "backup_invalid")
}
