package retention

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/benhook1013/FireController/internal/backupphase"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestRetentionFirstAndRepeatCadence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		interval   time.Duration
		firstDelay time.Duration
		busySkips  int
		dryRun     bool
		failPass   bool
	}{
		{"default", DefaultInterval, 5 * time.Minute, 0, true, false},
		{"short_interval", 2 * time.Minute, 2 * time.Minute, 0, false, false},
		{"five_minute_interval", 5 * time.Minute, 5 * time.Minute, 0, true, false},
		{"initial_busy", DefaultInterval, 5 * time.Minute, 2, true, false},
		{"failed_first_pass", DefaultInterval, 5 * time.Minute, 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "phase.lock")
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
				store, err := core.Open(t.Context(), filepath.Join(dir, "state"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				if err = reviews.EnsureSchema(t.Context(), store); err != nil {
					t.Fatal(err)
				}
				if err = jobs.EnsureSchema(t.Context(), store); err != nil {
					t.Fatal(err)
				}
				if err = store.EnsureIncrementalVacuum(t.Context()); err != nil {
					t.Fatal(err)
				}
				// Omitting the diagnostics table produces a genuine failed phase
				// that still completes and records a report.
				if !tc.failPass {
					if _, err = store.DB().Exec(`CREATE TABLE review_selection_decision_history(sequence INTEGER PRIMARY KEY,last_seen_at TEXT NOT NULL)`); err != nil {
						t.Fatal(err)
					}
				}
				var release func()
				if tc.busySkips > 0 {
					release, err = backupphase.Acquire(t.Context(), path)
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if release != nil {
							release()
						}
					}()
				}
				cfg := Config{Interval: tc.interval, DryRun: tc.dryRun, ReviewHistory: DefaultHistory, JobHistory: DefaultHistory, Receipts: DefaultHistory, Diagnostics: DefaultDiagnostics, PhaseLease: path}
				ctx, cancel := context.WithCancel(t.Context())
				reportEvents := make(chan Report, 4)
				failureEvents := make(chan error, 4)
				var reports []Report
				var failures []error
				start := time.Now()
				done := Run(ctx, store, cfg, func(r Report) { reportEvents <- r }, func(err error) { failureEvents <- err })
				defer func() {
					cancel()
					<-done
				}()
				synctest.Wait()
				advance := func(d time.Duration) {
					time.Sleep(d)
					synctest.Wait()
					for len(reportEvents) > 0 {
						reports = append(reports, <-reportEvents)
					}
					for len(failureEvents) > 0 {
						failures = append(failures, <-failureEvents)
					}
				}
				assertNoPass := func() {
					t.Helper()
					if len(reports) != 0 || len(failures) != 0 {
						t.Fatalf("unexpected reports or failures: %+v, %v", reports, failures)
					}
					var count int
					if err := store.DB().QueryRow(`SELECT count(*) FROM metadata WHERE key=?`, LastPassKey).Scan(&count); err != nil || count != 0 {
						t.Fatalf("unexpected durable report: count=%d, err=%v", count, err)
					}
				}
				advance(tc.firstDelay - time.Nanosecond)
				assertNoPass()
				for range tc.busySkips {
					advance(time.Nanosecond)
					assertNoPass()
					advance(tc.firstDelay - time.Nanosecond)
					assertNoPass()
				}
				if release != nil {
					release()
					release = nil
				}
				advance(time.Nanosecond)
				firstAt := start.Add(time.Duration(tc.busySkips+1) * tc.firstDelay)
				wantError := ""
				if tc.failPass {
					wantError = "diagnostics_failed"
				}
				if len(reports) != 1 || len(failures) != 0 {
					t.Fatalf("first pass reports or failures: %+v, %v", reports, failures)
				}
				if r := reports[0]; r.StartedAt != stamp(firstAt) || r.FinishedAt != stamp(firstAt) || r.DryRun != tc.dryRun || r.Error != wantError {
					t.Fatalf("unexpected first report: %+v", r)
				}
				var raw string
				if err := store.DB().QueryRow(`SELECT value FROM metadata WHERE key=?`, LastPassKey).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				want, err := json.Marshal(reports[0])
				if err != nil || raw != string(want) {
					t.Fatalf("first report not persisted: %s, %v", raw, err)
				}
				advance(tc.interval - time.Nanosecond)
				if len(reports) != 1 || len(failures) != 0 {
					t.Fatalf("pass repeated before configured interval: %+v, %v", reports, failures)
				}
				advance(time.Nanosecond)
				if len(reports) != 2 || len(failures) != 0 || reports[1].StartedAt != stamp(firstAt.Add(tc.interval)) {
					t.Fatalf("unexpected repeat cadence: %+v, %v", reports, failures)
				}
			})
		})
	}
}

func TestRetentionLeaseErrorAndShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		// A missing preprovisioned lease reports an error without opening a DB.
		cfg := Config{Interval: DefaultInterval, ReviewHistory: DefaultHistory, JobHistory: DefaultHistory, Receipts: DefaultHistory, Diagnostics: DefaultDiagnostics, PhaseLease: filepath.Join(t.TempDir(), "missing.lock")}
		failures := make(chan error, 4)
		done := Run(ctx, nil, cfg, func(r Report) { t.Errorf("lease error emitted report: %+v", r) }, func(err error) { failures <- err })
		requireLeaseError := func() {
			t.Helper()
			select {
			case err := <-failures:
				if !errors.Is(err, backupphase.ErrInvalid) {
					t.Fatalf("unexpected lease error: %v", err)
				}
			default:
				t.Fatal("scheduled lease error was lost")
			}
		}
		synctest.Wait()
		time.Sleep(5*time.Minute - time.Nanosecond)
		synctest.Wait()
		if len(failures) != 0 {
			t.Fatal("lease error before the first attempt")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		requireLeaseError()
		time.Sleep(DefaultInterval - time.Nanosecond)
		synctest.Wait()
		if len(failures) != 0 {
			t.Fatal("lease error repeated before the configured interval")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		requireLeaseError()
		cancel()
		<-done
		if len(failures) != 0 {
			t.Fatal("shutdown emitted an unscheduled lease error")
		}
	})
}

func TestRetentionDisabled(t *testing.T) {
	if done := Run(t.Context(), nil, Config{}, func(r Report) { t.Errorf("disabled worker emitted report: %+v", r) }, func(err error) { t.Errorf("disabled worker emitted failure: %v", err) }); done != nil {
		t.Fatal("disabled worker started")
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
