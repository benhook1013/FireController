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

func managedActivatedCopy(t *testing.T, f *restoreFixture) (*core.Store, string) {
	t.Helper()
	bundle, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "restore-source"))
	if err != nil {
		t.Fatal("source backup", err)
	}
	dir := filepath.Join(t.TempDir(), "active-copy")
	restored, err := Restore(t.Context(), bundle.Path, dir)
	if err != nil {
		t.Fatal("restore", err)
	}
	file := filepath.Join(t.TempDir(), "new-admin.token")
	if err = os.WriteFile(file, []byte(core.NewID("synthetic_admin")), 0600); err != nil {
		t.Fatal("token fixture", err)
	}
	if _, err = Activate(t.Context(), dir, ActivationRequest{ExpectedInstanceID: restored.InstanceID, FencedPredecessorID: restored.SourceInstanceID, AdminTokenFile: file}); err != nil {
		t.Fatal("activate", err)
	}
	s, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal("open", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

// Complete one real backup without waiting through the operator interval.
func managedRecoveryAttempt(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.attempt(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.update(t.Context(), func(r *managedRecord, _ *sql.Tx) error {
		r.Status.State = "scheduled"
		r.Status.NextAttemptAt = managedStamp(time.Now().UTC().Add(m.config.Interval))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func managedFreshDue(t *testing.T, s *core.Store, dir string, cfg ScheduleConfig) *Manager {
	t.Helper()
	before := time.Now().UTC()
	m, err := NewManager(s, dir, cfg)
	after := time.Now().UTC()
	if err != nil {
		t.Fatal(err)
	}
	if m.next.Before(before.Add(cfg.Interval)) || m.next.After(after.Add(cfg.Interval)) {
		t.Fatal("predecessor timing scheduled a current-generation attempt", m.next, before, after)
	}
	return m
}

func TestManagedRecoveryFreshGenerationPreservesLegacyStatusAndSuccess(t *testing.T) {
	for _, stamp := range []string{"predecessor", "legacy-omitted"} {
		t.Run(stamp, func(t *testing.T) {
			f, m := newManagedFixture(t, 1)
			managedRecoveryAttempt(t, m)
			previous, err := ReadStatus(t.Context(), f.dir)
			if err != nil {
				t.Fatal(err)
			}
			past := time.Now().UTC().Add(-time.Hour)
			if err = m.update(t.Context(), func(r *managedRecord, _ *sql.Tx) error {
				r.Status.NextAttemptAt = managedStamp(past)
				r.Status.LastAttempt.StartedAt = managedStamp(past.Add(-time.Minute))
				r.Status.LastAttempt.FinishedAt = managedStamp(past)
				if stamp == "legacy-omitted" {
					r.SourceInstanceID = ""
					r.Status.LastAttempt.SourceInstanceID = ""
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// Old omitted records still round-trip canonically and status is read-only.
			before := backupRows(t, f.store.DB())
			if _, err = ReadStatus(t.Context(), f.dir); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
				t.Fatal("status rewrote legacy ownership")
			}
			restored, dir := managedActivatedCopy(t, f)
			m = managedFreshDue(t, restored, dir, m.config)
			status, err := ReadStatus(t.Context(), dir)
			if err != nil || !reflect.DeepEqual(status.LastSuccess, previous.LastSuccess) || status.LastSuccess.SourceInstanceID == restored.InstanceID() {
				t.Fatal("predecessor success relabelled", status, err)
			}
			retained, err := loadManaged(t.Context(), restored.DB())
			if err != nil || retained.SourceInstanceID != restored.InstanceID() {
				t.Fatal("current schedule lacks generation binding", err)
			}
			// Restart before the first genuine new success retains this generation's due
			// time, despite LastSuccess still describing the predecessor.
			restarted, err := NewManager(restored, dir, m.config)
			if err != nil || !restarted.next.Equal(m.next) {
				t.Fatal("current generation due time reset", err)
			}
			due := time.Now().UTC().Add(-time.Second)
			if err = m.update(t.Context(), func(r *managedRecord, _ *sql.Tx) error { r.Status.NextAttemptAt = managedStamp(due); return nil }); err != nil {
				t.Fatal(err)
			}
			restarted, err = NewManager(restored, dir, m.config)
			if err != nil || !restarted.next.Equal(due) {
				t.Fatal("current generation overdue observation lost", err)
			}
		})
	}
}

func TestManagedRecoveryDisabledThenEnabledDoesNotResumePredecessorAttempt(t *testing.T) {
	for _, offset := range []time.Duration{-time.Hour, time.Hour} {
		t.Run(offset.String(), func(t *testing.T) {
			f, m := newManagedFixture(t, 1)
			managedRecoveryAttempt(t, m)
			previous, err := ReadStatus(t.Context(), f.dir)
			if err != nil {
				t.Fatal(err)
			}
			started := managedStamp(time.Now().UTC().Add(offset))
			if err = m.update(t.Context(), func(r *managedRecord, _ *sql.Tx) error {
				r.Status.State = "running"
				r.Status.NextAttemptAt = ""
				r.Status.LastAttempt = &AttemptStatus{StartedAt: started, State: "running"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			restored, dir := managedActivatedCopy(t, f)
			disabled, err := NewManager(restored, dir, ScheduleConfig{})
			if err != nil {
				t.Fatal(err)
			}
			disabled.wait = func(context.Context, time.Duration) error {
				t.Fatal("disabled timer resumed predecessor configuration")
				return nil
			}
			disabled.create = func(context.Context, *core.Store, string, string) (createdBundle, error) {
				t.Fatal("disabled backup")
				return createdBundle{}, nil
			}
			if err = disabled.Run(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			status, err := ReadStatus(t.Context(), dir)
			if err != nil || status.State != "disabled" || status.NextAttemptAt != "" || status.LastAttempt.State != "interrupted" || status.LastAttempt.StartedAt != started || !reflect.DeepEqual(status.LastSuccess, previous.LastSuccess) {
				t.Fatal("disabled recovery changed history or resumed schedule", status, err)
			}
			startTime, _ := time.Parse(time.RFC3339Nano, started)
			if finish, _ := time.Parse(time.RFC3339Nano, status.LastAttempt.FinishedAt); finish.Before(startTime) {
				t.Fatal("interruption recording precedes retained start")
			}
			if status.LastAttempt.SourceInstanceID != f.store.InstanceID() {
				t.Fatal("interrupted predecessor origin changed")
			}
			managedFreshDue(t, restored, dir, m.config)
		})
	}
}

func TestManagedRecoveryRetentionNeverDeletesPredecessorBundles(t *testing.T) {
	f, m := newManagedFixture(t, 2)
	for i := 0; i < 2; i++ {
		managedRecoveryAttempt(t, m)
	}
	rows, err := f.store.DB().Query(`SELECT id FROM managed_backup_bundles ORDER BY sequence`)
	if err != nil {
		t.Fatal(err)
	}
	originals := map[string]map[string]string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		originals[id] = restoreTree(t, filepath.Join(m.config.Destination, id))
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	restored, dir := managedActivatedCopy(t, f)
	cfg := m.config
	cfg.Keep = 1
	current := managedFreshDue(t, restored, dir, cfg)
	for i := 0; i < 2; i++ {
		managedRecoveryAttempt(t, current)
	}
	status, err := ReadStatus(t.Context(), dir)
	if err != nil || status.LastSuccess.SourceInstanceID != restored.InstanceID() {
		t.Fatal("new genuine success not current", status, err)
	}
	for id, body := range originals {
		if !reflect.DeepEqual(body, restoreTree(t, filepath.Join(cfg.Destination, id))) {
			t.Fatal("predecessor bundle changed")
		}
		var state, instance string
		if err = restored.DB().QueryRow(`SELECT state,source_instance_id FROM managed_backup_bundles WHERE id=?`, id).Scan(&state, &instance); err != nil || state != "retained" || instance != f.store.InstanceID() {
			t.Fatal("predecessor ledger changed", err)
		}
	}
	var retained, deleted int
	if err = restored.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE source_instance_id=? AND state='retained'`, restored.InstanceID()).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if err = restored.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE source_instance_id=? AND state='deleted'`, restored.InstanceID()).Scan(&deleted); err != nil || retained != 1 || deleted != 1 {
		t.Fatal("current retention not isolated", retained, deleted, err)
	}
	if _, err = Inspect(t.Context(), status.LastSuccess.Path); err != nil {
		t.Fatal("new generation bundle not verifiable", err)
	}
}

func TestManagedRecoveryFuturePredecessorAttemptDoesNotAuthorizeTiming(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit-origin", true: "legacy-unknown"}[legacy], func(t *testing.T) {
			f, m := newManagedFixture(t, 1)
			managedRecoveryAttempt(t, m)
			future := time.Now().UTC().Add(time.Hour)
			if err := m.update(t.Context(), func(r *managedRecord, _ *sql.Tx) error {
				r.Status.LastAttempt.StartedAt = managedStamp(future)
				r.Status.LastAttempt.FinishedAt = managedStamp(future.Add(time.Minute))
				if legacy {
					r.SourceInstanceID = ""
					r.Status.LastAttempt.SourceInstanceID = ""
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			restored, dir := managedActivatedCopy(t, f)
			if _, err := NewManager(restored, dir, ScheduleConfig{}); err != nil {
				t.Fatal(err)
			}
			current := managedFreshDue(t, restored, dir, m.config)
			// Once an actual current-instance attempt has completed, its completion time
			// is a valid interval origin even when enabling after a disabled restart.
			managedRecoveryAttempt(t, current)
			status, err := ReadStatus(t.Context(), dir)
			if err != nil || status.LastAttempt.SourceInstanceID != restored.InstanceID() {
				t.Fatal("new attempt lacks its actual origin", status, err)
			}
			finish, err := time.Parse(time.RFC3339Nano, status.LastAttempt.FinishedAt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = NewManager(restored, dir, ScheduleConfig{}); err != nil {
				t.Fatal(err)
			}
			enabled, err := NewManager(restored, dir, m.config)
			if err != nil || !enabled.next.Equal(finish.Add(m.config.Interval)) {
				t.Fatal("own attempt completion not used", err)
			}
		})
	}
}

func TestManagedClockRollbackFailureSettlesAndLeavesCoordinationUsable(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	clock := time.Now().UTC()
	m.now = func() time.Time { return clock }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waits := 0
	m.wait = func(ctx context.Context, _ time.Duration) error {
		waits++
		if waits > 1 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	m.create = func(context.Context, *core.Store, string, string) (createdBundle, error) {
		clock = clock.Add(-time.Hour)
		return createdBundle{}, unavailable()
	}
	reports := 0
	if err := m.Run(ctx, func(error) { reports++ }); err != nil {
		t.Fatal(err)
	}
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || reports != 1 || status.State != "scheduled" || status.LastAttempt.State != "failed" || status.LastAttempt.FinishedAt != status.LastAttempt.StartedAt || status.LastAttempt.SourceInstanceID != f.store.InstanceID() {
		t.Fatal("clock rollback lost safe settled status", status, err)
	}
	f.request(t, "POST", "/api/v1/projects/"+f.project+"/messages", map[string]any{"recipient_worker_id": f.worker, "body": "Coordination after local backup clock rollback"})
}
