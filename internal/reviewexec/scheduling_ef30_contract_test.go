package reviewexec

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestOccupiedPlannerLeavesExplicitSelectionAvailable(t *testing.T) {
	f := schedulingFixture(t)
	receipt, _ := selectionEnqueue(t, f, "without-planner-slot", "cli", nil)
	f.e.automaticMu.Lock()
	defer f.e.automaticMu.Unlock()
	if worked, err := f.e.coordinateAutomaticHosted(t.Context(), nil); worked || err != nil {
		t.Fatal(worked, err)
	}
	selectionNext(t, f, &refreshTestReader{})
	if got := selectionRead(t, f, receipt.ID); got.State != "selected" || got.OperationID == "" {
		t.Fatal("explicit selection failed", got)
	}
}

func TestSchedulingAllOffRequiresAnActiveWorker(t *testing.T) {
	f := schedulingFixture(t)
	before := snapshotTable(t, f, "review_scheduling")
	attempt := func(worker, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/scheduling", strings.NewReader(`{"expected_revision":0,"automatic_hosted":false,"automatic_cli":false}`))
		r.Header.Set("Authorization", "Bearer "+f.admin)
		if worker != "" {
			r.Header.Set("X-FC-Worker", worker)
		}
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	if w := attempt("", "missing-with-all-off"); w.Code != http.StatusNotFound {
		t.Fatal(w.Code, w.Body.String())
	}
	if before != snapshotTable(t, f, "review_scheduling") {
		t.Fatal("unattributed request wrote state")
	}
	if w := attempt("selection-worker", "bound-with-all-off"); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestPreferredCLIQueuedDoesNotYieldForBlockedSuccessor(t *testing.T) {
	f := schedulingFixture(t)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	low := f.enqueue(t, attemptProject, "ordinary", "cli", 2, 1)
	f.enqueue(t, otherAttemptProject, "first-preferred", "cli", 2, 1)
	f.enqueue(t, otherAttemptProject, "second-preferred", "cli", 4, 1)
	admissionSQL(t, f, `UPDATE projects SET repository=? WHERE id=?`, preferredCLIRepository, otherAttemptProject)
	if !schedulingCLI0030Priority(t, f, attemptProject, low.ID) {
		t.Fatal("eligible predecessor did not retain priority")
	}
	admissionSQL(t, f, `INSERT INTO review_execution_attempts(id,operation_id,project_id,provider,account,state,revision,anchor_json,intent,external_id)
 SELECT 'prior-reservation',id,project_id,'coderabbit','second-account','preparing',1,'null','','' FROM review_operations WHERE id=?`, low.ID)
	admissionSQL(t, f, `INSERT INTO review_execution_reservations(provider,repository_id,pull_number,action_class,account,attempt_id)
 SELECT 'coderabbit',github_repository_id,2,'cli','second-account','prior-reservation' FROM projects WHERE id=?`, otherAttemptProject)
	if schedulingCLI0030Priority(t, f, attemptProject, low.ID) {
		t.Fatal("blocked successor still consumes priority")
	}
}

func TestAutomaticCLITransientAccountLookupDoesNotStarveOrdinaryProject(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "shared-storage"}[fatal], func(t *testing.T) {
			f := schedulingFixture(t)
			f.configure(t, otherAttemptProject, "other-account", 0, false)
			admissionSQL(t, f, `UPDATE projects SET repository=? WHERE id=?`, preferredCLIRepository, attemptProject)
			schedulingCLIEnable(t, f, 0, true)
			zero, off, on := int64(0), false, true
			if _, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, otherAttemptProject, f.admin, "write"), "other-active", SchedulingRequest{ExpectedRevision: &zero, AutomaticHosted: &off, AutomaticCLI: &on}); err != nil {
				t.Fatal(err)
			}
			lookupErr := core.Fail("overloaded", "bounded local lookup fixture", 503)
			if fatal {
				lookupErr = core.Fail("storage_unavailable", "shared store lookup fixture", 500)
			}
			calls := []string{}
			worked, err := f.e.generateAutomaticCLIWithAccountLookup(t.Context(), map[string]TargetReader{"other-account": &refreshTestReader{}}, func(ctx context.Context, project string) (string, error) {
				calls = append(calls, project)
				if project == attemptProject {
					return "", lookupErr
				}
				return f.e.automaticCLIAccount(ctx, project)
			})
			n := attemptCount(t, f.store, `SELECT count(*) FROM review_automatic_selections WHERE project_id=?`, otherAttemptProject)
			if fatal {
				if worked || !errors.Is(err, lookupErr) || len(calls) != 1 || n != 0 {
					t.Fatal(worked, err, calls, n)
				}
				return
			}
			if !worked || err != nil || len(calls) != 2 || calls[0] != attemptProject || calls[1] != otherAttemptProject || n != 1 {
				t.Fatal(worked, err, calls, n)
			}
		})
	}
}
