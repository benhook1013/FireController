// Package retention bounds database history growth without touching execution evidence.
package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/backupphase"
	"github.com/benhook1013/FireController/internal/core"
)

const (
	DefaultInterval    = 6 * time.Hour
	DefaultHistory     = 30 * 24 * time.Hour
	DefaultDiagnostics = 14 * 24 * time.Hour
	LastPassKey        = "retention:last_pass"
	BatchRows          = 500
	batchYield         = 10 * time.Millisecond
)

type Config struct {
	Interval      time.Duration
	DryRun        bool
	ReviewHistory time.Duration
	JobHistory    time.Duration
	Receipts      time.Duration
	Diagnostics   time.Duration
	PhaseLease    string
}

type Report struct {
	StartedAt  string           `json:"started_at"`
	FinishedAt string           `json:"finished_at"`
	DryRun     bool             `json:"dry_run"`
	Deleted    map[string]int64 `json:"deleted"`
	Error      string           `json:"error"`

	// Scheduled operator diagnostics only; the durable five-field report stays stable.
	HistoryFailurePhase string `json:"-"`
	HistoryFailureClass string `json:"-"`
}

func (c Config) Validate() error {
	if c.Interval < 0 || c.ReviewHistory <= 0 || c.JobHistory <= 0 || c.Receipts <= 0 || c.Diagnostics <= 0 {
		return core.Fail("configuration_invalid", "Retention interval must be non-negative and every retention horizon must be positive.", 400)
	}
	return nil
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

// Avoid storing impossible report intervals if wall time moves backwards.
func reportFinish(started, finished time.Time) string {
	if finished.UTC().Before(started.UTC()) {
		finished = started
	}
	return stamp(finished)
}

// Older schema writers used RFC3339Nano, which omits fractional trailing
// zeroes. Compare UTC instants exactly, including equal whole-second cutoffs.
func legacyTime(column string) string {
	return "substr(" + column + ",1,19)||'.'||substr(CASE WHEN substr(" + column + ",20,1)='.' THEN substr(" + column + ",21,length(" + column + ")-21) ELSE '' END||'000000000',1,9)||'Z'"
}

// Run first tries after at most five minutes, then waits the configured
// interval between passes. Initial busy skips retain the short delay.
// Passes never overlap or delay backup or deployment while acquiring the lease.
func Run(ctx context.Context, store *core.Store, cfg Config, report func(Report), failed func(error)) <-chan struct{} {
	if cfg.Interval == 0 {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		delay := min(cfg.Interval, 5*time.Minute)
		for wait(ctx, delay) == nil && ctx.Err() == nil {
			r, skip, err := RunPass(ctx, store, cfg, time.Now())
			if ctx.Err() != nil {
				return
			}
			if !skip || err != nil {
				delay = cfg.Interval
			}
			if !skip && report != nil {
				report(r)
			}
			if err != nil && failed != nil && r.Error == "" {
				failed(err)
			}
		}
	}()
	return done
}

// RunPass holds the same preprovisioned inode used by both backup and deploy.
// The report intentionally exposes only a stable failed phase, not SQL values.
func RunPass(ctx context.Context, store *core.Store, cfg Config, now time.Time) (Report, bool, error) {
	if err := cfg.Validate(); err != nil {
		return Report{}, true, err
	}
	release, err := backupphase.TryAcquire(ctx, cfg.PhaseLease)
	if errors.Is(err, backupphase.ErrBusy) {
		return Report{}, true, nil
	}
	if err != nil {
		return Report{}, true, err
	}
	defer release()
	p := &pass{store: store, dryRun: cfg.DryRun, deleted: make(map[string]int64), yield: batchYield}
	r := Report{StartedAt: stamp(now), DryRun: cfg.DryRun, Deleted: p.deleted}
	steps := []struct {
		name string
		run  func() error
	}{
		{"review_history", func() error { return pruneReviewHistory(ctx, p, now.Add(-cfg.ReviewHistory)) }},
		{"job_history", func() error { return pruneJobHistory(ctx, p, now.Add(-cfg.JobHistory)) }},
		{"receipts", func() error { return pruneReceipts(ctx, p, now.Add(-cfg.Receipts)) }},
		{"diagnostics", func() error { return pruneDiagnostics(ctx, p, now.Add(-cfg.Diagnostics)) }},
	}
	if !cfg.DryRun {
		steps = append(steps, struct {
			name string
			run  func() error
		}{"incremental_vacuum", func() error { return reclaim(ctx, p) }})
	}
	for _, step := range steps {
		if err = step.run(); err != nil {
			r.Error = step.name + "_failed"
			var failure *historyPhaseError
			if errors.As(err, &failure) {
				r.HistoryFailurePhase = failure.phase
				r.HistoryFailureClass = historyErrorClass(failure.cause)
			}
			break
		}
	}
	r.FinishedAt = reportFinish(now, time.Now())
	raw, marshalErr := json.Marshal(r)
	if marshalErr != nil {
		return r, false, marshalErr
	}
	// Do not start new work on shutdown. A subsequent pass resumes deletions
	// idempotently and records its own result.
	if ctx.Err() != nil {
		return r, false, ctx.Err()
	}
	writeErr := store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, LastPassKey, string(raw))
		return err
	})
	if writeErr != nil {
		r.Error = "pass_record_failed"
	}
	return r, false, errors.Join(err, writeErr)
}

type pass struct {
	store   *core.Store
	dryRun  bool
	deleted map[string]int64
	yield   time.Duration
}

func ident(s string) string { return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\"" }

func (p *pass) delete(ctx context.Context, table, where string, args ...any) error {
	if p.dryRun {
		var count int64
		err := p.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM "+ident(table)+" WHERE "+where, args...).Scan(&count)
		if err == nil {
			p.deleted[table] += count
		}
		return err
	}
	return p.batches(ctx, func(ctx context.Context, tx *sql.Tx) (int64, error) {
		res, err := tx.ExecContext(ctx, "DELETE FROM "+ident(table)+" WHERE rowid IN (SELECT rowid FROM "+ident(table)+" WHERE "+where+" ORDER BY rowid LIMIT "+fmt.Sprint(BatchRows)+")", args...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}, table)
}

func (p *pass) batches(ctx context.Context, run func(context.Context, *sql.Tx) (int64, error), table string) error {
	for {
		var n int64
		err := p.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			n, err = run(ctx, tx)
			return err
		})
		if err != nil {
			return err
		}
		if table != "" {
			p.deleted[table] += n
		}
		if n < BatchRows {
			return nil
		}
		if err = wait(ctx, p.yield); err != nil {
			return err
		}
	}
}

func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func pruneDiagnostics(ctx context.Context, p *pass, cutoff time.Time) error {
	return p.delete(ctx, "review_selection_decision_history", legacyTime("last_seen_at")+" < ?", stamp(cutoff))
}

func reclaim(ctx context.Context, p *pass) error {
	for {
		var before, after int64
		err := p.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			if err := tx.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&before); err != nil {
				return err
			}
			if before == 0 {
				return nil
			}
			rows, err := tx.QueryContext(ctx, `PRAGMA incremental_vacuum(2000)`)
			if err != nil {
				return err
			}
			for rows.Next() {
			}
			if err = errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			return tx.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&after)
		})
		if err != nil {
			return err
		}
		if after == 0 {
			return nil
		}
		if after >= before {
			return errors.New("incremental vacuum did not reduce the freelist")
		}
		if err = wait(ctx, p.yield); err != nil {
			return err
		}
	}
}
