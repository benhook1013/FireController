package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/benhook1013/FireController/internal/core"
)

const managedSettlementTimeout = 5 * time.Second

type ScheduleConfig struct {
	Destination string        `json:"destination"`
	Interval    time.Duration `json:"interval"`
	Timeout     time.Duration `json:"timeout"`
	Keep        int           `json:"keep"`
}

// ValidateScheduleConfig is side-effect-free, including for a new installation.
func ValidateScheduleConfig(dataDir string, cfg ScheduleConfig) (ScheduleConfig, error) {
	return normalizeScheduleConfig(dataDir, cfg, true)
}
func normalizeScheduleConfig(dataDir string, cfg ScheduleConfig, inspect bool) (ScheduleConfig, error) {
	if cfg == (ScheduleConfig{}) {
		return cfg, nil
	}
	if cfg.Destination == "" || strings.TrimSpace(cfg.Destination) != cfg.Destination || len(cfg.Destination) > 2048 || strings.IndexFunc(cfg.Destination, unicode.IsControl) >= 0 || cfg.Interval < time.Minute || cfg.Interval > 30*24*time.Hour || cfg.Keep < 1 || cfg.Keep > 1000 || cfg.Timeout < 0 || cfg.Timeout > 24*time.Hour {
		return ScheduleConfig{}, core.Fail("configuration_invalid", "Supply a private backup destination, interval from one minute to thirty days, keep from one to one thousand, and a positive timeout no greater than twenty-four hours.", 400)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Minute
	}
	destination, err := filepath.Abs(cfg.Destination)
	if err != nil || len(destination) > 2048 {
		return ScheduleConfig{}, invalid()
	}
	cfg.Destination = destination
	if !scheduleStatusFits(cfg) {
		return ScheduleConfig{}, core.Fail("configuration_invalid", "The backup configuration cannot fit the bounded operator status with retained history.", 400)
	}
	if dataDir != "" {
		source, err := filepath.Abs(dataDir)
		if err != nil || within(source, destination) || within(destination, source) {
			return ScheduleConfig{}, invalid()
		}
	}
	if inspect {
		for p := destination; ; p = filepath.Dir(p) {
			info, err := os.Lstat(p)
			if err == nil {
				if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || p == destination && runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
					return ScheduleConfig{}, invalid()
				}
			} else if !os.IsNotExist(err) {
				return ScheduleConfig{}, invalid()
			}
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	return cfg, nil
}

type Manager struct {
	store   *core.Store
	dataDir string
	config  ScheduleConfig
	next    time.Time
	running atomic.Bool
	now     func() time.Time
	wait    func(context.Context, time.Duration) error
	create  func(context.Context, *core.Store, string, string) (Summary, error)
	remove  func(context.Context, string, Manifest) error
}

func scheduleWait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func managedStamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func safeManaged(err error) error {
	var e *core.Error
	if errors.As(err, &e) && managedReason(e.Code) {
		return core.Fail(e.Code, "The managed backup attempt failed; inspect operator backup status and the private destination.", 503)
	}
	return unavailable()
}
func NewManager(store *core.Store, dataDir string, cfg ScheduleConfig) (*Manager, error) {
	if store == nil {
		return nil, unavailable()
	}
	cfg, err := ValidateScheduleConfig(dataDir, cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = CheckDataDirectory(dataDir); err != nil {
		return nil, err
	}
	if err = StartupGuard(ctx, store); err != nil {
		return nil, err
	}
	if err = ValidateSource(ctx, dataDir); err != nil {
		return nil, err
	}
	var path string
	if err = store.DB().QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		return nil, unavailable()
	}
	chosen, e := privateFile(filepath.Join(dataDir, "firecontroller.sqlite"))
	actual, ae := privateFile(path)
	if e != nil || ae != nil || !os.SameFile(chosen, actual) {
		return nil, invalid()
	}
	if err = EnsureManagedSchema(ctx, store); err != nil {
		return nil, err
	}
	m := &Manager{store: store, dataDir: dataDir, config: cfg, now: time.Now, wait: scheduleWait, create: Create, remove: removeManagedBundle}
	now := time.Now().UTC()
	err = store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		old, err := loadManaged(ctx, tx)
		if err != nil {
			return err
		}
		r := managedRecord{Configuration: cfg}
		if old != nil {
			r.Status = old.Status
		}
		s := &r.Status
		if s.State == "running" {
			if s.LastAttempt != nil && s.LastAttempt.State == "running" {
				s.LastAttempt = &AttemptStatus{StartedAt: s.LastAttempt.StartedAt, FinishedAt: managedStamp(now), State: "interrupted", Reason: "backup_interrupted"}
			} else {
				s.Retention = &RetentionStatus{AttemptedAt: managedStamp(now), State: "failed", Reason: "backup_retention_interrupted"}
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE managed_backup_bundles SET state='cleanup_failed' WHERE state='deleting'`); err != nil {
			return err
		}
		s.Destination, s.Keep = cfg.Destination, cfg.Keep
		s.Interval, s.Timeout, s.NextAttemptAt = "", "", ""
		s.State = "disabled"
		if cfg.Destination != "" {
			s.State, s.Interval, s.Timeout = "scheduled", cfg.Interval.String(), cfg.Timeout.String()
			m.next = now.Add(cfg.Interval)
			if old != nil && old.Configuration == cfg && old.Status.NextAttemptAt != "" {
				m.next, _ = time.Parse(time.RFC3339Nano, old.Status.NextAttemptAt)
			} else if s.LastAttempt != nil && s.LastAttempt.FinishedAt != "" {
				finish, _ := time.Parse(time.RFC3339Nano, s.LastAttempt.FinishedAt)
				m.next = finish.Add(cfg.Interval)
			}
			s.NextAttemptAt = managedStamp(m.next)
		}
		return storeManaged(ctx, tx, r)
	})
	if err != nil {
		return nil, safeManaged(err)
	}
	return m, nil
}
func (m *Manager) update(ctx context.Context, fn func(*managedRecord, *sql.Tx) error) error {
	return m.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		r, err := loadManaged(ctx, tx)
		if err != nil {
			return err
		}
		if r == nil || r.Configuration != m.config {
			return invalid()
		}
		if err = fn(r, tx); err != nil {
			return err
		}
		return storeManaged(ctx, tx, *r)
	})
}

// Run owns one serial worker. Per-attempt failures are reported once, followed
// by the configured interval; cancellation stops only this owned local worker.
func (m *Manager) Run(ctx context.Context, report func(error)) error {
	if m == nil || ctx == nil {
		return invalid()
	}
	if !m.running.CompareAndSwap(false, true) {
		return core.Fail("backup_running", "The managed backup worker is already running.", 409)
	}
	defer m.running.Store(false)
	if m.config.Destination == "" {
		return nil
	}
	for {
		if err := m.wait(ctx, m.next.Sub(m.now())); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return unavailable()
		}
		if ctx.Err() != nil {
			return nil
		}
		err := m.attempt(ctx)
		m.next = m.now().Add(m.config.Interval)
		settle, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		saved := m.update(settle, func(r *managedRecord, _ *sql.Tx) error {
			r.Status.State = "scheduled"
			r.Status.NextAttemptAt = managedStamp(m.next)
			return nil
		})
		cancel()
		if err == nil {
			err = saved
		}
		if err != nil && report != nil {
			report(safeManaged(err))
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}
func (m *Manager) attempt(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, m.config.Timeout)
	defer cancel()
	start := managedStamp(m.now())
	err := m.update(ctx, func(r *managedRecord, _ *sql.Tx) error {
		r.Status.State = "running"
		r.Status.NextAttemptAt = ""
		r.Status.LastAttempt = &AttemptStatus{StartedAt: start, State: "running"}
		return nil
	})
	if err != nil {
		return err
	}
	result, err := m.create(ctx, m.store, m.dataDir, m.config.Destination)
	if err == nil {
		var manifest Manifest
		manifest, err = inspectManifest(ctx, result.Path)
		if err == nil && (manifest.ID != result.ID || manifest.SourceInstanceID != m.store.InstanceID() || filepath.Dir(result.Path) != m.config.Destination) {
			err = invalid()
		}
		if err == nil {
			var hash string
			hash, err = manifestHash(manifest)
			if err == nil {
				err = m.update(ctx, func(r *managedRecord, tx *sql.Tx) error {
					if _, e := tx.ExecContext(ctx, `INSERT INTO managed_backup_bundles(id,destination,source_instance_id,manifest_sha256,created_at,state) VALUES(?,?,?,?,?,'retained')`, manifest.ID, m.config.Destination, m.store.InstanceID(), hash, managedStamp(m.now())); e != nil {
						return e
					}
					r.Status.LastSuccess = &result
					r.Status.LastAttempt = &AttemptStatus{StartedAt: start, FinishedAt: managedStamp(m.now()), State: "complete"}
					return nil
				})
			}
		}
	}
	if err != nil {
		reason := "backup_unavailable"
		state := "failed"
		if ctx.Err() != nil {
			reason = "backup_interrupted"
			state = "interrupted"
		} else {
			var e *core.Error
			if errors.As(err, &e) && managedReason(e.Code) {
				reason = e.Code
			}
		}
		settle, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		saveErr := m.update(settle, func(r *managedRecord, _ *sql.Tx) error {
			r.Status.LastAttempt = &AttemptStatus{StartedAt: start, FinishedAt: managedStamp(m.now()), State: state, Reason: reason}
			return nil
		})
		if saveErr != nil {
			return saveErr
		}
		return safeManaged(err)
	}
	err = m.retain(ctx)
	stamp := managedStamp(m.now())
	settle, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	saveErr := m.update(settle, func(r *managedRecord, _ *sql.Tx) error {
		r.Status.Retention = &RetentionStatus{AttemptedAt: stamp, State: "complete"}
		if err != nil {
			r.Status.Retention.State = "failed"
			r.Status.Retention.Reason = "backup_retention_failed"
		}
		return nil
	})
	if saveErr != nil {
		return saveErr
	}
	if err != nil {
		return core.Fail("backup_retention_failed", "Managed retention failed; the newest verified backup remains retained.", 503)
	}
	return nil
}
