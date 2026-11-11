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
		for _, fault := range []string{"scheduling", "scheduling-type", "pull-scheduling", "pull-scheduling-type", "front", "oversized-front", "oversized-basis", "retry-selection"} {
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
