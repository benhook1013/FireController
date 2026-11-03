package reviewexec

import (
	"strings"
	"testing"
)

func TestPolicyChannelPauseMigrationPreservesHistoryAndStops(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "stopped"}[paused], func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			legacy := `{"required_channels":["hosted","cli"],"paused_channels":[],"max_review_files":0}`
			if paused {
				legacy = strings.Replace(legacy, `"paused_channels":[]`, `"paused_channels":["cli"]`, 1)
			}
			admissionSQL(t, f, `UPDATE review_policy_config SET body_json=? WHERE project_id=?`, legacy, attemptProject)
			admissionSQL(t, f, `UPDATE metadata SET value='1' WHERE key='schema:review_policy'`)
			var config string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_config WHERE project_id=? AND revision=1`, attemptProject).Scan(&config); err != nil {
				t.Fatal(err)
			}
			if err := EnsurePolicySchema(t.Context(), f.store); err != nil {
				t.Fatal(err)
			}
			got := policyRead(t, f, attemptProject, f.reader)
			if got.Revision != 2 || strings.Contains(encoded(got.Policy), "paused_channels") {
				t.Fatal(got)
			}
			var old, oldConfig string
			_ = f.store.DB().QueryRow(`SELECT body_json FROM review_policy_config WHERE project_id=? AND revision=1`, attemptProject).Scan(&old)
			_ = f.store.DB().QueryRow(`SELECT body_json FROM review_execution_config WHERE project_id=? AND revision=1`, attemptProject).Scan(&oldConfig)
			if old != legacy || oldConfig != config {
				t.Fatal("migration rewrote retained originals")
			}
			current, err := f.e.ReadExecutionConfiguration(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader))
			if err != nil || current.Paused != paused {
				t.Fatal(current, err)
			}
			before := policyState(t, f)
			if err := EnsurePolicySchema(t.Context(), f.store); err != nil || policyState(t, f) != before {
				t.Fatal("migration not idempotent", err)
			}
		})
	}
}
func TestPolicyChannelPauseMigrationRollsBack(t *testing.T) {
	f := newAttemptFixture(t)
	legacy := `{"required_channels":["cli"],"paused_channels":[],"max_review_files":0}`
	admissionSQL(t, f, `UPDATE review_policy_config SET body_json=? WHERE project_id=?`, legacy, attemptProject)
	admissionSQL(t, f, `UPDATE metadata SET value='1' WHERE key='schema:review_policy'`)
	admissionSQL(t, f, `CREATE TRIGGER policy_migration_fail BEFORE INSERT ON events WHEN NEW.actor_id='system_review_channel_pause_removal' BEGIN SELECT RAISE(ABORT,'injected'); END`)
	before := policyState(t, f)
	if EnsurePolicySchema(t.Context(), f.store) == nil || policyState(t, f) != before {
		t.Fatal("failed migration changed history")
	}
	var version string
	_ = f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_policy'`).Scan(&version)
	if version != "1" {
		t.Fatal(version)
	}
}
