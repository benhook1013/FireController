package reviewexec

import (
	"testing"
)

func TestHostedDamagedWaitControlPreservesEvidence(t *testing.T) {
	for _, dependency := range []string{"review_hosted_preflight", "review_hosted_allowance_queries"} {
		t.Run(dependency, func(t *testing.T) {
			f, a, _, _ := queuedCooldown(t)
			scope := f.scope(t, attemptProject, f.writer, false)
			o := bindingOperation(t, f, scope, a.OperationID)
			admissionSQL(t, f, "UPDATE "+dependency+" SET body_json='damaged' WHERE attempt_id=?", a.ID)
			tables := []string{"review_operations", "review_execution_attempts", "review_operation_waits", dependency, "events", "receipts"}
			before := map[string]string{}
			for _, table := range tables {
				before[table] = snapshotTable(t, f, table)
			}
			for _, action := range []string{"cancel", "pause", "resume"} {
				_, err := f.e.ControlOperation(t.Context(), scope, "damaged-"+action, o.ID, OperationControl{ExpectedRevision: o.Revision, Action: action})
				attemptCode(t, err, "storage_unavailable")
			}
			for _, table := range tables {
				if before[table] != snapshotTable(t, f, table) {
					t.Fatal("damaged control mutated history", table)
				}
			}
		})
	}
}
