package reviewexec

import (
	"encoding/json"
	"testing"
)

func TestSelectionRepositoryPreferenceRespectsInstalledAccount(t *testing.T) {
	for _, tc := range []struct {
		name, capability string
		prefer           bool
	}{
		{"same-account", "test-capability", true},
		{"different-account", "other-account", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := schedulingFixture(t)
			f.configure(t, otherAttemptProject, tc.capability, 0, false)
			admissionSQL(t, f, `UPDATE projects SET repository='benhook1013/FireMUD' WHERE id=?`, otherAttemptProject)
			ordinary, _ := selectionEnqueue(t, f, "account-ordinary", "cli", nil)
			scope := selectionScope(t, f, otherAttemptProject, f.other, "write")
			raw, err := f.e.EnqueueRun(t.Context(), scope, "account-preferred", RunRequest{Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			var preferred Receipt
			if err = json.Unmarshal(raw, &preferred); err != nil {
				t.Fatal(err)
			}
			admissionSQL(t, f, `UPDATE review_selections SET created_at='2026-01-01T00:00:01Z' WHERE id=?`, ordinary.ID)
			admissionSQL(t, f, `UPDATE review_selections SET created_at='2026-01-01T00:00:02Z' WHERE id=?`, preferred.ID)
			reader := &refreshTestReader{}
			worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": reader, tc.capability: reader})
			if !worked || err != nil {
				t.Fatal(worked, err)
			}
			left, right := ordinary.ID, preferred.ID
			if tc.prefer {
				left, right = right, left
			}
			var first, second string
			if err = f.store.DB().QueryRow(`SELECT state FROM review_selections WHERE id=?`, left).Scan(&first); err != nil {
				t.Fatal(err)
			}
			if err = f.store.DB().QueryRow(`SELECT state FROM review_selections WHERE id=?`, right).Scan(&second); err != nil {
				t.Fatal(err)
			}
			if first == "queued" || second != "queued" {
				t.Fatalf("expected first selected pass for %s; got %s, %s", left, first, second)
			}
		})
	}
}

// An ordinary local reservation remains authoritative without letting a
// conflicting preferred request consume priority on an independent account.
func TestPreferredCLIQueuedSkipsOccupiedTarget(t *testing.T) {
	f := schedulingFixture(t)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	low := f.enqueue(t, attemptProject, "priority-ordinary", "cli", 2, 1)
	f.enqueue(t, otherAttemptProject, "priority-preferred", "cli", 2, 1)
	admissionSQL(t, f, `UPDATE projects SET repository=? WHERE id=?`, preferredCLIRepository, otherAttemptProject)
	if !schedulingCLI0030Priority(t, f, attemptProject, low.ID) {
		t.Fatal("eligible preferred target lost priority")
	}
	// The fixture adds a synthetic existing reservation for the exact preferred
	// comparison. No provider or worker is invoked.
	admissionSQL(t, f, `INSERT INTO review_execution_attempts(id,operation_id,project_id,provider,account,state,revision,anchor_json,intent,external_id)
 SELECT 'fixture-reservation',id,project_id,'coderabbit','second-account','preparing',1,'null','','' FROM review_operations WHERE id=?`, low.ID)
	admissionSQL(t, f, `INSERT INTO review_execution_reservations(provider,repository_id,pull_number,action_class,account,attempt_id)
 SELECT 'coderabbit',github_repository_id,2,'cli','second-account','fixture-reservation' FROM projects WHERE id=?`, otherAttemptProject)
	if schedulingCLI0030Priority(t, f, attemptProject, low.ID) {
		t.Fatal("occupied preferred target still consumes priority")
	}
}
