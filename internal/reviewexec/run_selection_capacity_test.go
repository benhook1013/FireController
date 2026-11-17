package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func selectionCapacityProject(t *testing.T, f *attemptFixture, project string, repository int64) {
	t.Helper()
	admissionSQL(t, f, `INSERT INTO projects(id,name,github_repository_id,repository) VALUES(?,?,?,'owner/repo')`, project, project, repository)
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, project)
	admissionSQL(t, f, `INSERT INTO review_execution_config SELECT ?,revision,body_json FROM review_execution_config WHERE project_id=?`, project, attemptProject)
	admissionSQL(t, f, `INSERT INTO review_policy_config SELECT ?,revision,body_json,actor_id,created_at FROM review_policy_config WHERE project_id=?`, project, attemptProject)
}

func selectionCapacityFill(t *testing.T, f *attemptFixture, project string) {
	t.Helper()
	// Unrelated Hosted requests occupy ordinary capacity without the CLI
	// selection's distinct review_inflight guard becoming the cause of refusal.
	scope := selectionScope(t, f, project, f.admin, "write")
	admissionSQL(t, f, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?)
 INSERT INTO review_operations(id,project_id,actor_id,worker_id,configuration_revision,policy_revision,capability_fingerprint,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at)
 SELECT 'capacity-'||?||'-'||x,?,?,?,1,1,?,4,'hosted',?,'queued',1,'','',?,? FROM n`, requestProjectCapacity, project, project, scope.PrincipalID, scope.WorkerID, f.e.capabilities["test-capability"].Fingerprint, strings.Repeat("a", 40), stamp(), stamp())
}

func selectionCapacityRelease(t *testing.T, f *attemptFixture, project string) {
	t.Helper()
	admissionSQL(t, f, `UPDATE review_operations SET state='held',reason='synthetic_capacity_release' WHERE id=?`, "capacity-"+project+"-1")
}

func selectionCapacityResult(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("selection did not yield cleanly", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("full capacity occupied the selection worker")
	}
}

func TestRunSelectionCapacityRaceYieldsAndRecoversOriginalSelection(t *testing.T) {
	for _, kind := range []string{"project", "global"} {
		t.Run(kind, func(t *testing.T) {
			f := selectionFixtureWithQueue(t, []int64{2})
			receipt, original := selectionEnqueue(t, f, "capacity-race", "cli", nil)
			reader, entered, release := selectionContentionReader()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := f.e.runNext(ctx, map[string]TargetReader{"test-capability": reader})
				done <- err
				close(done)
			}()
			t.Cleanup(func() { cancel(); selectionCapacityResult(t, done) })
			selectionContentionEntered(t, entered)
			occupied, total := attemptProject, requestProjectCapacity
			if kind == "global" {
				total = requestGlobalCapacity
				for i := range requestGlobalCapacity / requestProjectCapacity {
					project := fmt.Sprintf("capacity-project-%d", i)
					selectionCapacityProject(t, f, project, int64(900+i))
					selectionCapacityFill(t, f, project)
					occupied = project
				}
			} else {
				selectionCapacityFill(t, f, occupied)
			}
			close(release)
			selectionCapacityResult(t, done)
			got := selectionRead(t, f, receipt.ID)
			if got.State != "queued" || got.Revision != 3 || got.OperationID != "" || got.Reason != "" || reader.calls.Load() != 2 {
				t.Fatal("capacity race lost pending selection", got, reader.calls.Load())
			}
			before := selectionCounts(t, f)
			for range 2 {
				worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": reader})
				if err != nil || worked || reader.calls.Load() != 2 || selectionCounts(t, f) != before {
					t.Fatal("full capacity repeated source work or durable mutations", worked, err)
				}
			}
			replayed, err := f.e.EnqueueRun(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "capacity-race", RunRequest{Channel: "cli"})
			if err != nil || !bytes.Equal(replayed, original) || selectionCounts(t, f) != before {
				t.Fatal("capacity wait changed original-key replay", err)
			}
			selectionCapacityRelease(t, f, occupied)
			fresh := &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
				return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("c", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
			}}
			selectionNext(t, f, fresh)
			got = selectionRead(t, f, receipt.ID)
			op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), got.OperationID)
			if err != nil || got.State != "selected" || got.Revision != 5 || op.HeadSHA != strings.Repeat("c", 40) || fresh.calls.Load() != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != total+1 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.requested'`) != 1 {
				t.Fatal("capacity recovery lost fresh source or duplicated child", got, op, err)
			}
		})
	}
}

func TestRunSelectionCapacitySkipsTwoSaturatedProjectsAndCancels(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{2})
	for i := range 2 {
		project := fmt.Sprintf("saturated-project-%d", i)
		selectionCapacityProject(t, f, project, int64(900+i))
		scope := selectionScope(t, f, project, f.admin, "write")
		raw, err := f.e.EnqueueRun(t.Context(), scope, "saturated", RunRequest{Channel: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		var receipt Receipt
		if err := json.Unmarshal(raw, &receipt); err != nil {
			t.Fatal(err)
		}
		selectionCapacityFill(t, f, project)
	}
	eligible, _ := selectionEnqueue(t, f, "eligible-after-saturated", "cli", nil)
	reader := &refreshTestReader{}
	cancel, done := selectionContentionStart(t, f, reader)
	selectionContentionWait(t, f, eligible.ID, "selected")
	selectionContentionStop(t, cancel, done)
	if reader.calls.Load() != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_selections WHERE state='queued' AND revision=1`) != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE channel='cli'`) != 1 {
		t.Fatal("saturated projects monopolized workers or acquired source", reader.calls.Load())
	}
}

func TestRunSelectionCapacityReleaseRechecksBinding(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{2})
	receipt, _ := selectionEnqueue(t, f, "capacity-binding", "cli", nil)
	selectionCapacityFill(t, f, attemptProject)
	reader := &refreshTestReader{}
	worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": reader})
	if err != nil || worked || reader.calls.Load() != 0 {
		t.Fatal("saturated project acquired source", worked, err)
	}
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	selectionCapacityRelease(t, f, attemptProject)
	selectionNext(t, f, reader)
	got := selectionContentionRead(t, f, receipt.ID)
	if got.State != "held" || got.Reason != "authorization_changed" || reader.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE channel='cli'`) != 0 {
		t.Fatal("capacity wait preserved withdrawn authority", got, reader.calls.Load())
	}
}

func TestRunSelectionCapacityPreservesActualChild409AndSavepoint(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{2})
	roundGrant(t, f, 2, "cli", 0, 0, 2, "open-round")
	receipt, _ := selectionEnqueue(t, f, "child-conflict", "cli", nil)
	// Change the round inside child insertion, after the front was checked.
	// The native admission helper then returns its real 409 after inserting a
	// child. Savepoint rollback must discard both that child and this trigger.
	admissionSQL(t, f, `CREATE TRIGGER selection_capacity_conflict AFTER INSERT ON review_operations WHEN NEW.channel='cli' BEGIN UPDATE review_round_controls SET closed=1 WHERE project_id=NEW.project_id AND pull_number=NEW.pull_number AND channel=NEW.channel; END`)
	selectionNext(t, f, &refreshTestReader{})
	got := selectionRead(t, f, receipt.ID)
	if got.State != "held" || got.Reason != "review_round_closed" || got.OperationID != "" || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.requested'`) != 0 || attemptCount(t, f.store, `SELECT closed FROM review_round_controls`) != 0 {
		t.Fatal("actual 409 retried or retained partial child work", got)
	}
}
