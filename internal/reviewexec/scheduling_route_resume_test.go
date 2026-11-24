package reviewexec

import "testing"

func TestAutomaticRouteProjectionRebuildReconsidersSameBasis(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	first := schedulingLatest(t, f)
	admissionSQL(t, f, `INSERT INTO review_route_policy_invalidations(project_id,target_pull_number,route_id) VALUES (?,2,'fixture')`, attemptProject)
	selectionNext(t, f, &refreshTestReader{})
	got := selectionRead(t, f, first.ID)
	if got.State != "held" || got.Reason != "routed_findings_unavailable" || got.OperationID != "" {
		t.Fatalf("did not reach exact typed route hold: %+v", got)
	}
	// This marker is the only fixture damage. Its removal models the same
	// receiving facts after an independently validated projection rebuild.
	ageSourceRetry(t, f, first.ID)
	schedulingGenerate(t, f, false)
	if still := selectionRead(t, f, first.ID); still.Revision != got.Revision {
		t.Fatal("retry consumed while projection is still dirty", still)
	}
	admissionSQL(t, f, `DELETE FROM review_route_policy_invalidations WHERE project_id=? AND target_pull_number=2 AND route_id='fixture'`, attemptProject)
	schedulingGenerate(t, f, true)
	fresh := schedulingLatest(t, f)
	if fresh.ID != first.ID || fresh.Revision != got.Revision+1 || fresh.State != "queued" {
		t.Fatalf("repair did not requeue exactly the original selection: %+v", fresh)
	}
	selectionNext(t, f, &refreshTestReader{})
	got = selectionRead(t, f, fresh.ID)
	if got.State != "selected" || got.OperationID == "" {
		t.Fatalf("clean receiving facts failed to be reconsidered: %+v", got)
	}
}

func TestAutomaticDirtyRouteRetriesDoNotResetBudget(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	first := schedulingLatest(t, f)
	for _, revision := range []int64{3, 6, 9} {
		// Reintroduce fixture damage after the coordinator's preflight.
		admissionSQL(t, f, `INSERT INTO review_route_policy_invalidations(project_id,target_pull_number,route_id) VALUES (?,2,'fixture')`, attemptProject)
		selectionNext(t, f, &refreshTestReader{})
		got := selectionRead(t, f, first.ID)
		if got.State != "held" || got.Reason != "routed_findings_unavailable" || got.Revision != revision || got.OperationID != "" {
			t.Fatalf("unexpected hold after route dirty race: %+v", got)
		}
		admissionSQL(t, f, `DELETE FROM review_route_policy_invalidations WHERE project_id=? AND target_pull_number=2 AND route_id='fixture'`, attemptProject)
		ageSourceRetry(t, f, first.ID)
		schedulingGenerate(t, f, revision < 9)
	}
}
