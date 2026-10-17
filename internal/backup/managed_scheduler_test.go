package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func newManagedFixture(t *testing.T, keep int) (*restoreFixture, *Manager) {
	t.Helper()
	f := newRestoreFixture(t)
	m, err := NewManager(f.store, f.dir, ScheduleConfig{Destination: filepath.Join(t.TempDir(), "managed"), Interval: time.Minute, Keep: keep})
	if err != nil {
		t.Fatal(err)
	}
	return f, m
}
func runManaged(t *testing.T, m *Manager, n int) []error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	now := m.next.Add(-m.config.Interval)
	m.now = func() time.Time { return now }
	steps := 0
	m.wait = func(ctx context.Context, d time.Duration) error {
		if d != m.config.Interval {
			t.Errorf("schedule did not wait interval after settled attempt: %s", d)
		}
		steps++
		if steps > n {
			cancel()
			return ctx.Err()
		}
		now = now.Add(d)
		return nil
	}
	var reports []error
	if err := m.Run(ctx, func(err error) { reports = append(reports, err) }); err != nil {
		t.Fatal(err)
	}
	return reports
}
func TestManagedConfigurationValidationDoesNotCreateInstallation(t *testing.T) {
	data := filepath.Join(t.TempDir(), "absent-source")
	destination := filepath.Join(t.TempDir(), "absent-target")
	valid := ScheduleConfig{Destination: destination, Interval: time.Minute, Keep: 1}
	got, err := ValidateScheduleConfig(data, valid)
	if err != nil || got.Timeout != 5*time.Minute {
		t.Fatal("default", got, err)
	}
	for _, bad := range []ScheduleConfig{{Destination: destination}, {Interval: time.Minute}, {Keep: 1}, {Timeout: time.Second}, {Destination: destination, Interval: time.Minute - time.Nanosecond, Keep: 1}, {Destination: destination, Interval: 30*24*time.Hour + time.Nanosecond, Keep: 1}, {Destination: destination, Interval: time.Minute, Keep: 1001}, {Destination: destination, Interval: time.Minute, Keep: 1, Timeout: -1}, {Destination: destination, Interval: time.Minute, Keep: 1, Timeout: 24*time.Hour + 1}, {Destination: filepath.Join(data, "nested"), Interval: time.Minute, Keep: 1}} {
		if _, err := ValidateScheduleConfig(data, bad); err == nil {
			t.Fatal("invalid configuration accepted", bad)
		}
	}
	for _, path := range []string{data, destination} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("validation created state")
		}
	}
	disabled, err := ValidateScheduleConfig(data, ScheduleConfig{})
	if err != nil || disabled != (ScheduleConfig{}) {
		t.Fatal(err)
	}
}
func TestManagedSnapshotsSerialRetentionAndReadOnlyStatus(t *testing.T) {
	f, m := newManagedFixture(t, 2)
	bundleNativeReferences(t, f)
	manual, err := Create(t.Context(), f.store, f.dir, m.config.Destination)
	if err != nil {
		t.Fatal(err)
	}
	manualBefore := restoreTree(t, manual.Path)
	create := m.create
	calls := 0
	m.create = func(ctx context.Context, s *core.Store, source, destination string) (createdBundle, error) {
		calls++
		out, err := create(ctx, s, source, destination)
		return out, err
	}
	reports := runManaged(t, m, 3)
	if len(reports) != 0 || calls != 3 {
		t.Fatal("attempts", calls, reports)
	}
	var retained, deleted int
	if f.store.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE state='retained'`).Scan(&retained) != nil || f.store.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE state='deleted'`).Scan(&deleted) != nil || retained != 2 || deleted != 1 {
		t.Fatal("owned retention counts", retained, deleted)
	}
	entries, err := os.ReadDir(m.config.Destination)
	if err != nil || len(entries) != 3 {
		t.Fatal("manual plus two managed copies", len(entries), err)
	}
	if !reflect.DeepEqual(manualBefore, restoreTree(t, manual.Path)) {
		t.Fatal("retention touched a manual bundle")
	}
	before := backupRows(t, f.store.DB())
	tree := restoreTree(t, m.config.Destination)
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || status.State != "scheduled" || status.LastAttempt == nil || status.LastAttempt.State != "complete" || status.LastSuccess == nil || status.Retention == nil || status.Retention.State != "complete" {
		t.Fatal("status", status, err)
	}
	if status.Timeout != "5m0s" || status.Interval != "1m0s" {
		t.Fatal("duration precision", status)
	}
	body, _ := json.Marshal(status)
	if len(body)+1 > 4096 || strings.Contains(string(body), f.token) {
		t.Fatal("status output budget/privacy")
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(tree, restoreTree(t, m.config.Destination)) {
		t.Fatal("status mutated source/destination")
	}
	if _, err := Inspect(t.Context(), status.LastSuccess.Path); err != nil {
		t.Fatal(err)
	}
	disabled, err := NewManager(f.store, f.dir, ScheduleConfig{})
	if err != nil {
		t.Fatal(err)
	}
	disabled.create = func(context.Context, *core.Store, string, string) (createdBundle, error) {
		t.Fatal("disabled snapshot")
		return createdBundle{}, nil
	}
	disabled.wait = func(context.Context, time.Duration) error { t.Fatal("disabled timer"); return nil }
	if err = disabled.Run(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	after, err := ReadStatus(t.Context(), f.dir)
	if err != nil || after.State != "disabled" || after.LastSuccess.ID != status.LastSuccess.ID || after.NextAttemptAt != "" {
		t.Fatal("disabled lost history", after, err)
	}
}
func TestManagedFailedAttemptPreservesSuccessAndDoesNotPrune(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	if reports := runManaged(t, m, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	first, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	before := restoreTree(t, m.config.Destination)
	m.create = func(context.Context, *core.Store, string, string) (createdBundle, error) {
		return createdBundle{}, errors.New("private path or provider body must not escape")
	}
	reports := runManaged(t, m, 2)
	if len(reports) != 2 {
		t.Fatal("failure reports must be once per attempt", len(reports))
	}
	for _, err := range reports {
		backupCode(t, err, "backup_unavailable")
		if strings.Contains(err.Error(), "private path") {
			t.Fatal("raw diagnostics escaped")
		}
	}
	after, err := ReadStatus(t.Context(), f.dir)
	if err != nil || after.LastAttempt.State != "failed" || after.LastSuccess.ID != first.LastSuccess.ID || after.Retention.State != "complete" {
		t.Fatal("failure overwrote success/retention", after, err)
	}
	if !reflect.DeepEqual(before, restoreTree(t, m.config.Destination)) {
		t.Fatal("failure pruned previous backup")
	}
}
func TestManagedRestartReconcilesRunningAndOverdueWithoutCatchup(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	now := time.Now().UTC()
	err := m.update(t.Context(), func(r *managedRecord, _ *sql.Tx) error {
		r.Status.State = "running"
		r.Status.NextAttemptAt = ""
		r.Status.LastAttempt = &AttemptStatus{StartedAt: managedStamp(now.Add(-time.Minute)), State: "running"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err = NewManager(f.store, f.dir, m.config)
	if err != nil {
		t.Fatal(err)
	}
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || status.LastAttempt.State != "interrupted" || status.LastAttempt.Reason != "backup_interrupted" || status.LastSuccess != nil {
		t.Fatal("restart invented success", status, err)
	}
	past := time.Now().UTC().Add(-10 * time.Minute)
	if err = m.update(t.Context(), func(r *managedRecord, _ *sql.Tx) error { r.Status.NextAttemptAt = managedStamp(past); return nil }); err != nil {
		t.Fatal(err)
	}
	m, err = NewManager(f.store, f.dir, m.config)
	if err != nil {
		t.Fatal(err)
	}
	if !m.next.Equal(past) {
		t.Fatal("overdue schedule was reset")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clock := time.Now().UTC()
	m.now = func() time.Time { return clock }
	waits := 0
	m.wait = func(ctx context.Context, d time.Duration) error {
		waits++
		if waits == 1 {
			if d >= 0 {
				t.Fatal("overdue did not run immediately")
			}
			return nil
		}
		if d != time.Minute {
			t.Fatal("catch-up queued or interval based on start", d)
		}
		cancel()
		return ctx.Err()
	}
	create := m.create
	m.create = func(ctx context.Context, s *core.Store, source, destination string) (createdBundle, error) {
		out, err := create(ctx, s, source, destination)
		clock = clock.Add(5 * time.Minute)
		return out, err
	}
	if err = m.Run(ctx, func(err error) { t.Error(err) }); err != nil || waits != 2 {
		t.Fatal(err, waits)
	}
}
func TestManagedUnconfiguredAndIncompleteRestoreStatus(t *testing.T) {
	f := newRestoreFixture(t)
	before := backupRows(t, f.store.DB())
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || status.State != "unconfigured" {
		t.Fatal(status, err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("status initialized optional schema")
	}
	if err = os.WriteFile(filepath.Join(f.dir, restoreMarker), nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = ReadStatus(t.Context(), f.dir)
	backupCode(t, err, "restore_inactive")
	if _, err = NewManager(f.store, f.dir, ScheduleConfig{}); err == nil {
		t.Fatal("incomplete restore allowed manager")
	}
}

func TestManagedEscapedDestinationKeepsRegisteredSuccessAndReportableStatus(t *testing.T) {
	f := newRestoreFixture(t)
	destination := filepath.Join(t.TempDir(), "long")
	for len(destination) < 430 {
		remaining := 430 - len(destination)
		if remaining == 1 {
			destination += "&"
			break
		}
		n := remaining - 1
		if n > 200 {
			n = 200
		}
		destination = filepath.Join(destination, strings.Repeat("&", n))
	}
	if len(destination) != 430 {
		t.Fatal("long destination fixture length", len(destination))
	}
	m, err := NewManager(f.store, f.dir, ScheduleConfig{Destination: destination, Interval: time.Minute, Keep: 1})
	if err != nil {
		t.Fatal("valid long configuration", err)
	}
	create := m.create
	m.create = func(ctx context.Context, s *core.Store, source, destination string) (createdBundle, error) {
		result, err := create(ctx, s, source, destination)
		if err != nil {
			t.Errorf("actual long-path creation failed: %#v", err)
		}
		return result, err
	}
	if reports := runManaged(t, m, 1); len(reports) != 0 {
		status, readErr := ReadStatus(t.Context(), f.dir)
		t.Fatalf("successful long-path bundle must be registered: errors=%#v status=%+v read=%v", reports, status.LastAttempt, readErr)
	}
	status, err := ReadStatus(t.Context(), f.dir)
	if err != nil || status.LastSuccess == nil || status.LastAttempt.State != "complete" {
		t.Fatal("success was lost", status, err)
	}
	body, err := json.Marshal(status)
	if err != nil || len(body)+1 <= 4096 || len(body)+1 > 16<<10 {
		t.Fatal("status should use only its selected larger budget", len(body), err)
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM managed_backup_bundles WHERE id=? AND state='retained'`, status.LastSuccess.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("bundle is usable but unowned", count, err)
	}
	verified, err := Inspect(t.Context(), status.LastSuccess.Path)
	if err != nil {
		t.Fatal(err)
	}
	manual, err := json.Marshal(verified)
	if err != nil || len(manual)+1 > 4096 {
		t.Fatal("manual output budget changed", len(manual), err)
	}
	t.Logf("destination bytes=%d; status bytes=%d; manual summary bytes=%d", len(destination), len(body)+1, len(manual)+1)
}

func TestManagedRejectsOversizedNormalizedAbsoluteDestination(t *testing.T) {
	relative := strings.Repeat("segment/", 250) + "end"
	if len(relative) > 2048 {
		t.Fatal("fixture raw path too long")
	}
	absolute, err := filepath.Abs(relative)
	if err != nil {
		t.Fatal(err)
	}
	if len(absolute) <= 2048 {
		relative += strings.Repeat("x", 2049-len(absolute))
	}
	if len(relative) > 2048 {
		t.Fatal("fixture must isolate normalized bound")
	}
	if _, err := ValidateScheduleConfig(filepath.Join(t.TempDir(), "source"), ScheduleConfig{Destination: relative, Interval: time.Minute, Keep: 1}); err == nil {
		t.Fatal("relative path normalized beyond allowed absolute bound")
	}
}

func TestManagedEncodedConfigurationBudgetPreservesPreviousSuccess(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	if reports := runManaged(t, m, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	previous, err := ReadStatus(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	destination := string(filepath.Separator)
	for len(destination) < 2048 {
		remaining := 2048 - len(destination)
		if remaining == 1 {
			destination += "&"
			break
		}
		n := remaining - 1
		if n > 200 {
			n = 200
		}
		destination = filepath.Join(destination, strings.Repeat("&", n))
	}
	cfg := ScheduleConfig{Destination: destination, Interval: time.Minute, Timeout: 5 * time.Minute, Keep: 1}
	// Exercise the actual serialized worst history permitted by the existing
	// manual Summary contract, independent of this host's SQLite path limit.
	largest := *previous.LastSuccess
	body, _ := json.Marshal(largest)
	target := len(largest.Path) + 4095 - len(body)
	parent := filepath.Join(t.TempDir(), "historical")
	parentTarget := target - len(largest.ID) - 1
	for len(parent) < parentTarget {
		remaining := parentTarget - len(parent)
		if remaining == 1 {
			parent += "p"
			break
		}
		n := remaining - 1
		if n > 200 {
			n = 200
		}
		parent = filepath.Join(parent, strings.Repeat("p", n))
	}
	largest.Path = filepath.Join(parent, largest.ID)
	body, _ = json.Marshal(largest)
	if len(body)+1 != 4096 {
		t.Fatal("manual-summary bound fixture", len(body))
	}
	worst := previous
	worst.Destination, worst.Interval, worst.Timeout, worst.Keep = cfg.Destination, cfg.Interval.String(), cfg.Timeout.String(), cfg.Keep
	worst.LastSuccess = &largest
	const latest = "9999-12-31T23:59:59.999999999Z"
	worst.NextAttemptAt = latest
	worst.LastAttempt = &AttemptStatus{StartedAt: latest, FinishedAt: latest, State: "interrupted", Reason: "backup_retention_interrupted"}
	worst.Retention = &RetentionStatus{AttemptedAt: latest, State: "failed", Reason: "backup_retention_interrupted"}
	public, _ := json.Marshal(worst)
	if len(public)+1 <= 16<<10 {
		t.Fatal("fixture does not expose accepted-config overflow", len(public))
	}
	t.Logf("encoded worst status bytes=%d; maximum prior manual summary bytes=%d", len(public)+1, len(body)+1)
	before := backupRows(t, f.store.DB())
	_, err = ValidateScheduleConfig(f.dir, cfg)
	backupCode(t, err, "configuration_invalid")
	_, err = NewManager(f.store, f.dir, cfg)
	backupCode(t, err, "configuration_invalid")
	current, err := ReadStatus(t.Context(), f.dir)
	if err != nil || !reflect.DeepEqual(current, previous) || !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("configuration preflight lost existing success/history", err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("configuration validation created destination")
	}
}
