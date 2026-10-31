package backuphealth

import (
	"strings"
	"testing"
	"time"
)

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
