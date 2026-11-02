package backuphealth

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOffsiteHealthRunningUsesConfiguredTimeoutAndIndependentReporterAge(t *testing.T) {
	base, now := healthFixture()
	base.ObservedAt = now.Format(time.RFC3339Nano)
	base.AttemptState = "running"
	base.AttemptTimeoutSeconds = 900
	for _, tc := range []struct {
		name          string
		age           time.Duration
		reporterAge   time.Duration
		state, reason string
	}{
		{"ten-minute-push", 10 * time.Minute, 0, "running", ""},
		{"timeout-plus-grace-boundary", 20 * time.Minute, 0, "running", ""},
		{"fresh-publisher-expired-attempt", 20*time.Minute + time.Nanosecond, 0, "unknown", "attempt_stale"},
		{"stale-publisher-live-deadline", 10 * time.Minute, ReporterMaxAge + time.Nanosecond, "unknown", "report_stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := base
			m.LastAttemptAt = now.Add(-tc.age).Format(time.RFC3339Nano)
			m.ObservedAt = now.Add(-tc.reporterAge).Format(time.RFC3339Nano)
			got := Offsite(m, m.InstanceID, now)
			if got.State != tc.state || got.Reason != tc.reason || got.LastSuccessAt != m.LastSnapshotAt {
				t.Fatal(got)
			}
		})
	}
	for _, state := range []string{"complete", "unchanged", "failed", "interrupted"} {
		m := base
		m.AttemptState = state
		before := Offsite(m, m.InstanceID, now)
		m.AttemptTimeoutSeconds = 0
		if after := Offsite(m, m.InstanceID, now); after != before {
			t.Fatal("timeout altered nonrunning health", state, before, after)
		}
	}
}

func TestOffsiteHealthOptionalAttemptTimeoutBoundsAndLegacyMirror(t *testing.T) {
	m, now := healthFixture()
	for _, seconds := range []int64{-1, 86401} {
		m.AttemptTimeoutSeconds = seconds
		if m.Valid() || Offsite(m, m.InstanceID, now).Reason != "report_unavailable" {
			t.Fatal("invalid timeout accepted", seconds)
		}
	}
	for _, seconds := range []int64{0, 1, 86400} {
		m.AttemptTimeoutSeconds = seconds
		if !m.Valid() {
			t.Fatal("valid timeout rejected", seconds)
		}
	}
	m.AttemptTimeoutSeconds = 0
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "legacy.json")
	if err := WriteMirror(path, m); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil || bytes.Contains(body, []byte("attempt_timeout_seconds")) {
		t.Fatal("legacy optional field not omitted", err)
	}
	got, err := ReadMirror(path)
	if err != nil || got != m || Offsite(got, m.InstanceID, now).State != "healthy" {
		t.Fatal("old absent-field mirror no longer readable", got, err)
	}
}

func healthFixture() (Mirror, time.Time) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	return Mirror{Version: 1, InstanceID: "instance_" + strings.Repeat("a", 32), Label: "Cloud backup", ObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), MaxAgeSeconds: 10800, LastSnapshotAt: now.Add(-time.Hour).Format(time.RFC3339Nano), AttemptState: "unchanged", LastAttemptAt: now.Add(-2 * time.Minute).Format(time.RFC3339Nano)}, now
}

