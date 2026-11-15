package backup

import (
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
)

type settledDeadlineContext struct {
	context.Context
	expired atomic.Bool
}

func (c *settledDeadlineContext) Err() error {
	if c.expired.Load() {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

func TestRetentionDeadlineAfterSettledDeleteIsDeferred(t *testing.T) {
	m, b, _ := retentionCancellationFixture(t)
	ctx := &settledDeadlineContext{Context: t.Context()}
	m.remove = func(c context.Context, p string, v Manifest) error {
		if e := removeManagedBundle(c, p, v); e != nil {
			return e
		}
		ctx.expired.Store(true)
		return nil
	}
	if e := m.retain(ctx); !errors.Is(e, errManagedRetentionDeferred) {
		t.Fatal(e)
	}
	if retentionBundleState(t, m, b) != "deleted" {
		t.Fatal("settled deletion lost")
	}
	if e := m.retain(t.Context()); e != nil {
		t.Fatal("fresh revalidation failed", e)
	}
}

func deferredFixture(t *testing.T) (*restoreFixture, *Manager) {
	t.Helper()
	f, m := newManagedFixture(t, 3)
	if r := runManaged(t, m, 2); len(r) != 0 {
		t.Fatal(r)
	}
	cfg := m.config
	cfg.Keep = 1
	var e error
	m, e = NewManager(f.store, f.dir, cfg)
	if e != nil {
		t.Fatal(e)
	}
	e = m.update(t.Context(), func(r *managedRecord, _ *sql.Tx) error {
		r.Status.Retention = &RetentionStatus{AttemptedAt: managedStamp(m.now()), State: "failed", Reason: "backup_retention_failed"}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return f, m
}

func TestDeferredMaintenanceCompletesBeforeNewSnapshot(t *testing.T) {
	f, m := deferredFixture(t)
	calls := 0
	original := m.create
	m.create = func(c context.Context, s *core.Store, d, p string) (createdBundle, error) {
		calls++
		var n int
		if e := s.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE state='retained'`).Scan(&n); e != nil || n != 1 {
			t.Fatalf("new snapshot before maintenance: %d %v", n, e)
		}
		return original(c, s, d, p)
	}
	if e := m.attempt(t.Context()); e != nil {
		t.Fatal(e)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	status, e := ReadStatus(t.Context(), f.dir)
	if e != nil || status.Retention.State != "complete" || status.LastAttempt.State != "complete" {
		t.Fatal(status, e)
	}
}

func TestDeferredMaintenanceTrueErrorBlocksNewGrowth(t *testing.T) {
	f, m := deferredFixture(t)
	bundles := retainedIntegrityBundles(t, m)
	if e := os.WriteFile(filepath.Join(bundles[0].destination, bundles[0].id, "manifest.json"), []byte("corrupt keeper"), 0600); e != nil {
		t.Fatal(e)
	}
	m.create = func(context.Context, *core.Store, string, string) (createdBundle, error) {
		t.Fatal("created despite failed keeper proof")
		return createdBundle{}, nil
	}
	for i := 0; i < 2; i++ {
		if e := m.attempt(t.Context()); e == nil {
			t.Fatal("true corruption masked")
		}
	}
	status, e := ReadStatus(t.Context(), f.dir)
	if e != nil || status.Retention.State != "failed" || status.Retention.Reason != "backup_retention_failed" {
		t.Fatal(status, e)
	}
}

func TestDeferredMaintenanceCancellationDebtSurvivesRestart(t *testing.T) {
	f, m := deferredFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	originalRemove := m.remove
	m.remove = func(c context.Context, p string, v Manifest) error {
		status, e := ReadStatus(t.Context(), f.dir)
		if e != nil || status.State != "running" || status.NextAttemptAt != "" {
			t.Fatal("maintenance not visibly active", status, e)
		}
		if e := originalRemove(c, p, v); e != nil {
			return e
		}
		cancel()
		return nil
	}
	m.create = func(context.Context, *core.Store, string, string) (createdBundle, error) {
		t.Fatal("growth during cancelled maintenance")
		return createdBundle{}, nil
	}
	if e := m.attempt(ctx); e == nil {
		t.Fatal("cancellation hidden")
	}
	before, e := ReadStatus(t.Context(), f.dir)
	if e != nil || before.Retention.State != "failed" {
		t.Fatal("debt lost", before, e)
	}
	restarted, e := NewManager(f.store, f.dir, m.config)
	if e != nil {
		t.Fatal(e)
	}
	after, e := ReadStatus(t.Context(), f.dir)
	if e != nil || after.Retention.State != "failed" {
		t.Fatal("restart lost debt", after, e)
	}
	if e := restarted.attempt(t.Context()); e != nil {
		t.Fatal("healthy retry could not resume creation", e)
	}
	final, e := ReadStatus(t.Context(), f.dir)
	if e != nil || final.Retention.State != "complete" || final.LastSuccess.ID == before.LastSuccess.ID {
		t.Fatal("maintenance did not settle before healthy growth", final, e)
	}
}

func TestIncompleteMaintenanceLegacyWithoutCreationHistory(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "owned"}[owned], func(t *testing.T) {
			f, m := newManagedFixture(t, 2)
			if owned {
				if r := runManaged(t, m, 1); len(r) != 0 {
					t.Fatal(r)
				}
			}
			if e := m.update(t.Context(), func(r *managedRecord, _ *sql.Tx) error {
				r.Status.LastAttempt = nil
				r.Status.Retention = &RetentionStatus{AttemptedAt: managedStamp(m.now()), State: "failed", Reason: "backup_retention_failed"}
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			if owned {
				m.create = func(context.Context, *core.Store, string, string) (createdBundle, error) {
					t.Fatal("invented creation history bypassed custody")
					return createdBundle{}, nil
				}
			}
			e := m.attempt(t.Context())
			if (e != nil) != owned {
				t.Fatal("legacy outcome", owned, e)
			}
			status, e := ReadStatus(t.Context(), f.dir)
			if e != nil || status.State == "running" {
				t.Fatal(status, e)
			}
			if owned && status.LastAttempt != nil {
				t.Fatal("invented history")
			}
			if !owned && (status.LastAttempt == nil || status.LastAttempt.State != "complete") {
				t.Fatal("healthy empty legacy record blocked", status)
			}
		})
	}
}

func TestIncompleteMaintenanceDestinationChangePreservesOldFiles(t *testing.T) {
	f, m := deferredFixture(t)
	oldDestination := m.config.Destination
	before := restoreTree(t, oldDestination)
	cfg := m.config
	cfg.Destination = filepath.Join(t.TempDir(), "new-backups")
	changed, e := NewManager(f.store, f.dir, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if e = changed.attempt(t.Context()); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(before, restoreTree(t, oldDestination)) {
		t.Fatal("changed destination pruned prior scope")
	}
	var n int
	if e = f.store.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE destination=? AND state='retained'`, cfg.Destination).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}
