package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func cliSourceRetryFixture(t *testing.T) (*attemptFixture, Run) {
	t.Helper()
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingCLIEnable(t, f, 0, true)
	schedulingCLIGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
		return reviewprovider.Repository{}, context.DeadlineExceeded
	}})
	r := schedulingLatest(t, f)
	if r.Channel != "cli" || r.State != "held" || r.Revision != 3 || r.OperationID != "" {
		t.Fatal(r)
	}
	return f, r
}

func TestAutomaticCLISourceRetryKeepsOriginalIntentAndBudget(t *testing.T) {
	for _, mode := range []string{"success", "budget"} {
		t.Run(mode, func(t *testing.T) {
			f, original := cliSourceRetryFixture(t)
			schedulingCLIGenerate(t, f, false)
			for _, revision := range []int64{6, 9} {
				ageSourceRetry(t, f, original.ID)
				restarted, err := New(f.store, attemptCapabilities)
				if err != nil {
					t.Fatal(err)
				}
				f.e = restarted
				if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
					t.Fatal(err)
				}
				schedulingCLIGenerate(t, f, true)
				r := schedulingLatest(t, f)
				if r.ID != original.ID || r.ActorID != original.ActorID || r.WorkerID != original.WorkerID || r.ConfigurationRevision != original.ConfigurationRevision || r.PolicyRevision != original.PolicyRevision {
					t.Fatal("recovery replaced intent", r)
				}
				schedulingCLIGenerate(t, f, false)
				reader := &refreshTestReader{}
				if mode == "budget" {
					reader.repository = func(context.Context, int64) (reviewprovider.Repository, error) {
						return reviewprovider.Repository{}, context.DeadlineExceeded
					}
				}
				selectionNext(t, f, reader)
				r = schedulingLatest(t, f)
				if r.Revision != revision {
					t.Fatal(r)
				}
				if mode == "success" {
					if r.State != "selected" || r.OperationID == "" {
						t.Fatal(r)
					}
					if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
						t.Fatal("recovery duplicated child")
					}
					break
				}
				if r.State != "held" || r.OperationID != "" {
					t.Fatal(r)
				}
			}
			ageSourceRetry(t, f, original.ID)
			schedulingCLIGenerate(t, f, false)
			if attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
				t.Fatal("recovery manufactured intent or provider work")
			}
		})
	}
}

func TestAutomaticCLISourceRetrySafetyFences(t *testing.T) {
	for _, mode := range []string{"revoked", "disabled", "pause", "restore", "later-cli", "later-hosted", "rate-limited", "configuration"} {
		t.Run(mode, func(t *testing.T) {
			f, r := cliSourceRetryFixture(t)
			ageSourceRetry(t, f, r.ID)
			switch mode {
			case "revoked":
				admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
			case "disabled":
				schedulingCLIEnable(t, f, 1, false)
			case "pause":
				schedulingPause(t, f, 2, 0, true)
			case "restore":
				admissionSQL(t, f, `INSERT INTO metadata VALUES('restore:provider_state','held')`)
			case "later-cli", "later-hosted":
				channel := "cli"
				if mode == "later-hosted" {
					channel = "hosted"
				}
				later, _ := selectionEnqueue(t, f, "later-recovery-fence", channel, nil)
				// A later settled intent must still prevent revival of the old one.
				admissionSQL(t, f, `UPDATE review_selections SET state='held',reason='selection_changed',revision=revision+1 WHERE id=?`, later.ID)
			case "rate-limited":
				admissionSQL(t, f, `UPDATE review_selections SET reason='source_rate_limited' WHERE id=?`, r.ID)
			case "configuration":
				f.configure(t, attemptProject, "other-account", 1, false)
			}
			if mode == "pause" || mode == "restore" {
				schedulingCLIGenerate(t, f, false)
				if got := selectionRead(t, f, r.ID); got.State != "held" || got.Revision != r.Revision {
					t.Fatal("coordinator safety hold revived selection", got)
				}
				return
			}
			// Deliberate control changes can authorize a new semantic intent; inspect only
			// whether this exact retained source hold may be recovered under its authority.
			var worked bool
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				var basis string
				if err := tx.QueryRowContext(ctx, `SELECT settled_basis_sha256 FROM review_automatic_selections WHERE selection_id=?`, r.ID).Scan(&basis); err != nil {
					return err
				}
				s, err := readScheduling(ctx, tx, r.ProjectID)
				if err != nil {
					return err
				}
				worked, err = f.e.retryAutomaticSourceChannelTx(ctx, tx, r.ProjectID, basis, s.Revision, time.Now(), "cli")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if worked != (mode == "later-hosted") {
				t.Fatalf("%s: recovery=%v", mode, worked)
			}
		})
	}
}

