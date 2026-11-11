package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"strings"
	"testing"
	"time"

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

func TestAutomaticIdlePlanningDoesNotEnterWriter(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, idle := range []string{"paused", "pending", "cancelled", "held-source-not-due"} {
			t.Run(channel+"/"+idle, func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2})
				on, zero := true, int64(0)
				scope := selectionScope(t, f, attemptProject, f.writer, "write")
				if _, err := f.e.ConfigureScheduling(t.Context(), scope, "idle-enable", SchedulingRequest{ExpectedRevision: &zero, AutomaticHosted: &on, AutomaticCLI: &on}); err != nil {
					t.Fatal(err)
				}
				reader := &refreshTestReader{}
				readers := map[string]TargetReader{"test-capability": reader}
				generate := f.e.generateAutomaticHosted
				if channel == "cli" {
					generate = f.e.generateAutomaticCLI
				}
				if idle == "paused" {
					schedulingPause(t, f, 2, 0, true)
				} else {
					if worked, err := generate(t.Context(), readers); err != nil || !worked {
						t.Fatal(worked, err)
					}
					if idle == "cancelled" {
						selectionNext(t, f, reader)
						r := schedulingLatest(t, f)
						if _, err := f.e.ControlOperation(t.Context(), scope, "cancel-idle", r.OperationID, OperationControl{ExpectedRevision: 1, Action: "cancel"}); err != nil {
							t.Fatal(err)
						}
					} else if idle == "held-source-not-due" {
						selectionNext(t, f, &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
							return reviewprovider.Repository{}, context.DeadlineExceeded
						}})
					}
				}
				beforeCalls := reader.calls.Load()
				entered, release := make(chan struct{}), make(chan struct{})
				done := make(chan error, 1)
				go func() {
					done <- f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
						if _, err := tx.ExecContext(ctx, `UPDATE projects SET name='writer held' WHERE id=?`, attemptProject); err != nil {
							return err
						}
						close(entered)
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					})
				}()
				defer func() {
					close(release)
					if err := <-done; err != nil {
						t.Error(err)
					}
				}()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("writer not held")
				}
				ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
				defer cancel()
				worked, err := generate(ctx, readers)
				if err != nil || worked || ctx.Err() != nil {
					t.Fatal("idle scheduler waited for writer", worked, err, ctx.Err())
				}
				if reader.calls.Load() != beforeCalls {
					t.Fatal("local planning contacted provider")
				}
			})
		}
	}
}

func TestAutomaticWriterRechecksReadPlan(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, change := range []string{"unchanged", "restore", "revoked", "scheduling", "configuration", "policy", "pull-pause", "basis", "competing-selection"} {
			t.Run(channel+"/"+change, func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2})
				on, zero := true, int64(0)
				if _, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "writer-enable", SchedulingRequest{ExpectedRevision: &zero, AutomaticHosted: &on, AutomaticCLI: &on}); err != nil {
					t.Fatal(err)
				}
				readers := map[string]TargetReader{"test-capability": &refreshTestReader{}}
				var candidate *automaticProjectPlan
				if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					var err error
					candidate, err = f.e.planAutomaticProject(ctx, tx, attemptProject, channel, readers)
					return err
				}); err != nil || candidate == nil {
					t.Fatal("candidate not nominated", candidate, err)
				}
				// Deterministic interleaving: commit a competing change after the exact
				// read preflight, then run the same writer path used by the generator.
				switch change {
				case "restore":
					admissionSQL(t, f, `INSERT INTO metadata VALUES('restore:provider_state','held')`)
				case "revoked":
					admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
				case "scheduling":
					schedulingEnable(t, f, 1, true)
				case "configuration":
					f.configure(t, attemptProject, "test-capability", 1, false)
				case "policy":
					// Integrated policy has no channel-pause field. A policy revision
					// still changes the basis and invalidates this nominated plan.
					assessmentPolicy(t, f, 1, []string{"cli", "hosted"})
				case "pull-pause":
					schedulingPause(t, f, 2, 0, true)
				case "basis":
					admissionSQL(t, f, `INSERT INTO review_pull_scheduling VALUES(?,2,1,0,'','actor',?)`, attemptProject, stamp())
				case "competing-selection":
					selectionEnqueue(t, f, "competing-explicit", channel, nil)
				}
				worked, err := f.e.acceptAutomaticProject(t.Context(), attemptProject, channel, readers, candidate)
				if err != nil || worked != (change == "unchanged") {
					t.Fatal("writer accepted stale or refused unchanged plan", worked, err)
				}
				want := 0
				if change == "unchanged" {
					want = 1
					// The same nominated plan cannot admit a second selection.
					if worked, err = f.e.acceptAutomaticProject(t.Context(), attemptProject, channel, readers, candidate); err != nil || worked {
						t.Fatal("repeated plan created duplicate intent", worked, err)
					}
				}
				if got := attemptCount(t, f.store, `SELECT count(*) FROM review_automatic_selections`); got != want {
					t.Fatal("unexpected automatic attribution", got)
				}
			})
		}
	}
}

func TestAutomaticConcurrentProjectPlansAcceptOnce(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		t.Run(channel, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			on, zero := true, int64(0)
			if _, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "concurrent-enable", SchedulingRequest{ExpectedRevision: &zero, AutomaticHosted: &on, AutomaticCLI: &on}); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			type result struct {
				worked bool
				err    error
			}
			results := make(chan result, 2)
			for range 2 {
				go func() {
					<-start
					worked, err := f.e.generateAutomaticProject(t.Context(), attemptProject, channel, map[string]TargetReader{"test-capability": &refreshTestReader{}})
					results <- result{worked, err}
				}()
			}
			close(start)
			accepted := 0
			for range 2 {
				r := <-results
				if r.err != nil {
					t.Fatal(r.err)
				}
				if r.worked {
					accepted++
				}
			}
			if accepted != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_automatic_selections`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 1 {
				t.Fatal("concurrent preflights did not serialize one acceptance", accepted)
			}
		})
	}
}
