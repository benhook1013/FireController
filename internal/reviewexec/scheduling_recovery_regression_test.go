package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestSupervisorIsolatesPullSchedulingRefusals(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, fault := range []string{"pull", "pending-prefix"} {
			t.Run(channel+"/"+fault, func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2, 4})
				f.configure(t, otherAttemptProject, "other-account", 0, false)
				bad := f.enqueue(t, attemptProject, "bad", channel, 2, 1)
				if fault == "pending-prefix" {
					f.enqueue(t, attemptProject, "bad-later", channel, 4, 1)
				}
				admissionSQL(t, f, `INSERT INTO review_pull_scheduling VALUES(?,2,1,0,'invalid unpaused reason','actor',?)`, attemptProject, stamp())
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
				if got := cliWaitRead(t, f, bad.ID); got.State != "queued" || got.AttemptID != "" {
					t.Fatal("corrupt candidate changed", got)
				}
				if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts WHERE project_id=?`, attemptProject) != 0 {
					t.Fatal("corrupt project reserved attempt")
				}
			})
		}
	}
}

func TestSelectionLocalRefusalKeepsExplicitAttribution(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	receipt, _ := selectionEnqueue(t, f, "explicit-attribution", "hosted", nil)
	admissionSQL(t, f, `UPDATE review_selections SET state='selecting',revision=2 WHERE id=?`, receipt.ID)
	r := selectionRead(t, f, receipt.ID)
	admissionSQL(t, f, `UPDATE review_execution_config SET body_json='not-json' WHERE project_id=?`, r.ProjectID)
	before := snapshotTable(t, f, "review_selections")
	frames := snapshotTable(t, f, "review_front_frames")
	_, err := f.e.finishRunChoice(t.Context(), r, 801, nil, runChoice{state: "held", reason: "source_fetch_timeout", sourceReason: "source_fetch_timeout"})
	if !automaticProjectRefused(err) || before != snapshotTable(t, f, "review_selections") {
		t.Fatal("explicit request became an automatic hold instead of preserving its error path", err)
	}
	if frames != snapshotTable(t, f, "review_front_frames") || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("refusal retained source or child writes")
	}
}

func TestRetainedReadersPreserveSQLFailures(t *testing.T) {
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

func assertIndependentSelectionProgress(t *testing.T, f *attemptFixture, channel string) {
	t.Helper()
	raw, err := f.e.EnqueueRun(t.Context(), selectionScope(t, f, otherAttemptProject, f.admin, "write"), "healthy", RunRequest{Channel: channel})
	if err != nil {
		t.Fatal(err)
	}
	var healthy Receipt
	if err = json.Unmarshal(raw, &healthy); err != nil {
		t.Fatal(err)
	}
	reader := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: n, RepositoryID: 802, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	if worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}, "other-account": reader}); err != nil || !worked {
		t.Fatal("healthy project stopped", worked, err)
	}
	var state string
	if err = f.store.DB().QueryRow(`SELECT state FROM review_selections WHERE id=?`, healthy.ID).Scan(&state); err != nil || state != "selected" {
		t.Fatal("healthy project not selected", state, err)
	}
}

func TestSelectionWorkerIsolatesRetainedRefusalsAtEveryStage(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, stage := range []string{"claim", "choose", "finish"} {
			t.Run(channel+"/explicit/"+stage, func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2})
				f.configure(t, otherAttemptProject, "other-account", 0, false)
				bad, _ := selectionEnqueue(t, f, "bad", channel, nil)
				switch stage {
				case "claim":
					admissionSQL(t, f, `UPDATE review_policy_config SET body_json='not-json' WHERE project_id=?`, attemptProject)
				case "choose":
					admissionSQL(t, f, `INSERT INTO review_pull_scheduling VALUES(?,2,1,0,'invalid unpaused reason','actor',?)`, attemptProject, stamp())
				case "finish":
					admissionSQL(t, f, `CREATE TRIGGER corrupt_selection_finish AFTER INSERT ON review_front_frames WHEN NEW.project_id='exec-project-one' BEGIN INSERT INTO review_pull_scheduling VALUES(NEW.project_id,2,1,0,'invalid unpaused reason','actor',NEW.checked_at); END`)
				}
				beforeFrames := snapshotTable(t, f, "review_front_frames")
				worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
				if err != nil || !worked {
					t.Fatal("local failure stopped worker", worked, err)
				}
				got := selectionRead(t, f, bad.ID)
				if got.State != "held" || got.Reason != "local_read_unavailable" || got.OperationID != "" {
					t.Fatal("incorrect refusal attribution", got)
				}
				if beforeFrames != snapshotTable(t, f, "review_front_frames") || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
					t.Fatal("partial source or child retained")
				}
				assertIndependentSelectionProgress(t, f, channel)
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
			f.configure(t, otherAttemptProject, "other-account", 0, false)
			admissionSQL(t, f, `INSERT INTO review_front_frames VALUES(?,'hosted',801,1,?,'',0,'','[]')`, attemptProject, stamp())
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
			assertIndependentSelectionProgress(t, f, "hosted")
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

func TestDamagedPendingSelectionQuarantinesWithoutStoppingOtherProjects(t *testing.T) {
	for _, state := range []string{"queued", "selecting"} {
		for _, fault := range []string{"time", "revision-type", "revision-limit", "fingerprint-type", "write-failure"} {
			t.Run(state+"/"+fault, func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2})
				f.configure(t, otherAttemptProject, "other-account", 0, false)
				bad, _ := selectionEnqueue(t, f, "damaged", "cli", nil)
				admissionSQL(t, f, `UPDATE review_selections SET state=? WHERE id=?`, state, bad.ID)
				switch fault {
				case "revision-type":
					admissionSQL(t, f, `UPDATE review_selections SET revision='broken' WHERE id=?`, bad.ID)
				case "revision-limit":
					admissionSQL(t, f, `UPDATE review_selections SET revision=9223372036854775807 WHERE id=?`, bad.ID)
				case "fingerprint-type":
					admissionSQL(t, f, `UPDATE review_selections SET capability_fingerprint=CAST(capability_fingerprint AS BLOB) WHERE id=?`, bad.ID)
				default:
					admissionSQL(t, f, `UPDATE review_selections SET updated_at='broken' WHERE id=?`, bad.ID)
				}
				if fault == "write-failure" {
					admissionSQL(t, f, `CREATE TRIGGER refuse_quarantine BEFORE UPDATE ON review_selections WHEN NEW.state='held' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`)
				}
				before := snapshotTable(t, f, "review_selections")
				var err error
				if state == "selecting" {
					err = f.e.RecoverReviewRuns(t.Context())
				} else {
					_, err = f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
				}
				if fault == "write-failure" {
					if err == nil || automaticProjectRefused(err) || before != snapshotTable(t, f, "review_selections") {
						t.Fatal("persistence failure suppressed or partially committed", err)
					}
					return
				}
				if err != nil {
					t.Fatal("damaged row stopped workers", err)
				}
				var got, reason string
				if err = f.store.DB().QueryRow(`SELECT state,reason FROM review_selections WHERE id=?`, bad.ID).Scan(&got, &reason); err != nil || got != "held" || reason != "local_read_unavailable" {
					t.Fatal(got, reason, err)
				}
				if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
					t.Fatal("damaged row created child")
				}
				if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
					t.Fatal("restart failed", err)
				}
				assertIndependentSelectionProgress(t, f, "cli")
			})
		}
	}
}