func TestAutomaticGeneratorsIsolateRetainedParentCycle(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		t.Run(channel, func(t *testing.T) {
			f := schedulingFixture(t)
			f.configure(t, otherAttemptProject, "test-capability", 0, false)
			admissionSQL(t, f, `UPDATE review_execution_config SET body_json=json_set(body_json,'$.mode','stacked') WHERE project_id=?`, attemptProject)
			now, sha := stamp(), strings.Repeat("a", 40)
			prefix := []frontPull{{Number: 2, State: "open", HeadSHA: sha, BaseSHA: sha, BaseRef: "four", ParentPullNumber: 4, ParentSHA: sha, ObservedAt: now}, {Number: 4, State: "open", HeadSHA: sha, BaseSHA: sha, BaseRef: "two", ParentPullNumber: 2, ParentSHA: sha, ObservedAt: now}}
			raw, err := json.Marshal(prefix)
			if err != nil {
				t.Fatal(err)
			}
			admissionSQL(t, f, `INSERT INTO review_front_frames VALUES(?,?,801,1,?,?,1,'',?)`, attemptProject, channel, now, now, string(raw))
			// Prove the fixture reaches the narrow cycle error, not generic corruption.
			err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				_, err := automaticDependenciesChannel(ctx, tx, attemptProject, 2, channel)
				return err
			})
			if !errors.Is(err, errAutomaticParentCycle) {
				t.Fatal("invalid cycle fixture", err)
			}
			for _, project := range []string{attemptProject, otherAttemptProject} {
				zero := int64(0)
				hosted, cli := channel == "hosted", channel == "cli"
				_, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, project, f.admin, "write"), core.NewID("enable"), SchedulingRequest{ExpectedRevision: &zero, AutomaticHosted: &hosted, AutomaticCLI: &cli})
				if err != nil {
					t.Fatal(err)
				}
			}
			readers := map[string]TargetReader{"test-capability": &refreshTestReader{}}
			generate := f.e.generateAutomaticHosted
			if channel == "cli" {
				generate = f.e.generateAutomaticCLI
			}
			worked, err := generate(t.Context(), readers)
			if err != nil || !worked {
				t.Fatal("project cycle stopped healthy project", worked, err)
			}
			var project string
			if err := f.store.DB().QueryRow(`SELECT project_id FROM review_automatic_selections`).Scan(&project); err != nil || project != otherAttemptProject {
				t.Fatal(project, err)
			}
			// Shared storage failure must still reach the coordinator rather than being hidden.
			admissionSQL(t, f, `ALTER TABLE review_front_frames RENAME TO missing_front_frames`)
			if _, err := generate(t.Context(), readers); err == nil {
				t.Fatal("shared storage failure was swallowed")
			}
		})
	}
}

func TestAutomaticCLISourceRetryRechecksFencesAfterSourceIO(t *testing.T) {
	for _, mode := range []string{"cancelled-operation", "scheduling", "configuration", "policy"} {
		t.Run(mode, func(t *testing.T) {
			f, r := cliSourceRetryFixture(t)
			ageSourceRetry(t, f, r.ID)
			schedulingCLIGenerate(t, f, true)
			var once sync.Once
			reader := &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
				once.Do(func() {
					switch mode {
					case "cancelled-operation":
						raw, err := f.e.Enqueue(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "during-source", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: strings.Repeat("a", 40), Channel: "cli"})
						if err != nil {
							t.Fatal(err)
						}
						var receipt Receipt
						if err = json.Unmarshal(raw, &receipt); err != nil {
							t.Fatal(err)
						}
						_, err = f.e.ControlOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "cancel-during-source", receipt.ID, OperationControl{ExpectedRevision: 1, Action: "cancel"})
						if err != nil {
							t.Fatal(err)
						}
					case "scheduling":
						schedulingCLIEnable(t, f, 1, true)
					case "configuration":
						f.configure(t, attemptProject, "test-capability", 1, false)
					case "policy":
						assessmentPolicy(t, f, 1, []string{"cli"}, []string{})
					}
				})
				return reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, nil
			}}
			selectionNext(t, f, reader)
			got := selectionRead(t, f, r.ID)
			if got.State != "held" || got.OperationID != "" {
				t.Fatal("changed authority or cancelled successor ignored", got)
			}
			expected := 0
			if mode == "cancelled-operation" {
				expected = 1
				if got.Reason != "selection_superseded" {
					t.Fatal(got)
				}
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != expected {
				t.Fatal("retry created a child after its fence changed")
			}
		})
	}
}

