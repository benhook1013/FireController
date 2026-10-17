package backup

import (
	"context"
	"database/sql"
	"encoding/json"
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
	store           *core.Store
	dataDir         string
	config          ScheduleConfig
	next            time.Time
	running         atomic.Bool
	now             func() time.Time
	wait            func(context.Context, time.Duration) error
	create          func(context.Context, *core.Store, string, string) (createdBundle, error)
	remove          func(context.Context, string, Manifest) error
	publicationHeld bool
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

// Attempt timestamps are effective recording facts. A local clock rollback
// must not turn a retained start into an invalid negative-duration status.
func managedFinish(started string, now time.Time) string {
	start, _ := time.Parse(time.RFC3339Nano, started)
	if now.Before(start) {
		return started
	}
	return managedStamp(now)
}
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
	if cfg.Destination != "" {
		if err = ValidateSource(ctx, dataDir); err != nil {
			return nil, err
		}
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
	m := &Manager{store: store, dataDir: dataDir, config: cfg, now: time.Now, wait: scheduleWait, create: createManaged, remove: removeManagedBundle}
	now := time.Now().UTC()
	err = store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		old, err := loadManaged(ctx, tx)
		if err != nil {
			return err
		}
		recovery, err := readRecoveryStatus(ctx, tx)
		if err != nil {
			return err
		}
		// The singleton may be inherited from a predecessor. Its history stays
		// visible, but its due time cannot schedule this new instance's work.
		currentGeneration := old != nil && (old.SourceInstanceID == store.InstanceID() || old.SourceInstanceID == "" && recovery.State == "ordinary")
		r := managedRecord{Configuration: cfg, SourceInstanceID: store.InstanceID()}
		if old != nil {
			r.Status = old.Status
		}
		s := &r.Status
		if s.State == "running" {
			if s.LastAttempt != nil && s.LastAttempt.State == "running" {
				origin := s.LastAttempt.SourceInstanceID
				if origin == "" {
					origin = old.SourceInstanceID
				}
				s.LastAttempt = &AttemptStatus{StartedAt: s.LastAttempt.StartedAt, FinishedAt: managedFinish(s.LastAttempt.StartedAt, now), State: "interrupted", Reason: "backup_interrupted", SourceInstanceID: origin}
			} else {
				s.Retention = &RetentionStatus{AttemptedAt: managedStamp(now), State: "failed", Reason: "backup_retention_interrupted"}
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE managed_backup_bundles SET state='cleanup_failed' WHERE state='deleting' AND source_instance_id=?`, store.InstanceID()); err != nil {
			return err
		}
		s.Destination, s.Keep = cfg.Destination, cfg.Keep
		s.Interval, s.Timeout, s.NextAttemptAt = "", "", ""
		s.State = "disabled"
		if cfg.Destination != "" {
			s.State, s.Interval, s.Timeout = "scheduled", cfg.Interval.String(), cfg.Timeout.String()
			m.next = now.Add(cfg.Interval)
			if currentGeneration && old.Configuration == cfg && old.Status.NextAttemptAt != "" {
				m.next, _ = time.Parse(time.RFC3339Nano, old.Status.NextAttemptAt)
			} else if currentGeneration && s.LastAttempt != nil && s.LastAttempt.FinishedAt != "" {
				if s.LastAttempt.SourceInstanceID == store.InstanceID() || s.LastAttempt.SourceInstanceID == "" && recovery.State == "ordinary" {
					finish, _ := time.Parse(time.RFC3339Nano, s.LastAttempt.FinishedAt)
					m.next = finish.Add(cfg.Interval)
				}
			}
			s.NextAttemptAt = managedStamp(m.next)
			var held string
			var id, hash string
			heldErr := tx.QueryRowContext(ctx, `SELECT bundle_id,manifest_sha256,CASE WHEN length(CAST(summary_json AS BLOB))<4096 THEN summary_json ELSE NULL END FROM managed_backup_publication_holds WHERE destination=? AND source_instance_id=? ORDER BY bundle_id LIMIT 1`, cfg.Destination, store.InstanceID()).Scan(&id, &hash, &held)
			if heldErr == nil {
				var pending Summary
				if json.Unmarshal([]byte(held), &pending) != nil || pending.ID != id || pending.SourceInstanceID != store.InstanceID() || filepath.Dir(pending.Path) != cfg.Destination || !digest.MatchString(hash) {
					return invalid()
				}
				canonical, marshalErr := json.Marshal(pending)
				if marshalErr != nil || string(canonical) != held {
					return invalid()
				}
				s.UnregisteredBundle = &pending
				m.publicationHeld = true
				s.State, s.NextAttemptAt = "held", ""
			} else if !errors.Is(heldErr, sql.ErrNoRows) {
				return unavailable()
			}
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
	if m.publicationHeld {
		<-ctx.Done()
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
			if m.publicationHeld {
				r.Status.State, r.Status.NextAttemptAt = "held", ""
			}
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
		if m.publicationHeld {
			<-ctx.Done()
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
		r.Status.LastAttempt = &AttemptStatus{StartedAt: start, State: "running", SourceInstanceID: m.store.InstanceID()}
		return nil
	})
	if err != nil {
		return err
	}
	result, err := m.create(ctx, m.store, m.dataDir, m.config.Destination)
	if err == nil {
		// Creation already verified these exact bytes. Settlement must not use
		// an expired snapshot context or rescan a potentially large bundle.
		settle, done := context.WithTimeout(context.Background(), managedSettlementTimeout)
		err = m.registerCreated(settle, result, start)
		done()
		if err != nil {
			m.publicationHeld = true
		}
	}
	if err != nil {
		reason := "backup_unavailable"
		state := "failed"
		if m.publicationHeld {
			reason = "backup_registration_failed"
		} else if ctx.Err() != nil {
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
		saveErr := m.update(settle, func(r *managedRecord, tx *sql.Tx) error {
			r.Status.LastAttempt = &AttemptStatus{StartedAt: start, FinishedAt: managedFinish(start, m.now()), State: state, Reason: reason, SourceInstanceID: m.store.InstanceID()}
			if m.publicationHeld {
				r.Status.UnregisteredBundle = &result.Summary
				body, marshalErr := json.Marshal(result.Summary)
				if marshalErr != nil {
					return invalid()
				}
				if _, holdErr := tx.ExecContext(settle, `INSERT INTO managed_backup_publication_holds(bundle_id,destination,source_instance_id,manifest_sha256,summary_json) VALUES(?,?,?,?,?)`, result.ID, m.config.Destination, m.store.InstanceID(), result.hash, string(body)); holdErr != nil {
					return holdErr
				}
			}
			return nil
		})
		if saveErr != nil {
			return saveErr
		}
		return safeManaged(err)
	}
	// Retention has its own bounded phase budget, still owned by this worker's
	// parent cancellation rather than detached from shutdown.
	retentionCtx, cancelRetention := context.WithTimeout(parent, m.config.Timeout)
	err = m.retain(retentionCtx)
	cancelRetention()
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

// A returned transaction error does not prove rollback. Recover only an exact
// committed row plus its atomic success record; otherwise preserve the bundle.
func (m *Manager) registerCreated(ctx context.Context, b createdBundle, started string) error {
	current, err := os.Lstat(b.Path)
	if err != nil || b.owned == nil || !os.SameFile(b.owned, current) || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || filepath.Dir(b.Path) != m.config.Destination || b.SourceInstanceID != m.store.InstanceID() || b.manifest.ID != b.ID || b.manifest.SourceInstanceID != b.SourceInstanceID || !digest.MatchString(b.hash) {
		return invalid()
	}
	err = m.update(ctx, func(r *managedRecord, tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, `INSERT INTO managed_backup_bundles(id,destination,source_instance_id,manifest_sha256,created_at,state) VALUES(?,?,?,?,?,'retained')`, b.ID, m.config.Destination, b.SourceInstanceID, b.hash, managedStamp(m.now())); e != nil {
			return e
		}
		r.Status.LastSuccess = &b.Summary
		r.Status.LastAttempt = &AttemptStatus{StartedAt: started, FinishedAt: managedFinish(started, m.now()), State: "complete", SourceInstanceID: m.store.InstanceID()}
		return nil
	})
	if err == nil {
		return nil
	}
	// Use a fresh bounded read after BackgroundWrite has returned. Neither a
	// missing row nor an unavailable read authorizes deletion or a new copy.
	read, cancel := context.WithTimeout(context.Background(), managedSettlementTimeout)
	defer cancel()
	tx, readErr := m.store.DB().BeginTx(read, &sql.TxOptions{ReadOnly: true})
	if readErr != nil {
		return err
	}
	defer tx.Rollback()
	var destination, instance, hash, state string
	readErr = tx.QueryRowContext(read, `SELECT destination,source_instance_id,manifest_sha256,state FROM managed_backup_bundles WHERE id=?`, b.ID).Scan(&destination, &instance, &hash, &state)
	if readErr != nil || destination != m.config.Destination || instance != b.SourceInstanceID || hash != b.hash || state != "retained" {
		return err
	}
	r, readErr := loadManaged(read, tx)
	if readErr != nil || r == nil || r.Status.LastSuccess == nil || *r.Status.LastSuccess != b.Summary {
		return err
	}
	return nil
}
