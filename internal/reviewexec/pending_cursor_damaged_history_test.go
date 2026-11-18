package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
	"testing"
	"time"
)

func TestPendingCursorDamagedHistoryCannotRewindForever(t *testing.T) {
	f := supervisorFixture(t)
	bad := f.enqueue(t, attemptProject, "first-damaged-history", "cli", 2, 1)
	slow := f.enqueue(t, attemptProject, "later-in-project", "cli", 4, 1)
	revision := int64(1)
	c := Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4, 6}, Channels: []string{"cli", "hosted"}}
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "suffix-add", c); err != nil {
		t.Fatal(err)
	}
	revision = 2
	c.Queue = []int64{2, 4}
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "suffix-remove", c); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `UPDATE review_execution_config SET body_json='broken' WHERE project_id=? AND revision=2`, attemptProject)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	healthy := f.enqueue(t, otherAttemptProject, "healthy-later", "cli", 2, 1)
	admissionSQL(t, f, `UPDATE review_operations SET created_at='2020-01-01T00:00:00Z' WHERE id=?`, bad.ID)
	admissionSQL(t, f, `UPDATE review_operations SET created_at='2020-01-02T00:00:00Z' WHERE id=?`, slow.ID)
	var seen []string
	driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
		seen = append(seen, o.ID)
		return Preparation{}, core.Fail("review_evidence_pending", "fixture hold", 409)
	}}
	for pass := range 3 {
		calls := 0
		// Inject slow damaged-prefix validation only while the scan still
		// starts before that prefix, never into the healthy final claim.
		resume := f.e.pendingScanCursor[0].id
		f.e.schedulingNow = func() time.Time {
			calls++
			if calls == 2 && resume != slow.ID {
				time.Sleep(2100 * time.Millisecond)
			}
			return time.Now().UTC().Add(time.Duration(pass*60+calls) * time.Second)
		}
		if err := f.e.processNextForWorker(t.Context(), map[string]Driver{"test-capability": driver, "other-account": driver}, false); err != nil {
			t.Fatal(err)
		}
		t.Log("pass", pass, "cursor", f.e.pendingScanCursor[0], "prepared", seen)
	}
	if len(seen) != 1 || seen[0] != healthy.ID {
		t.Fatal("damaged returned candidate starved eligible independent work", seen)
	}
	if driver.calls.Load() != 0 {
		t.Fatal("fixture dispatched provider work")
	}
}