func TestAutomaticCoordinatorIsolatesProjectState(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, fault := range []string{"policy", "policy-type", "scheduling", "scheduling-type", "pull-scheduling", "pull-scheduling-type", "front", "oversized-front", "oversized-basis", "retry-selection"} {
			t.Run(channel+"/"+fault, func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2})
				f.configure(t, otherAttemptProject, "test-capability", 0, false)
				for _, project := range []string{attemptProject, otherAttemptProject} {
					zero, on := int64(0), true
					if _, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, project, f.admin, "write"), core.NewID("enable"), SchedulingRequest{ExpectedRevision: &zero, AutomaticHosted: &on, AutomaticCLI: &on}); err != nil {
						t.Fatal(err)
					}
				}
				readers := map[string]TargetReader{"test-capability": &refreshTestReader{}}
				generate := f.e.generateAutomaticHosted
				if channel == "cli" {
					generate = f.e.generateAutomaticCLI
				}
				switch fault {
				case "policy", "policy-type":
					if fault == "policy" {
						admissionSQL(t, f, `UPDATE review_policy_config SET body_json='not-json' WHERE project_id=?`, attemptProject)
					} else {
						admissionSQL(t, f, `UPDATE review_policy_config SET revision='malformed' WHERE project_id=?`, attemptProject)
					}
				case "scheduling":
					admissionSQL(t, f, `UPDATE review_scheduling SET updated_at='malformed' WHERE project_id=?`, attemptProject)
				case "scheduling-type":
					admissionSQL(t, f, `UPDATE review_scheduling SET revision='malformed' WHERE project_id=?`, attemptProject)
				case "pull-scheduling-type":
					admissionSQL(t, f, `INSERT INTO review_pull_scheduling VALUES(?,2,'malformed',0,'','actor',?)`, attemptProject, stamp())
				case "pull-scheduling":
					admissionSQL(t, f, `INSERT INTO review_pull_scheduling VALUES(?,2,1,0,'unexpected reason','actor',?)`, attemptProject, stamp())
				case "front", "oversized-front":
					body := "not-json"
					if fault == "oversized-front" {
						body = strings.Repeat(" ", maxFrontPrefixBytes+1)
					}
					admissionSQL(t, f, `INSERT INTO review_front_frames VALUES(?,?,801,1,?,'',0,'',?)`, attemptProject, channel, stamp(), body)
				case "oversized-basis":
					// The basis reads only these three columns; a private synthetic table
					// keeps this bound test independent of route creation workflows.
					admissionSQL(t, f, `ALTER TABLE review_finding_routes RENAME TO saved_finding_routes`)
					admissionSQL(t, f, `CREATE TABLE review_finding_routes(project_id TEXT,id TEXT,revision INTEGER,state TEXT,disposition TEXT,has_fix INTEGER,source_pull_number INTEGER,target_pull_number INTEGER)`)
					admissionSQL(t, f, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<10001) INSERT INTO review_finding_routes SELECT ?,printf('route-%d',x),1,'active','accepted',0,2,4 FROM n`, attemptProject)
				case "retry-selection":
					if worked, err := generate(t.Context(), readers); err != nil || !worked {
						t.Fatal(worked, err)
					}
					selectionNext(t, f, &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
						return reviewprovider.Repository{}, context.DeadlineExceeded
					}})
					r := schedulingLatest(t, f)
					admissionSQL(t, f, `UPDATE review_selections SET updated_at='malformed' WHERE id=?`, r.ID)
					// Ensure the bad project is considered before the healthy one again.
					f.e.automaticCursor, f.e.automaticCLICursor = "", ""
				}
				worked, err := generate(t.Context(), readers)
				if err != nil || !worked {
					t.Fatal("project-local state stopped the healthy project", worked, err)
				}
				if got := attemptCount(t, f.store, `SELECT count(*) FROM review_automatic_selections a JOIN review_selections s ON s.id=a.selection_id WHERE a.project_id=? AND s.channel=?`, otherAttemptProject, channel); got != 1 {
					t.Fatal("healthy project not selected", got)
				}
				// A corrupt Hosted project must not prevent the independent CLI pass.
				if worked, err = f.e.coordinateAutomaticHosted(t.Context(), readers); err != nil || !worked {
					t.Fatal("project-local state stopped the other channel", worked, err)
				}
				otherChannel := "cli"
				if channel == "cli" {
					otherChannel = "hosted"
				}
				if got := attemptCount(t, f.store, `SELECT count(*) FROM review_automatic_selections a JOIN review_selections s ON s.id=a.selection_id WHERE s.channel=?`, otherChannel); got < 1 {
					t.Fatal("other channel was suppressed", got)
				}
			})
		}
	}
}

func TestAutomaticCoordinatorPreservesStorageFailuresAndCancellation(t *testing.T) {
	for _, fault := range []string{"front-table", "scheduling-table", "write-rollback", "cancelled", "cancelled-throttled"} {
		t.Run(fault, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			schedulingEnable(t, f, 0, true)
			switch fault {
			case "front-table":
				admissionSQL(t, f, `ALTER TABLE review_front_frames RENAME TO missing_front_frames`)
			case "scheduling-table":
				admissionSQL(t, f, `ALTER TABLE review_scheduling RENAME TO missing_scheduling`)
			case "write-rollback":
				admissionSQL(t, f, `CREATE TRIGGER fail_automatic_attribution BEFORE INSERT ON review_automatic_selections BEGIN SELECT RAISE(ABORT, 'synthetic attribution failure'); END`)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if strings.HasPrefix(fault, "cancelled") {
				cancel()
				if fault == "cancelled-throttled" {
					f.e.automaticNext = time.Now().Add(time.Minute)
				}
			}
			before := selectionCounts(t, f)
			worked, err := f.e.coordinateAutomaticHosted(ctx, map[string]TargetReader{"test-capability": &refreshTestReader{}})
			if err == nil || worked || automaticProjectRefused(err) {
				t.Fatal("storage/cancellation error was suppressed", worked, err)
			}
			if strings.HasPrefix(fault, "cancelled") && !errors.Is(err, context.Canceled) {
				t.Fatal("parent cancellation lost", err)
			}
			if before != selectionCounts(t, f) || attemptCount(t, f.store, `SELECT count(*) FROM review_automatic_selections`) != 0 {
				t.Fatal("failed transaction retained partial intent")
			}
		})
	}
	for _, err := range []error{core.Fail("storage_unavailable", "Shared storage failed", 503), core.Fail("storage_unavailable", "Shared storage failed", 500), core.Fail("overloaded", "Shared writer busy", 503), core.Fail("unrelated", "Unknown refusal", 403), context.Canceled, sql.ErrTxDone} {
		if automaticProjectRefused(err) {
			t.Fatal("public error code alone was classified project-local", err)
		}
	}
}

func TestAutomaticMalformedProjectDoesNotStopExplicitSelectionWorkers(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	schedulingEnable(t, f, 0, true)
	admissionSQL(t, f, `UPDATE review_scheduling SET updated_at='malformed' WHERE project_id=?`, attemptProject)
	raw, err := f.e.EnqueueRun(t.Context(), selectionScope(t, f, otherAttemptProject, f.admin, "write"), "healthy-explicit", RunRequest{Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err = json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: number, RepositoryID: 802, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	done, err := f.e.StartReviewRuns(ctx, map[string]TargetReader{"test-capability": reader})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error("selection workers failed", err)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if err := f.store.DB().QueryRow(`SELECT state FROM review_selections WHERE id=?`, receipt.ID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "selected" {
			return
		}
		if state == "held" || state == "empty" {
			t.Fatal("healthy explicit request not selected", state)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("healthy explicit selection did not finish")
}

func TestAutomaticSupervisorIsolatesSchedulingRefusals(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, fault := range []string{"automatic", "pull", "pending-prefix"} {
			t.Run(channel+"/"+fault, func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2, 4})
				f.configure(t, otherAttemptProject, "other-account", 0, false)
				var bad string
				if fault == "automatic" {
					if channel == "hosted" {
						schedulingEnable(t, f, 0, true)
						schedulingGenerate(t, f, true)
					} else {
						schedulingCLIEnable(t, f, 0, true)
						schedulingCLIGenerate(t, f, true)
					}
					selectionNext(t, f, &refreshTestReader{})
					bad = schedulingLatest(t, f).OperationID
					admissionSQL(t, f, `UPDATE review_scheduling SET updated_at='malformed' WHERE project_id=?`, attemptProject)
				} else {
					bad = f.enqueue(t, attemptProject, "bad", channel, 2, 1).ID
					if fault == "pending-prefix" {
						f.enqueue(t, attemptProject, "bad-later", channel, 4, 1)
					}
					admissionSQL(t, f, `INSERT INTO review_pull_scheduling VALUES(?,2,1,0,'invalid unpaused reason','actor',?)`, attemptProject, stamp())
				}
				healthy := f.enqueue(t, otherAttemptProject, "healthy", channel, 2, 1)
				driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
					if o.ID != healthy.ID {
						t.Error("wrong candidate reached provider", o.ID)
					}
					return Preparation{}, sourceHold("synthetic_stop")
				}}
				if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver, "other-account": driver}); err != nil {
					t.Fatal("local scheduling refusal stopped supervisor", err)
				}
				if driver.prepares.Load() != 1 || driver.calls.Load() != 0 {
					t.Fatal("healthy candidate not reached", driver.prepares.Load())
				}
				if got := cliWaitRead(t, f, bad); got.State != "queued" || got.AttemptID != "" {
					t.Fatal("corrupt candidate changed", got)
				}
				if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts WHERE project_id=?`, attemptProject) != 0 {
					t.Fatal("corrupt project reserved attempt")
				}
			})
		}
	}
}

