package reviewexec

import (
	"context"
	"database/sql"
	"github.com/benhook1013/FireController/internal/core"
	"testing"
	"time"
)

func continuityFixture(t *testing.T, channel string, selected bool) (*attemptFixture, Run) {
	t.Helper()
	f := schedulingFixtureWithQueue(t, []int64{2})
	if channel == "hosted" {
		schedulingEnable(t, f, 0, true)
		schedulingGenerate(t, f, true)
	} else {
		schedulingCLIEnable(t, f, 0, true)
		schedulingCLIGenerate(t, f, true)
	}
	if selected {
		selectionNext(t, f, &refreshTestReader{})
	}
	r := schedulingLatest(t, f)
	admissionSQL(t, f, `INSERT INTO workers VALUES(?,'replacement-worker','Replacement manager')`, attemptProject)
	continuityConfigure(t, f, 1, true, true)
	return f, r
}
func continuityConfigure(t *testing.T, f *attemptFixture, revision int64, hosted, cli bool) {
	t.Helper()
	scope := selectionScope(t, f, attemptProject, f.admin, "write")
	scope.WorkerID = "replacement-worker"
	_, err := f.e.ConfigureScheduling(t.Context(), scope, core.NewID("continuity"), SchedulingRequest{ExpectedRevision: &revision, AutomaticHosted: &hosted, AutomaticCLI: &cli})
	if err != nil {
		t.Fatal(err)
	}
}
func continuityGuard(t *testing.T, f *attemptFixture, r Run) error {
	t.Helper()
	return f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error { return f.e.requireAutomaticRun(ctx, tx, r) })
}
func TestRetainedAutomaticAuthorizationSurvivesUnrelatedLaneToggle(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		t.Run(channel, func(t *testing.T) {
			f, r := continuityFixture(t, channel, true)
			if channel == "hosted" {
				// Pre-CLI scheduling events recorded only the Hosted choice.
				admissionSQL(t, f, `UPDATE events SET payload_json='{"automatic_hosted":true}' WHERE event_type='review.scheduling_updated' AND revision=1`)
			}
			if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error { return ValidateSchedulingSnapshot(ctx, tx) }); err != nil {
				t.Fatal(err)
			}
			attribution := snapshotTable(t, f, "review_automatic_selections")
			selections := snapshotTable(t, f, "review_selections")
			if err := continuityGuard(t, f, r); err != nil {
				t.Fatal("valid continuously enabled intent denied", err)
			}
			a, err := f.e.StartAttempt(t.Context(), attemptProject, r.OperationID)
			if err != nil || a.ID == "" || a.OperationID != r.OperationID {
				t.Fatal("original operation did not resume", a, err)
			}
			if snapshotTable(t, f, "review_automatic_selections") != attribution || snapshotTable(t, f, "review_selections") != selections {
				t.Fatal("continuity rewrote ownership or selected a replacement")
			}
			op := cliWaitRead(t, f, r.OperationID)
			if op.ActorID != r.ActorID || op.WorkerID != r.WorkerID || op.Channel != channel {
				t.Fatal("original identity changed", op)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
				t.Fatal("duplicate operation")
			}
		})
	}
}

