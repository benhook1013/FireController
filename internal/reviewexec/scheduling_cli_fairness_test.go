package reviewexec

import (
	"testing"
)

func TestAutomaticCLITraversalReachesOrdinaryProjectBehindPreferredWindow(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	// Idle projects have an enabled schedule but haven't configured execution.
	// Insert durable fixture rows from an ordinary enabled template.
	schedulingCLIEnable(t, f, 0, true)
	admissionSQL(t, f, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<110)
		INSERT INTO projects(id,name,github_repository_id,repository)
		SELECT printf('preferred-%03d',i),printf('Preferred %03d',i),10000+i,'benhook1013/FireMUD' FROM n`)
	admissionSQL(t, f, `INSERT INTO review_scheduling(project_id,revision,automatic_hosted,automatic_cli,actor_id,worker_id,credential_id,updated_at)
		SELECT p.id,s.revision,s.automatic_hosted,s.automatic_cli,s.actor_id,s.worker_id,s.credential_id,s.updated_at
		FROM projects p CROSS JOIN review_scheduling s WHERE p.id LIKE 'preferred-%' AND s.project_id=?`, attemptProject)
	worked, err := f.e.generateAutomaticCLI(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
	if err != nil || !worked {
		t.Fatalf("preferred idle rows starved configured ordinary project: worked=%v err=%v", worked, err)
	}
	var selections int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM review_automatic_selections WHERE project_id=?", attemptProject).Scan(&selections); err != nil || selections != 1 {
		t.Fatalf("wanted exactly one ordinary-project selection, got %d, err=%v", selections, err)
	}
}