func TestAutomaticSelectionSettlementRefusalIsAtomicAndLocal(t *testing.T) {
	for _, fault := range []string{"basis-cap", "front-after-child", "sql-after-child", "hold-write"} {
		t.Run(fault, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			schedulingEnable(t, f, 0, true)
			schedulingGenerate(t, f, true)
			r := schedulingLatest(t, f)
			admissionSQL(t, f, `UPDATE review_selections SET state='selecting',revision=2 WHERE id=?`, r.ID)
			r = selectionRead(t, f, r.ID)
			var c Configuration
			if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				var err error
				c, _, err = readConfiguration(ctx, tx, r.ProjectID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			choice, err := f.e.chooseRun(t.Context(), r, 801, c, &refreshTestReader{})
			if err != nil || choice.state != "selected" {
				t.Fatal("invalid selected fixture", choice, err)
			}
			if fault == "basis-cap" {
				admissionSQL(t, f, `ALTER TABLE review_finding_routes RENAME TO saved_finding_routes`)
				admissionSQL(t, f, `CREATE TABLE review_finding_routes(project_id TEXT,id TEXT,revision INTEGER,state TEXT,disposition TEXT,has_fix INTEGER,source_pull_number INTEGER,target_pull_number INTEGER)`)
				admissionSQL(t, f, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<10001) INSERT INTO review_finding_routes SELECT ?,printf('route-%d',x),1,'active','accepted',0,2,4 FROM n`, attemptProject)
				choice = runChoice{state: "held", reason: "source_fetch_timeout", sourceReason: "source_fetch_timeout"}
			} else {
				if fault == "sql-after-child" {
					admissionSQL(t, f, `CREATE TRIGGER fail_basis BEFORE UPDATE ON review_automatic_selections BEGIN SELECT RAISE(ABORT,'synthetic settlement write failure'); END`)
				} else {
					admissionSQL(t, f, `CREATE TRIGGER corrupt_basis AFTER INSERT ON review_operations BEGIN UPDATE review_front_frames SET prefix_json='not-json' WHERE project_id=NEW.project_id; END`)
					if fault == "hold-write" {
						admissionSQL(t, f, `CREATE TRIGGER fail_local_hold BEFORE UPDATE ON review_selections WHEN NEW.reason='automatic_scheduling_unavailable' BEGIN SELECT RAISE(ABORT,'synthetic local hold failure'); END`)
					}
				}
			}
			attribution := snapshotTable(t, f, "review_automatic_selections")
			frames := snapshotTable(t, f, "review_front_frames")
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			_, err = f.e.finishRunChoice(ctx, r, 801, nil, choice)
			fatal := fault == "sql-after-child" || fault == "hold-write"
			if fatal {
				if err == nil || automaticProjectRefused(err) {
					t.Fatal("real persistence failure suppressed", err)
				}
			} else if err != nil {
				t.Fatal("local basis refusal escaped or retried", err)
			}
			got := selectionRead(t, f, r.ID)
			if fatal {
				if got.State != "selecting" || got.Revision != 2 {
					t.Fatal("failed transaction changed selection", got)
				}
			} else if got.State != "held" || got.Reason != "automatic_scheduling_unavailable" || got.Revision != 3 || got.OperationID != "" {
				t.Fatal("local hold not retained", got)
			}
			if attribution != snapshotTable(t, f, "review_automatic_selections") || frames != snapshotTable(t, f, "review_front_frames") || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("settlement failure retained partial child, frame or basis")
			}
			if fatal {
				return
			}
			if fault == "basis-cap" {
				admissionSQL(t, f, `DELETE FROM review_finding_routes`)
			} else {
				admissionSQL(t, f, `DROP TRIGGER corrupt_basis`)
			}
			restarted, err := New(f.store, attemptCapabilities)
			if err != nil {
				t.Fatal(err)
			}
			f.e = restarted
			if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
				t.Fatal(err)
			}
			schedulingGenerate(t, f, false)
			if got := selectionRead(t, f, r.ID); got.State != "held" || got.Reason != "automatic_scheduling_unavailable" {
				t.Fatal("restart reopened refusal", got)
			}
			// The same worker can still select an independent explicit channel.
			healthy, _ := selectionEnqueue(t, f, "healthy-cli", "cli", nil)
			selectionNext(t, f, &refreshTestReader{})
			if got := selectionRead(t, f, healthy.ID); got.State != "selected" {
				t.Fatal("healthy channel did not progress", got)
			}
		})
	}
}

func TestAutomaticSelectionRetryNeverRetriesTypedRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	calls := 0
	refusal := automaticProjectStateFailure("overloaded", "Retained basis limit", 503)
	err := retryRunSelectionLocal(ctx, func() error { calls++; return refusal })
	if err != refusal || calls != 1 {
		t.Fatal("deterministic refusal entered transient retry", calls, err)
	}
}

func TestSelectionLocalRefusalKeepsExplicitAttribution(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		name := "explicit"
		if automatic {
			name = "automatic"
		}
		t.Run(name, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			var r Run
			if automatic {
				schedulingEnable(t, f, 0, true)
				schedulingGenerate(t, f, true)
				r = schedulingLatest(t, f)
			} else {
				receipt, _ := selectionEnqueue(t, f, "explicit-attribution", "hosted", nil)
				r = selectionRead(t, f, receipt.ID)
			}
			admissionSQL(t, f, `UPDATE review_selections SET state='selecting',revision=2 WHERE id=?`, r.ID)
			r = selectionRead(t, f, r.ID)
			admissionSQL(t, f, `UPDATE review_execution_config SET body_json='not-json' WHERE project_id=?`, r.ProjectID)
			before := snapshotTable(t, f, "review_selections")
			frames := snapshotTable(t, f, "review_front_frames")
			_, err := f.e.finishRunChoice(t.Context(), r, 801, nil, runChoice{state: "held", reason: "source_fetch_timeout", sourceReason: "source_fetch_timeout"})
			if automatic {
				if err != nil {
					t.Fatal("automatic refusal escaped", err)
				}
				var state, reason string
				if err := f.store.DB().QueryRow(`SELECT state,reason FROM review_selections WHERE id=?`, r.ID).Scan(&state, &reason); err != nil || state != "held" || reason != "automatic_scheduling_unavailable" {
					t.Fatal("automatic hold missing", state, reason, err)
				}
			} else if !automaticProjectRefused(err) || before != snapshotTable(t, f, "review_selections") {
				t.Fatal("explicit request became an automatic hold instead of preserving its error path", err)
			}
			if frames != snapshotTable(t, f, "review_front_frames") || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
				t.Fatal("refusal retained source or child writes")
			}
		})
	}
}

func TestAutomaticParentReadersIsolateRetainedCorruption(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, fault := range []string{"proof", "round-body", "round-type", "route", "adoption"} {
			t.Run(channel+"/"+fault, func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2})
				f.configure(t, otherAttemptProject, "test-capability", 0, false)
				now, sha := stamp(), strings.Repeat("a", 40)
				prefix := []frontPull{{Number: 2, State: "open", HeadSHA: sha, BaseSHA: sha, BaseRef: "four", ParentPullNumber: 4, ParentSHA: sha, ObservedAt: now}, {Number: 4, State: "open", HeadSHA: sha, BaseSHA: sha, BaseRef: "main", ObservedAt: now}}
				raw, err := json.Marshal(prefix)
				if err != nil {
					t.Fatal(err)
				}
				admissionSQL(t, f, `INSERT INTO review_front_frames VALUES(?,?,801,1,?,?,1,'',?)`, attemptProject, channel, now, now, string(raw))
				switch fault {
				case "proof":
					// Isolate the bounded proof reader from unrelated evidence construction.
					admissionSQL(t, f, `ALTER TABLE review_comparison_proofs RENAME TO saved_comparison_proofs`)
					admissionSQL(t, f, `CREATE TABLE review_comparison_proofs(project_id TEXT,pull_number INTEGER,channel TEXT,state TEXT,repository_id INTEGER)`)
					admissionSQL(t, f, `INSERT INTO review_comparison_proofs VALUES(?,4,?,'dirty',801)`, attemptProject, channel)
				case "round-body", "round-type":
					roundGrant(t, f, 4, channel, 0, 1, 2, "parent-grant")
					if fault == "round-body" {
						admissionSQL(t, f, `UPDATE review_round_changes SET body_json='not-json' WHERE project_id=?`, attemptProject)
					} else {
						admissionSQL(t, f, `UPDATE review_round_controls SET updated_at=123 WHERE project_id=?`, attemptProject)
					}
				case "adoption":
					integrationImportHold(t, f, 4)
					admissionSQL(t, f, `DROP TRIGGER review_adoption_originals_immutable`)
					admissionSQL(t, f, `UPDATE review_adoption_obligations SET original_json='not-json' WHERE project_id=?`, attemptProject)
				case "route":
					admissionSQL(t, f, `INSERT INTO review_route_policy_invalidations VALUES(?,4,'broken-route')`, attemptProject)
				}
				err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					_, err := automaticDependenciesChannel(ctx, tx, attemptProject, 2, channel)
					return err
				})
				if !automaticProjectRefused(err) {
					t.Fatal("retained corruption not classified", err)
				}
				for _, project := range []string{attemptProject, otherAttemptProject} {
					zero := int64(0)
					hosted, cli := channel == "hosted", channel == "cli"
					if _, err = f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, project, f.admin, "write"), core.NewID("enable"), SchedulingRequest{ExpectedRevision: &zero, AutomaticHosted: &hosted, AutomaticCLI: &cli}); err != nil {
						t.Fatal(err)
					}
				}
				generate := f.e.generateAutomaticHosted
				if channel == "cli" {
					generate = f.e.generateAutomaticCLI
				}
				worked, err := generate(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
				if err != nil || !worked {
					t.Fatal("corruption stopped healthy project", worked, err)
				}
				if n := attemptCount(t, f.store, `SELECT count(*) FROM review_automatic_selections WHERE project_id=?`, otherAttemptProject); n != 1 {
					t.Fatal("healthy project not selected", n)
				}
			})
		}
	}
}

func TestAutomaticSharedReadersPreserveSQLFailures(t *testing.T) {
	for _, table := range []string{"review_policy_config", "review_comparison_proofs", "review_round_controls", "review_route_policy_invalidations", "review_adoption_obligations"} {
		t.Run(table, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			admissionSQL(t, f, `ALTER TABLE `+table+` RENAME TO unavailable_retained_table`)
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				var err error
				switch table {
				case "review_policy_config":
					_, err = readPolicy(ctx, tx, attemptProject)
				case "review_comparison_proofs":
					_, err = channelProgress(ctx, tx, attemptProject, 801, 4, "hosted")
				case "review_round_controls":
					_, err = readRoundRecord(ctx, tx, attemptProject, 4, "hosted")
				case "review_route_policy_invalidations":
					_, err = pendingIncomingRoutes(ctx, tx, attemptProject, 4)
				case "review_adoption_obligations":
					_, err = sourceObligationCount(ctx, tx, attemptProject, 4)
				}
				return err
			})
			if err == nil || automaticProjectRefused(err) {
				t.Fatal("SQL failure hidden as project-local refusal", err)
			}
		})
	}
}

func TestSelectionWorkerIsolatesRetainedRefusalsAtEveryStage(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, automatic := range []bool{false, true} {
			for _, stage := range []string{"claim", "choose", "finish"} {
				name := channel + "/explicit/" + stage
				if automatic {
					name = channel + "/automatic/" + stage
				}
				t.Run(name, func(t *testing.T) {
					f := schedulingFixtureWithQueue(t, []int64{2})
					f.configure(t, otherAttemptProject, "other-account", 0, false)
					var bad Run
					if automatic {
						if channel == "hosted" {
							schedulingEnable(t, f, 0, true)
							schedulingGenerate(t, f, true)
						} else {
							schedulingCLIEnable(t, f, 0, true)
							schedulingCLIGenerate(t, f, true)
						}
						bad = schedulingLatest(t, f)
					} else {
						r, _ := selectionEnqueue(t, f, "bad", channel, nil)
						bad = selectionRead(t, f, r.ID)
					}
					switch stage {
					case "claim":
						if automatic {
							admissionSQL(t, f, `UPDATE review_scheduling SET updated_at='malformed' WHERE project_id=?`, attemptProject)
						} else {
							admissionSQL(t, f, `UPDATE review_policy_config SET body_json='not-json' WHERE project_id=?`, attemptProject)
						}
					case "choose":
						admissionSQL(t, f, `INSERT INTO review_pull_scheduling VALUES(?,2,1,0,'invalid unpaused reason','actor',?)`, attemptProject, stamp())
					case "finish":
						admissionSQL(t, f, `CREATE TRIGGER corrupt_selection_finish AFTER INSERT ON review_front_frames BEGIN INSERT INTO review_pull_scheduling VALUES(NEW.project_id,2,1,0,'invalid unpaused reason','actor',NEW.checked_at); END`)
					}
					beforeFrames := snapshotTable(t, f, "review_front_frames")
					worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
					if err != nil || !worked {
						t.Fatal("local failure stopped worker", worked, err)
					}
					got := selectionRead(t, f, bad.ID)
					reason := "local_read_unavailable"
					if automatic {
						reason = "automatic_scheduling_unavailable"
					}
					if got.State != "held" || got.Reason != reason || got.OperationID != "" {
						t.Fatal("incorrect refusal attribution", got)
					}
					if beforeFrames != snapshotTable(t, f, "review_front_frames") || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
						t.Fatal("partial source or child retained")
					}
					if stage == "finish" {
						admissionSQL(t, f, `DROP TRIGGER corrupt_selection_finish`)
					}
					raw, err := f.e.EnqueueRun(t.Context(), selectionScope(t, f, otherAttemptProject, f.admin, "write"), "healthy", RunRequest{Channel: channel})
					if err != nil {
						t.Fatal(err)
					}
					var healthy Receipt
					if err = json.Unmarshal(raw, &healthy); err != nil {
						t.Fatal(err)
					}
					readers := map[string]TargetReader{"test-capability": &refreshTestReader{}, "other-account": &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
						return reviewprovider.Pull{Number: n, RepositoryID: 802, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
					}}}
					if worked, err = f.e.runNext(t.Context(), readers); err != nil || !worked {
						t.Fatal("healthy project stopped", worked, err)
					}
					var state string
					if err = f.store.DB().QueryRow(`SELECT state FROM review_selections WHERE id=?`, healthy.ID).Scan(&state); err != nil || state != "selected" {
						t.Fatal("healthy project not selected", state, err)
					}
					if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
						t.Fatal(err)
					}
					if got = selectionRead(t, f, bad.ID); got.State != "held" {
						t.Fatal("restart reopened poison selection", got)
					}
				})
			}
		}
	}
}

func TestSelectionWorkerLocalHoldPreservesPersistenceFailuresAndRaces(t *testing.T) {
	for _, fault := range []string{"hold-write", "concurrent-transition", "canceled"} {
		t.Run(fault, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			receipt, _ := selectionEnqueue(t, f, "local-hold", "cli", nil)
			r := selectionRead(t, f, receipt.ID)
			switch fault {
			case "hold-write":
				admissionSQL(t, f, `CREATE TRIGGER fail_local_hold BEFORE UPDATE ON review_selections WHEN NEW.reason='local_read_unavailable' BEGIN SELECT RAISE(ABORT,'synthetic persistence failure'); END`)
			case "concurrent-transition":
				admissionSQL(t, f, `UPDATE review_selections SET state='held',reason='configuration_changed',revision=revision+1 WHERE id=?`, r.ID)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if fault == "canceled" {
				cancel()
			}
			before := snapshotTable(t, f, "review_selections")
			worked, err := f.e.holdRunLocalRefusal(ctx, r)
			if worked {
				t.Fatal("failed or stale hold reported progress")
			}
			if fault == "concurrent-transition" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || automaticProjectRefused(err) {
				t.Fatal("real failure hidden", err)
			}
			if before != snapshotTable(t, f, "review_selections") {
				t.Fatal("failed or stale hold changed selection")
			}
		})
	}
}

func TestStartupIsolatesDamagedFrontCacheButSnapshotsRejectIt(t *testing.T) {
	for _, fault := range []string{"json", "oversized", "timestamp", "type"} {
		t.Run(fault, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			f.configure(t, otherAttemptProject, "test-capability", 0, false)
			for _, project := range []string{attemptProject, otherAttemptProject} {
				zero := int64(0)
				on := true
				if _, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, project, f.admin, "write"), core.NewID("enable"), SchedulingRequest{ExpectedRevision: &zero, AutomaticHosted: &on, AutomaticCLI: &on}); err != nil {
					t.Fatal(err)
				}
			}
			now := stamp()
			admissionSQL(t, f, `INSERT INTO review_front_frames VALUES(?,'hosted',801,1,?,'',0,'','[]')`, attemptProject, now)
			switch fault {
			case "json":
				admissionSQL(t, f, `UPDATE review_front_frames SET prefix_json='not-json'`)
			case "oversized":
				admissionSQL(t, f, `UPDATE review_front_frames SET prefix_json=?`, strings.Repeat(" ", maxFrontPrefixBytes+1))
			case "timestamp":
				admissionSQL(t, f, `UPDATE review_front_frames SET checked_at='not-time'`)
			case "type":
				admissionSQL(t, f, `UPDATE review_front_frames SET repository_id='bad'`)
			}
			before := snapshotTable(t, f, "review_front_frames")
			if err := EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal("one cache row blocked startup", err)
			}
			if before != snapshotTable(t, f, "review_front_frames") {
				t.Fatal("startup rewrote retained cache")
			}
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error { return ValidateFrontSnapshot(ctx, tx) })
			if err == nil {
				t.Fatal("backup validation accepted damaged cache")
			}
			worked, err := f.e.generateAutomaticHosted(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
			if err != nil || !worked {
				t.Fatal("healthy project blocked after restart", worked, err)
			}
			if n := attemptCount(t, f.store, `SELECT count(*) FROM review_automatic_selections WHERE project_id=?`, otherAttemptProject); n != 1 {
				t.Fatal("healthy project not selected", n)
			}
		})
	}
}

func TestStartupStillRejectsSharedFrontSchemaDamage(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	admissionSQL(t, f, `CREATE INDEX unexpected_front_index ON review_front_frames(repository_id)`)
	if err := EnsureFrontSchema(t.Context(), f.store); err == nil {
		t.Fatal("shared physical schema damage accepted")
	}
}
