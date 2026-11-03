package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviews"
)

func TestSchedulingAutomaticWorkspaceBasisUsesTargetFirstCurrentReferences(t *testing.T) {
	f := frontFixture(t)
	for _, query := range []string{automaticCurrentRunsBasisQuery, automaticCurrentFindingsBasisQuery} {
		if automaticBasisQuery(query, false) != query {
			t.Fatal("ordinary query text changed")
		}
		rows, err := f.store.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+automaticBasisQuery(query, true), attemptProject, 2)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, "SEARCH c USING INDEX") || !strings.Contains(plan[0], "project_id=? AND pull_number=?") {
			t.Fatal("basis starts from project-wide history instead of selected current PR", plan)
		}
		for _, detail := range plan {
			if (strings.Contains(detail, "SEARCH v ") || strings.Contains(detail, "SEARCH f ")) && strings.HasSuffix(detail, "(project_id=?)") {
				t.Fatal("basis walks unrelated historical versions", plan)
			}
		}
	}
}

func TestSchedulingAutomaticHistoryChurnPreservesFactsAndDedupe(t *testing.T) {
	f := frontFixture(t)
	one := int64(1)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "one-target", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `UPDATE grants SET can_manage=1 WHERE credential_id='exec-writer-cred'`)
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	selected := schedulingLatest(t, f)
	if _, err := f.e.ControlOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "cancel-original", selected.OperationID, OperationControl{ExpectedRevision: 1, Action: "cancel"}); err != nil {
		t.Fatal(err)
	}
	basis := func() string {
		t.Helper()
		var sum string
		err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			var err error
			sum, err = automaticPullBasis(ctx, tx, attemptProject, 2)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	before := basis()
	observe := func(target reviews.PullInput, unrelated reviews.PullInput, key string) {
		t.Helper()
		var revision int64
		if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, attemptProject).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		request := reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 801, Source: "unrelated-history", ObservedAt: stamp(), Queue: []reviews.PullInput{frontMergedPull(), target, unrelated}}
		response := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, key, encoded(request))
		if response.Code != 201 {
			t.Fatal("history observation", response.Code, response.Body.String())
		}
	}
	for n := 0; n < 8; n++ {
		other := projectionPull(4)
		other.Runs = []reviews.RunInput{{SourceRunID: fmt.Sprintf("unrelated-%d", n), Channel: "hosted", HeadSHA: other.HeadSHA, Reviewer: "Historical reviewer", Scope: "Retained comparison", Outcome: "completed", Attributable: true, Complete: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Found: 1, Findings: []reviews.Finding{{SourceFindingID: "original", Title: "Retained finding", Detail: "Historical source detail", Severity: "low", Disposition: "unadjudicated"}}}}
		observe(projectionPull(2), other, fmt.Sprintf("history-%d", n))
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_run_versions WHERE project_id=?`, attemptProject) < 8 || attemptCount(t, f.store, `SELECT count(*) FROM review_finding_versions WHERE project_id=?`, attemptProject) < 8 {
		t.Fatal("fixture did not retain run/finding history")
	}
	if after := basis(); after != before {
		t.Fatal("unrelated historical versions changed selected source basis")
	}
	schedulingGenerate(t, f, false)
	changed := projectionPull(2)
	changed.HeadSHA = strings.Repeat("c", 40)
	observe(changed, projectionPull(4), "changed-current-head")
	if basis() == before {
		t.Fatal("actual selected source change did not change basis")
	}
	schedulingGenerate(t, f, true)
}
