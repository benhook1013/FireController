package reviewexec

import (
	"fmt"
	"testing"
)

func TestPreferredCLIAccountWindowIgnoresOtherAccounts(t *testing.T) {
	f := schedulingFixture(t)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	low := f.enqueue(t, attemptProject, "competing", "cli", 2, 1)
	high := f.enqueue(t, otherAttemptProject, "preferred", "cli", 2, 1)
	admissionSQL(t, f, `UPDATE projects SET repository=? WHERE id=?`, preferredCLIRepository, otherAttemptProject)

	for i := range 101/requestProjectCapacity + 1 {
		project := fmt.Sprintf("other-account-priority-%d", i)
		selectionCapacityProject(t, f, project, int64(900+i))
		admissionSQL(t, f, `UPDATE review_execution_config SET body_json=json_set(body_json, '$.capability_id', 'other-account') WHERE project_id=?`, project)
		selectionCapacityFill(t, f, project)
		admissionSQL(t, f, `UPDATE projects SET repository=? WHERE id=?`, preferredCLIRepository, project)
		admissionSQL(t, f, `UPDATE review_operations SET channel='cli',capability_fingerprint=?,created_at='2000-01-01T00:00:00Z' WHERE project_id=?`, f.e.capabilities["other-account"].Fingerprint, project)
	}
	if !schedulingCLI0030Priority(t, f, attemptProject, low.ID) {
		t.Fatal("older candidates on another account consumed the priority window")
	}
	admissionSQL(t, f, `UPDATE review_operations SET reason='cli_draft_wait' WHERE id=?`, high.ID)
	if schedulingCLI0030Priority(t, f, attemptProject, low.ID) {
		t.Fatal("other-account candidates or an ineligible shared-account candidate reserved priority")
	}
}