func TestRetainedAutomaticAuthorizationRequiresCompleteAuthorityAndHistory(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, fault := range []string{"lane-disabled", "lane-off-on", "missing-initial", "missing-middle", "missing-current", "duplicate-revision", "original-actor", "current-actor", "malformed-payload", "duplicate-channel", "fractional-revision", "null-lane", "revoked-original", "lost-manage", "retired-original", "revoked-current", "mismatched-child", "unselected"} {
			t.Run(channel+"/"+fault, func(t *testing.T) {
				f, r := continuityFixture(t, channel, fault != "unselected")
				switch fault {
				case "lane-disabled", "lane-off-on":
					hosted, cli := true, true
					if channel == "hosted" {
						hosted = false
					} else {
						cli = false
					}
					continuityConfigure(t, f, 2, hosted, cli)
					if fault == "lane-off-on" {
						continuityConfigure(t, f, 3, true, true)
					}
				case "missing-initial":
					admissionSQL(t, f, `DELETE FROM events WHERE event_type='review.scheduling_updated' AND revision=1`)
				case "missing-current":
					admissionSQL(t, f, `DELETE FROM events WHERE event_type='review.scheduling_updated' AND revision=2`)
				case "missing-middle":
					continuityConfigure(t, f, 2, true, true)
					admissionSQL(t, f, `DELETE FROM events WHERE event_type='review.scheduling_updated' AND revision=2`)
				case "duplicate-revision":
					admissionSQL(t, f, `INSERT INTO events(project_id,event_type,resource_id,revision,actor_id,payload_json,created_at) SELECT project_id,event_type,resource_id,revision,actor_id,payload_json,created_at FROM events WHERE event_type='review.scheduling_updated' AND revision=1`)
				case "original-actor":
					admissionSQL(t, f, `UPDATE events SET actor_id='forged-actor' WHERE event_type='review.scheduling_updated' AND revision=1`)
				case "current-actor":
					admissionSQL(t, f, `UPDATE events SET actor_id='forged-actor' WHERE event_type='review.scheduling_updated' AND revision=2`)
				case "malformed-payload":
					admissionSQL(t, f, `UPDATE events SET payload_json='{}' WHERE event_type='review.scheduling_updated' AND revision=2`)
				case "duplicate-channel":
					admissionSQL(t, f, `UPDATE events SET payload_json='{"automatic_hosted":false,"automatic_hosted":true,"automatic_cli":false,"automatic_cli":true}' WHERE event_type='review.scheduling_updated' AND revision=2`)
				case "fractional-revision":
					admissionSQL(t, f, `UPDATE events SET revision=1.5 WHERE event_type='review.scheduling_updated' AND revision=2`)
				case "null-lane":
					admissionSQL(t, f, `UPDATE events SET payload_json='{"automatic_hosted":null,"automatic_cli":null}' WHERE event_type='review.scheduling_updated' AND revision=2`)
				case "revoked-original":
					admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
				case "lost-manage":
					admissionSQL(t, f, `UPDATE grants SET can_manage=0 WHERE credential_id='exec-writer-cred'`)
				case "retired-original":
					admissionSQL(t, f, `INSERT INTO worker_retirements VALUES(?,'selection-worker',?,'synthetic-actor')`, attemptProject, stamp())
				case "revoked-current":
					admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id=(SELECT credential_id FROM review_scheduling WHERE project_id=?)`, attemptProject)
				case "mismatched-child":
					admissionSQL(t, f, `UPDATE review_operations SET worker_id='replacement-worker' WHERE id=?`, r.OperationID)
				}
				if fault == "unselected" && (r.State != "queued" || r.OperationID != "") {
					t.Fatal("unselected fixture unexpectedly has a child", r)
				}
				attemptCode(t, continuityGuard(t, f, r), "automation_authorization_changed")
				if r.OperationID != "" && fault != "mismatched-child" {
					a, err := f.e.StartAttempt(t.Context(), attemptProject, r.OperationID)
					if err != nil || a.ID != "" {
						t.Fatal("invalid continuity dispatched", a, err)
					}
				}
			})
		}
	}
}

func TestRetainedAutomaticAuthorizationKeepsQuotaWaitIdentity(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingCLIEnable(t, f, 0, true)
	schedulingCLIGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	r := schedulingLatest(t, f)
	a := f.start(t, attemptProject, Receipt{ID: r.OperationID})
	in := CLIAllowanceObservation{Anchor: attemptAnchor(attemptProject, 2), ObservedAt: stamp(), Remaining: 0, Limit: 3, FullCapacityInMS: 86400000, RollingWindowMS: 86400000, ChangedFiles: 2}
	if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	original := cliWaitRead(t, f, r.OperationID)
	due := cliWaitDue(t, original)
	admissionSQL(t, f, `INSERT INTO workers VALUES(?,'replacement-worker','Replacement manager')`, attemptProject)
	continuityConfigure(t, f, 1, true, true)
	wait := snapshotTable(t, f, "review_operation_waits")
	if err := continuityGuard(t, f, r); err != nil {
		t.Fatal(err)
	}
	if wait != snapshotTable(t, f, "review_operation_waits") {
		t.Fatal("continuity changed deadline or baseline")
	}
	f.e.schedulingNow = func() time.Time { return due.Add(time.Second) }
	next, err := f.e.StartAttempt(t.Context(), attemptProject, r.OperationID)
	if err != nil || next.ID == "" || next.OperationID != r.OperationID {
		t.Fatal("same quota operation did not resume", next, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
		t.Fatal("quota intent replaced")
	}
}

func TestRetainedAutomaticAuthorizationKeepsHostedCooldownThroughRestart(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	r := schedulingLatest(t, f)
	due := priorityCooldown(t, f, Receipt{ID: r.OperationID}, 2, time.Minute)
	before := snapshotTable(t, f, "review_operation_waits")
	admissionSQL(t, f, `INSERT INTO workers VALUES(?,'replacement-worker','Replacement manager')`, attemptProject)
	continuityConfigure(t, f, 1, true, true)
	restarted, err := New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	f.e = restarted
	if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
		t.Fatal(err)
	}
	if before != snapshotTable(t, f, "review_operation_waits") {
		t.Fatal("ownership update/restart changed retained timer")
	}
	f.e.schedulingNow = func() time.Time { return due.Add(time.Second) }
	next, err := f.e.StartAttempt(t.Context(), attemptProject, r.OperationID)
	if err != nil || next.ID == "" || next.OperationID != r.OperationID {
		t.Fatal("same Hosted operation did not resume", next, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_automatic_selections`) != 1 {
		t.Fatal("Hosted request replaced")
	}
}

func TestRetainedAutomaticAuthorizationDiagnosticIsReadOnly(t *testing.T) {
	f, r := continuityFixture(t, "hosted", true)
	continuityConfigure(t, f, 2, false, true)
	continuityConfigure(t, f, 3, true, true)
	before := selectionCounts(t, f) + snapshotTable(t, f, "review_automatic_selections")
	w := statusHTTP(t, f, r.OperationID, "", "wait=0")
	if w.Code != 200 || w.Header().Get(queuedDiagnosticHeader) != "automation_authorization_changed" {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
	if before != selectionCounts(t, f)+snapshotTable(t, f, "review_automatic_selections") {
		t.Fatal("diagnostic changed retained intent")
	}
}

func TestAutomaticSameAuthorizerRevisionContinuity(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, state := range []string{"queued", "selecting", "selected"} {
			for _, change := range []string{"unchanged", "unrelated-channel", "same-settings", "off-on", "missing-history"} {
				t.Run(channel+"/"+state+"/"+change, func(t *testing.T) {
					f := schedulingFixtureWithQueue(t, []int64{2})
					scope := selectionScope(t, f, attemptProject, f.writer, "write")
					configure := func(revision int64, hosted, cli bool) {
						t.Helper()
						if _, err := f.e.ConfigureScheduling(t.Context(), scope, core.NewID("same-authorizer"), SchedulingRequest{ExpectedRevision: &revision, AutomaticHosted: &hosted, AutomaticCLI: &cli}); err != nil {
							t.Fatal(err)
						}
					}
					configure(0, true, true)
					generate := f.e.generateAutomaticHosted
					if channel == "cli" {
						generate = f.e.generateAutomaticCLI
					}
					if worked, err := generate(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}}); err != nil || !worked {
						t.Fatal(worked, err)
					}
					if state == "selected" {
						selectionNext(t, f, &refreshTestReader{})
					} else if state == "selecting" {
						admissionSQL(t, f, `UPDATE review_selections SET state='selecting',revision=2 WHERE project_id=?`, attemptProject)
					}
					r := schedulingLatest(t, f)
					switch change {
					case "unrelated-channel":
						configure(1, channel == "hosted", channel == "cli")
					case "same-settings", "missing-history":
						configure(1, true, true)
						if change == "missing-history" {
							admissionSQL(t, f, `DELETE FROM events WHERE event_type='review.scheduling_updated' AND revision=1`)
						}
					case "off-on":
						configure(1, channel != "hosted", channel != "cli")
						attemptCode(t, continuityGuard(t, f, r), "automation_authorization_changed")
						configure(2, true, true)
					}
					before := selectionCounts(t, f) + snapshotTable(t, f, "review_automatic_selections")
					err := continuityGuard(t, f, r)
					denied := change == "off-on" || change == "missing-history"
					if denied {
						attemptCode(t, err, "automation_authorization_changed")
					} else if err != nil {
						t.Fatal("continuous same-authorizer run refused", err)
					}
					if after := selectionCounts(t, f) + snapshotTable(t, f, "review_automatic_selections"); after != before {
						t.Fatal("authorization guard mutated retained intent")
					}
					if state == "selected" {
						a, err := f.e.StartAttempt(t.Context(), attemptProject, r.OperationID)
						if err != nil || (a.ID == "") != denied {
							t.Fatal("dispatch did not honor continuity", a, err)
						}
					}
				})
			}
		}
	}
}