func TestOffsiteHealthSeparatesReporterSnapshotAndAttemptFreshness(t *testing.T) {
	base, now := healthFixture()
	for _, tc := range []struct {
		name, state, reason string
		change              func(*Mirror)
	}{
		{"recent-snapshot", "healthy", "", func(*Mirror) {}},
		{"late-snapshot", "overdue", "", func(m *Mirror) { m.LastSnapshotAt = now.Add(-4 * time.Hour).Format(time.RFC3339Nano) }},
		{"stale-reporter", "unknown", "report_stale", func(m *Mirror) {
			m.ObservedAt = now.Add(-6 * time.Minute).Format(time.RFC3339Nano)
			m.LastAttemptAt = now.Add(-7 * time.Minute).Format(time.RFC3339Nano)
		}},
		{"future-reporter", "unknown", "report_unavailable", func(m *Mirror) { m.ObservedAt = now.Add(time.Second).Format(time.RFC3339Nano) }},
		{"future-snapshot", "unknown", "report_unavailable", func(m *Mirror) { m.LastSnapshotAt = now.Add(time.Second).Format(time.RFC3339Nano) }},
		{"wrong-instance", "unknown", "report_unavailable", func(m *Mirror) { m.InstanceID = "instance_" + strings.Repeat("b", 32) }},
		{"latest-failure", "failed", "last_attempt_failed", func(m *Mirror) { m.AttemptState = "failed"; m.AttemptReason = "offsite_transport_failed" }},
		{"interrupted", "failed", "last_attempt_failed", func(m *Mirror) { m.AttemptState = "interrupted" }},
		{"retention-failure", "failed", "retention_failed", func(m *Mirror) { m.RetentionWarning = true }},
		{"recent-running", "running", "", func(m *Mirror) { m.AttemptState = "running" }},
		{"old-running-ledger", "unknown", "attempt_stale", func(m *Mirror) {
			m.AttemptState = "running"
			m.LastAttemptAt = now.Add(-10 * time.Minute).Format(time.RFC3339Nano)
		}},
		{"no-first-completion", "unknown", "no_completed_backup", func(m *Mirror) { m.LastSnapshotAt = ""; m.AttemptState = ""; m.LastAttemptAt = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := base
			tc.change(&m)
			r := Offsite(m, base.InstanceID, now)
			if r.State != tc.state || r.Reason != tc.reason {
				t.Fatal(r)
			}
			if (tc.state == "failed" || tc.reason == "report_stale") && r.LastSuccessAt != m.LastSnapshotAt {
				t.Fatal("prior completed snapshot lost", r)
			}
		})
	}
}

func TestManagedHealthUsesOnlyCurrentInstanceAndConfiguredSchedule(t *testing.T) {
	m, now := healthFixture()
	base := LocalRecord{Configured: true, State: "scheduled", LastSnapshotAt: m.LastSnapshotAt, SuccessInstance: m.InstanceID, AttemptState: "complete", LastAttemptAt: m.LastAttemptAt, AttemptInstance: m.InstanceID, Interval: time.Hour, Timeout: 5 * time.Minute}
	for _, tc := range []struct {
		name, state, reason string
		change              func(*LocalRecord)
	}{
		{"complete", "healthy", "", func(*LocalRecord) {}},
		{"disabled", "unconfigured", "", func(r *LocalRecord) { r.Configured = false }},
		{"overdue", "overdue", "", func(r *LocalRecord) { r.LastSnapshotAt = now.Add(-2 * time.Hour).Format(time.RFC3339Nano) }},
		{"restored-history", "unknown", "no_completed_backup", func(r *LocalRecord) { r.SuccessInstance = "instance_" + strings.Repeat("b", 32) }},
		{"latest-failure", "failed", "last_attempt_failed", func(r *LocalRecord) { r.AttemptState = "failed" }},
		{"retention-failure", "failed", "retention_failed", func(r *LocalRecord) { r.RetentionWarning = true }},
		{"publication-held", "failed", "backup_held", func(r *LocalRecord) { r.State = "held" }},
		{"running", "running", "", func(r *LocalRecord) { r.State = "running" }},
		{"stuck-running", "unknown", "attempt_stale", func(r *LocalRecord) {
			r.State = "running"
			r.LastAttemptAt = now.Add(-time.Hour).Format(time.RFC3339Nano)
		}},
		{"future-time", "unknown", "invalid_backup_time", func(r *LocalRecord) { r.LastSnapshotAt = now.Add(time.Second).Format(time.RFC3339Nano) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := base
			tc.change(&v)
			r := Local(v, m.InstanceID, now)
			if r.State != tc.state || r.Reason != tc.reason {
				t.Fatal(r)
			}
		})
	}
}
