package retention

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backupphase"
	"github.com/benhook1013/FireController/internal/core"
)

func TestRetentionWaitsForFirstConfiguredTickAndJoinsShutdown(t *testing.T) {
	const interval = 75 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// A missing preprovisioned lease reports an error without opening a DB.
	cfg := Config{Interval: interval, ReviewHistory: DefaultHistory, JobHistory: DefaultHistory, Receipts: DefaultHistory, Diagnostics: DefaultDiagnostics, PhaseLease: filepath.Join(t.TempDir(), "missing.lock")}
	failures := make(chan error, 2)
	start := time.Now()
	done := Run(ctx, nil, cfg, nil, func(err error) { failures <- err })
	select {
	case err := <-failures:
		if elapsed := time.Since(start); elapsed < interval || !errors.Is(err, backupphase.ErrInvalid) {
			t.Fatalf("first tick after %s: %v", elapsed, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("configured first pass did not run")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not join on shutdown")
	}
	select {
	case err := <-failures:
		t.Fatalf("shutdown or unscheduled retry emitted a failure: %v", err)
	default:
	}
}

func TestRetentionBusyLeaseDoesNotRecordOrTouchDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "phase.lock")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	release, err := backupphase.Acquire(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = backupphase.TryAcquire(t.Context(), path); !errors.Is(err, backupphase.ErrBusy) {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := Config{Interval: DefaultInterval, ReviewHistory: DefaultHistory, JobHistory: DefaultHistory, Receipts: DefaultHistory, Diagnostics: DefaultDiagnostics, PhaseLease: path}
	r, skip, err := RunPass(t.Context(), store, cfg, time.Now())
	if err != nil || !skip || r.StartedAt != "" {
		t.Fatal(r, skip, err)
	}
	var count int
	if err = store.DB().QueryRow(`SELECT count(*) FROM metadata WHERE key=?`, LastPassKey).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestRetentionDiagnosticsExactCutoffAndDryRun(t *testing.T) {
	store, err := core.Open(t.Context(), filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.DB().Exec(`CREATE TABLE review_selection_decision_history(sequence INTEGER PRIMARY KEY,last_seen_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	for i, d := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		if _, err = store.DB().Exec(`INSERT INTO review_selection_decision_history VALUES(?,?)`, i+1, base.Add(d).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	p := &pass{store: store, dryRun: true, deleted: map[string]int64{}}
	const table = "review_selection_decision_history"
	if err = pruneDiagnostics(t.Context(), p, base); err != nil || p.deleted[table] != 1 {
		t.Fatal(p.deleted, err)
	}
	var remaining int
	if err = store.DB().QueryRow(`SELECT count(*) FROM review_selection_decision_history`).Scan(&remaining); err != nil || remaining != 3 {
		t.Fatal(remaining, err)
	}
	p.dryRun = false
	p.deleted = map[string]int64{}
	if err = pruneDiagnostics(t.Context(), p, base); err != nil || p.deleted[table] != 1 {
		t.Fatal(p.deleted, err)
	}
	if err = store.DB().QueryRow(`SELECT count(*) FROM review_selection_decision_history`).Scan(&remaining); err != nil || remaining != 2 {
		t.Fatal(remaining, err)
	}
	if reportFinish(base, base.Add(-time.Second)) != stamp(base) {
		t.Fatal("backdated status")
	}
}

func TestRetentionVacuumConversionOnceAndReclamation(t *testing.T) {
	store, err := core.Open(t.Context(), filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mode := func() int {
		t.Helper()
		var n int
		if err := store.DB().QueryRow(`PRAGMA auto_vacuum`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := mode(); got != 0 {
		t.Fatal("fixture did not start in normal mode", got)
	}
	if err = store.EnsureIncrementalVacuum(t.Context()); err != nil || mode() != 2 {
		t.Fatal(err)
	}
	if _, err = store.DB().Exec(`CREATE TABLE synthetic_free_space(x BLOB);
WITH RECURSIVE n(k) AS (VALUES(1) UNION ALL SELECT k+1 FROM n WHERE k<64)
INSERT INTO synthetic_free_space SELECT zeroblob(131072) FROM n;
DELETE FROM synthetic_free_space`); err != nil {
		t.Fatal(err)
	}
	pages := func() int {
		t.Helper()
		var n int
		if err := store.DB().QueryRow(`PRAGMA freelist_count`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := pages()
	if before < 2000 {
		t.Fatal("fixture did not create a multi-step freelist", before)
	}
	if err = store.EnsureIncrementalVacuum(t.Context()); err != nil || pages() != before {
		t.Fatal("startup repeated conversion", err)
	}
	if err = reclaim(t.Context(), &pass{store: store}); err != nil || pages() != 0 {
		t.Fatal("not fully reclaimed", err)
	}
}
