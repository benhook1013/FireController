package backup

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestManagedRetentionRestartAfterBetweenPassClockRollback(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	if err := m.attempt(t.Context()); err != nil {
		t.Fatal(err)
	}
	first, err := ReadStatus(t.Context(), f.dir)
	if err != nil || first.LastSuccess == nil || first.Retention == nil || first.Retention.State != "complete" || first.State != "scheduled" {
		t.Fatal("first real backup did not settle", err)
	}
	firstBytes := restoreTree(t, first.LastSuccess.Path)
	// A genuinely settled result survives ordinary startup unchanged.
	m, err = NewManager(f.store, f.dir, m.config)
	if err != nil {
		t.Fatal(err)
	}
	settled, err := ReadStatus(t.Context(), f.dir)
	if err != nil || !reflect.DeepEqual(settled.Retention, first.Retention) {
		t.Fatal("restart changed settled pruning", err)
	}
	priorAt, err := time.Parse(time.RFC3339Nano, first.Retention.AttemptedAt)
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return priorAt.Add(-time.Hour) }
	// This existing removal seam runs AFTER real creation/registration, keeper
	// verification and durable deletion admission, but BEFORE any unlink. A panic
	// models loss of the worker before its terminal retention-result transaction.
	stopped := new(int)
	var pending Status
	m.remove = func(context.Context, string, Manifest) error {
		var readErr error
		pending, readErr = ReadStatus(t.Context(), f.dir)
		if readErr != nil {
			t.Error("pending status became unreadable", readErr)
		}
		panic(stopped)
	}
	var recovered any
	func() { defer func() { recovered = recover() }(); _ = m.attempt(t.Context()) }()
	if recovered != stopped {
		t.Fatal("did not reach real pruning crash boundary")
	}
	if pending.State != "running" || pending.LastAttempt == nil || pending.LastAttempt.State != "complete" || pending.LastSuccess == nil || pending.LastSuccess.ID == first.LastSuccess.ID {
		t.Fatal("did not retain the new registered creation")
	}
	if pending.Retention != nil {
		t.Error("previous pruning result masquerades during the new retention phase")
	}
	finish, err := time.Parse(time.RFC3339Nano, pending.LastAttempt.FinishedAt)
	if err != nil || !finish.Before(priorAt) {
		t.Fatal("fixture did not reproduce between-pass rollback", err)
	}
	newestBytes := restoreTree(t, pending.LastSuccess.Path)
	_, err = NewManager(f.store, f.dir, m.config)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Retention == nil || restarted.Retention.State != "failed" || restarted.Retention.Reason != "backup_retention_interrupted" {
		t.Error("restart hid the interrupted pruning behind an older wall-clock result")
	}
	if !reflect.DeepEqual(restarted.LastSuccess, pending.LastSuccess) || !reflect.DeepEqual(restarted.LastAttempt, pending.LastAttempt) {
		t.Fatal("restart altered the completed creation")
	}
	var oldState, newState string
	if err = f.store.DB().QueryRowContext(t.Context(), `SELECT state FROM managed_backup_bundles WHERE id=?`, first.LastSuccess.ID).Scan(&oldState); err != nil {
		t.Fatal(err)
	}
	if err = f.store.DB().QueryRowContext(t.Context(), `SELECT state FROM managed_backup_bundles WHERE id=?`, pending.LastSuccess.ID).Scan(&newState); err != nil {
		t.Fatal(err)
	}
	if oldState != "cleanup_failed" || newState != "retained" {
		t.Fatal("interrupted deletion lost ownership or its hold", oldState, newState)
	}
	if !reflect.DeepEqual(firstBytes, restoreTree(t, first.LastSuccess.Path)) || !reflect.DeepEqual(newestBytes, restoreTree(t, pending.LastSuccess.Path)) {
		t.Fatal("stop/restart changed either verified bundle")
	}
	if _, err = Inspect(t.Context(), pending.LastSuccess.Path); err != nil {
		t.Fatal("newest backup is not independently usable", err)
	}
}
