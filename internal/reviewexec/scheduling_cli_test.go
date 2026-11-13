package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func schedulingCLIEnable(t *testing.T, f *attemptFixture, revision int64, on bool) {
	t.Helper()
	hosted := false
	_, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), core.NewID("key"), SchedulingRequest{ExpectedRevision: &revision, AutomaticHosted: &hosted, AutomaticCLI: &on})
	if err != nil {
		t.Fatal(err)
	}
}
func schedulingCLIGenerate(t *testing.T, f *attemptFixture, want bool) {
	t.Helper()
	got, err := f.e.generateAutomaticCLI(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
	if err != nil || got != want {
		t.Fatalf("automatic CLI generated=%v want=%v err=%v", got, want, err)
	}
}
func TestSchedulingCLIAutomaticDefaultOffCancellationAndRestart(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingCLIGenerate(t, f, false)
	schedulingCLIEnable(t, f, 0, true)
	schedulingCLIGenerate(t, f, true)
	r := schedulingLatest(t, f)
	if r.Channel != "cli" || r.ActorID != "exec-writer-principal" || r.WorkerID != "selection-worker" {
		t.Fatal(r)
	}
	selectionNext(t, f, &refreshTestReader{})
	r = schedulingLatest(t, f)
	if r.State != "selected" {
		t.Fatal(r)
	}
	_, err := f.e.ControlOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "cancel-auto", r.OperationID, OperationControl{ExpectedRevision: 1, Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	counts := selectionCounts(t, f)
	schedulingCLIGenerate(t, f, false)
	restarted, err := New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	f.e = restarted
	if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
		t.Fatal(err)
	}
	schedulingCLIGenerate(t, f, false)
	if selectionCounts(t, f) != counts {
		t.Fatal("cancelled intent regenerated")
	}
	// A deliberate scheduling revision authorizes a new semantic intent, not a tick.
	schedulingCLIEnable(t, f, 1, true)
	schedulingCLIGenerate(t, f, true)
}
func TestSchedulingCLIAutomaticAuthorizerWithdrawalAndRestoreHold(t *testing.T) {
	for _, kind := range []string{"management", "revoked", "retired", "disabled", "restore"} {
		t.Run(kind, func(t *testing.T) {
			f := schedulingFixture(t)
			schedulingCLIEnable(t, f, 0, true)
			switch kind {
			case "management":
				admissionSQL(t, f, `UPDATE grants SET can_manage=0 WHERE credential_id='exec-writer-cred'`)
			case "revoked":
				admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
			case "retired":
				admissionSQL(t, f, `INSERT INTO worker_retirements VALUES(?,'selection-worker',?,'synthetic-actor')`, attemptProject, stamp())
			case "disabled":
				schedulingCLIEnable(t, f, 1, false)
			case "restore":
				admissionSQL(t, f, `INSERT INTO metadata VALUES('restore:provider_state','held')`)
			}
			schedulingCLIGenerate(t, f, false)
			if attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 0 {
				t.Fatal("withdrawn authority generated selection")
			}
		})
	}
	// Authorization is also checked after a selection has created its child.
	f := schedulingFixture(t)
	schedulingCLIEnable(t, f, 0, true)
	schedulingCLIGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	r := schedulingLatest(t, f)
	admissionSQL(t, f, `UPDATE grants SET can_manage=0 WHERE credential_id='exec-writer-cred'`)
	a, err := f.e.StartAttempt(t.Context(), attemptProject, r.OperationID)
	if err != nil || a.ID != "" {
		t.Fatal("withdrawn manager claimed attempt", a, err)
	}
}

func TestSchedulingCLIPreservesHostedAndExplicitIntent(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	on := true
	revision := int64(1)
	_, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "enable-both", SchedulingRequest{ExpectedRevision: &revision, AutomaticHosted: &on, AutomaticCLI: &on})
	if err != nil {
		t.Fatal(err)
	}
	schedulingEnable(t, f, 2, false)
	got, err := f.e.ReadScheduling(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"))
	if err != nil || !got.AutomaticCLI || got.AutomaticHosted {
		t.Fatal(got, err)
	}
	original := f.enqueue(t, attemptProject, "explicit-intent", "cli", 2, 1)
	before := selectionCounts(t, f)
	schedulingCLIGenerate(t, f, false)
	if selectionCounts(t, f) != before {
		t.Fatal("duplicated explicit queued intent")
	}
	op := cliWaitRead(t, f, original.ID)
	if op.State != "queued" || op.ID != original.ID {
		t.Fatal(op)
	}
}

func TestSchedulingCLIRetainedQuotaPauseAndRestart(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingCLIEnable(t, f, 0, true)
	schedulingCLIGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	r := schedulingLatest(t, f)
	if r.State != "selected" {
		t.Fatal(r)
	}
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
	schedulingCLIGenerate(t, f, false)
	schedulingPause(t, f, 2, 0, true)
	schedulingCLIGenerate(t, f, false)
	schedulingPause(t, f, 2, 1, false)
	schedulingCLIGenerate(t, f, false)
	if after := cliWaitRead(t, f, r.OperationID); !reflect.DeepEqual(after, original) {
		t.Fatal("changed quota identity", after)
	}
	counts := selectionCounts(t, f)
	restarted, err := New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	f.e = restarted
	if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
		t.Fatal(err)
	}
	schedulingCLIGenerate(t, f, false)
	if selectionCounts(t, f) != counts {
		t.Fatal("quota wait duplicated on restart")
	}
	f.e.schedulingNow = func() time.Time { return due.Add(-time.Second) }
	if attempt, err := f.e.StartAttempt(t.Context(), attemptProject, r.OperationID); err != nil || attempt.ID != "" {
		t.Fatal(attempt, err)
	}
}

func TestSchedulingCLIControlsAndCurrentHead(t *testing.T) {
	for _, kind := range []string{"closed", "maximum", "taper", "stale-head"} {
		t.Run(kind, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			schedulingCLIEnable(t, f, 0, true)
			if kind == "closed" {
				zero := int64(0)
				_, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close", 2, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "close"})
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "maximum" || kind == "taper" {
				roundGrant(t, f, 2, "cli", 0, 1, 1, "limit")
				if kind == "maximum" {
					admissionSQL(t, f, `UPDATE review_round_controls SET used=1 WHERE project_id=? AND channel='cli'`, attemptProject)
				} else {
					admissionSQL(t, f, `UPDATE review_round_controls SET tapered=1 WHERE project_id=? AND channel='cli'`, attemptProject)
				}
			}
			schedulingCLIGenerate(t, f, true)
			selectionNext(t, f, &refreshTestReader{})
			r := schedulingLatest(t, f)
			if kind == "closed" || kind == "maximum" || kind == "taper" {
				if r.OperationID != "" {
					t.Fatal("round restriction bypassed", r)
				}
				return
			}
			if r.State != "selected" {
				t.Fatal(r)
			}
			if kind == "stale-head" {
				admissionSQL(t, f, `UPDATE review_operations SET head_sha=? WHERE id=?`, strings.Repeat("f", 40), r.OperationID)
			}
			// Normal execution gates, rather than automatic scheduling, still own final admission.
			if kind == "stale-head" {
				_, err := f.e.ExecutionContext(t.Context(), attemptProject, r.OperationID)
				if err == nil {
					t.Fatal("stale head admitted")
				}
			}
			schedulingCLIGenerate(t, f, false)
		})
	}
}

func TestSchedulingCLIFireMUDPriorityChecksSharedAccount(t *testing.T) {
	for _, mode := range []string{"competing", "paused", "other-account"} {
		t.Run(mode, func(t *testing.T) {
			f := schedulingFixture(t)
			f.configure(t, otherAttemptProject, "test-capability", 0, false)
			low := f.enqueue(t, attemptProject, "lower", "cli", 2, 1)
			high := f.enqueue(t, otherAttemptProject, "higher", "cli", 2, 1)
			admissionSQL(t, f, `UPDATE projects SET repository='benhook1013/FireMUD' WHERE id=?`, otherAttemptProject)
			if mode == "paused" {
				one := int64(0)
				yes := true
				_, err := f.e.ConfigurePullScheduling(t.Context(), f.scope(t, otherAttemptProject, f.admin, true), "pause", 2, PullSchedulingRequest{ExpectedRevision: &one, Paused: &yes})
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "other-account" {
				f.configure(t, otherAttemptProject, "other-account", 1, false)
			}
			err := f.store.Read(t.Context(), f.scope(t, attemptProject, f.writer, false), func(ctx context.Context, tx *sql.Tx) error {
				o, err := readOperation(ctx, tx, attemptProject, low.ID)
				if err != nil {
					return err
				}
				c, _, err := readConfiguration(ctx, tx, attemptProject)
				if err != nil {
					return err
				}
				preferred, err := f.e.preferredCLIQueued(ctx, tx, o, c)
				if err != nil {
					return err
				}
				if preferred != (mode == "competing") {
					t.Fatalf("priority=%v mode=%s", preferred, mode)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "competing" {
				a, err := f.e.StartAttempt(t.Context(), attemptProject, low.ID)
				if err != nil || a.ID != "" {
					t.Fatal("lower priority claimed", a, err)
				}
				if op := cliWaitRead(t, f, low.ID); op.State != "queued" {
					t.Fatal(op)
				}
				_ = high
			}
		})
	}
}

func TestSchedulingCLIPublicationAndAdjudicationRemainRequired(t *testing.T) {
	f, a, _ := cliPublicationFixture(t, 1, "completed")
	admissionSQL(t, f, `INSERT INTO workers VALUES(?,'selection-worker','Selector')`, attemptProject)
	admissionSQL(t, f, `UPDATE grants SET can_manage=1 WHERE credential_id='exec-writer-cred'`)
	one := int64(1)
	_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "single-pull", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2}, Channels: []string{"cli", "hosted"}})
	if err != nil {
		t.Fatal(err)
	}
	schedulingCLIEnable(t, f, 0, true)
	schedulingCLIGenerate(t, f, false) // Active unpublished capture cannot authorize another request.
	adjudicationComplete(t, f, a)
	schedulingCLIGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	r := schedulingLatest(t, f)
	if r.OperationID != "" || r.State != "held" {
		t.Fatal("unsettled native evidence bypassed", r)
	}
	original := snapshotTable(t, f, "review_adjudication_state")
	schedulingCLIGenerate(t, f, false)
	if original != snapshotTable(t, f, "review_adjudication_state") {
		t.Fatal("automatic adjudication changed evidence")
	}
	if op := cliWaitRead(t, f, a.OperationID); op.State != "completed" {
		t.Fatal("automation changed published provider result", op)
	}
}

func TestSchedulingCLIPartialFindingsHoldAndChangedComparisonAdvances(t *testing.T) {
	f, a, evidence := cliPublicationFixture(t, 2, "failed")
	ctx := t.Context()
	if _, err := f.e.PublishEvidence(ctx, attemptProject, evidence.ID, cliPublicationPull(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(ctx, attemptProject, a.ID, a.Revision, "failed", "coverage_incomplete", ""); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `INSERT INTO workers VALUES(?,'selection-worker','Selector')`, attemptProject)
	admissionSQL(t, f, `UPDATE grants SET can_manage=1 WHERE credential_id='exec-writer-cred'`)
	one := int64(1)
	_, err := f.e.Configure(ctx, f.scope(t, attemptProject, f.admin, true), "partial-single-pull", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2}, Channels: []string{"cli", "hosted"}})
	if err != nil {
		t.Fatal(err)
	}
	schedulingCLIEnable(t, f, 0, true)
	head := evidence.Anchor.CandidateSHA
	reader := &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: number, RepositoryID: evidence.Anchor.RepositoryID, State: "open", HeadSHA: head, BaseSHA: evidence.Anchor.BaseSHA, BaseRef: evidence.Anchor.BaseRef, ChangedFiles: 1}, nil
	}}
	check := func(reason string) {
		t.Helper()
		ok, err := f.e.generateAutomaticCLI(ctx, map[string]TargetReader{"test-capability": reader})
		if err != nil || !ok {
			t.Fatal("expected a source selection", ok, err)
		}
		selectionNext(t, f, reader)
		run := schedulingLatest(t, f)
		if reason == "" {
			if run.State != "selected" || run.OperationID == "" || cliWaitRead(t, f, run.OperationID).HeadSHA != head {
				t.Fatal("qualified changed comparison did not advance", run)
			}
		} else if run.State != "held" || run.OperationID != "" || run.Reason != reason {
			t.Fatal("automatic selection should hold", reason, run)
		}
	}
	check("review_evidence_pending")
	adjudicationDecide(t, f, evidence.ID, "partial-accept", 0, 0, "accepted")
	adjudicationDecide(t, f, evidence.ID, "partial-reject", 1, 1, "rejected")
	check("review_evidence_pending")
	coverageFix(t, f, evidence.ID, "partial-fix", 2, 0, "")
	check("automatic_intent_retained")
	if detail := cliPublicationDetail(t, f, 2, evidence.ID); detail.Run.Complete || detail.Run.Finalized || detail.Run.Accepted != 1 || detail.Run.Fixed != 1 {
		t.Fatal("partial progress was lost or incorrectly credited", detail.Run)
	}
	head = strings.Repeat("d", 40)
	changed := reviews.PullInput{Number: 2, Title: "Changed PR", State: "open", HeadSHA: head, BaseSHA: evidence.Anchor.BaseSHA, BaseRef: evidence.Anchor.BaseRef}
	scope := f.scope(t, attemptProject, f.writer, false)
	err = f.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		pins, err := reviews.PinSourceMetadata(ctx, tx, attemptProject, []int64{2})
		if err != nil {
			return err
		}
		_, err = reviews.PublishSourceMetadata(ctx, tx, scope, evidence.Anchor.RepositoryID, pins, []reviews.PullInput{changed})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	check("")
}

func TestSchedulingCLIActualParentPauseDoesNotBlockChild(t *testing.T) {
	f := schedulingFixture(t)
	one := int64(1)
	_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "stacked-cli", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}})
	if err != nil {
		t.Fatal(err)
	}
	schedulingPause(t, f, 2, 0, true)
	schedulingCLIEnable(t, f, 0, true)
	schedulingCLIGenerate(t, f, true)
	reader := &refreshTestReader{}
	reader.pull = func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		p := schedulingPull(n)
		p.HeadRef = "branch-two"
		if n == 4 {
			p.BaseRef = "branch-two"
			p.BaseSHA = strings.Repeat("a", 40)
		}
		return p, nil
	}
	reader.parents = map[string]reviewprovider.Pull{"branch-two": func() reviewprovider.Pull { p := schedulingPull(2); p.HeadRef = "branch-two"; return p }()}
	selectionNext(t, f, reader)
	r := schedulingLatest(t, f)
	if r.OperationID == "" || r.State != "selected" {
		t.Fatal("ancestor pause blocked the child review", r)
	}
	op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), r.OperationID)
	if err != nil || op.PullNumber != 4 {
		t.Fatal(op, err)
	}
}

func TestSchedulingCLIFireMUDSelectionBeforeCompetingClaim(t *testing.T) {
	f := schedulingFixture(t)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	admissionSQL(t, f, `UPDATE projects SET repository='benhook1013/FireMUD' WHERE id=?`, otherAttemptProject)
	schedulingCLIEnable(t, f, 0, true)
	zero := int64(0)
	yes, no := true, false
	_, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, otherAttemptProject, f.admin, "write"), "preferred-auto", SchedulingRequest{ExpectedRevision: &zero, AutomaticHosted: &no, AutomaticCLI: &yes})
	if err != nil {
		t.Fatal(err)
	}
	schedulingCLIGenerate(t, f, true)
	var project, id string
	if err = f.store.DB().QueryRow(`SELECT project_id,selection_id FROM review_automatic_selections`).Scan(&project, &id); err != nil || project != otherAttemptProject {
		t.Fatal("selection did not prefer FireMUD", project, id, err)
	}
	low := f.enqueue(t, attemptProject, "competing-during-source", "cli", 2, 1)
	if a, err := f.e.StartAttempt(t.Context(), attemptProject, low.ID); err != nil || a.ID != "" {
		t.Fatal("claim raced preferred pending selection", a, err)
	}
	// Once the preferred selection is no longer eligible it cannot reserve a turn.
	admissionSQL(t, f, `UPDATE review_scheduling SET automatic_cli=0 WHERE project_id=?`, otherAttemptProject)
	if a, err := f.e.StartAttempt(t.Context(), attemptProject, low.ID); err != nil || a.ID == "" {
		t.Fatal("withdrawn preferred selection blocked account", a, err)
	}
}

func TestSchedulingCLIEarlierFrontWhileLaterIntentWaits(t *testing.T) {
	f := schedulingFixture(t)
	later := f.enqueue(t, attemptProject, "later-explicit", "cli", 4, 1)
	schedulingCLIEnable(t, f, 0, true)
	schedulingCLIGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	r := schedulingLatest(t, f)
	if r.State != "selected" {
		t.Fatal(r)
	}
	earlier := cliWaitRead(t, f, r.OperationID)
	if earlier.PullNumber != 2 {
		t.Fatal(earlier)
	}
	if op := cliWaitRead(t, f, later.ID); op.State != "queued" {
		t.Fatal("later intent replaced", op)
	}
	schedulingCLIGenerate(t, f, false)
}

func TestSchedulingOtherChannelChangeKeepsCancelledIntent(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, legacy := range []bool{false, true} {
			t.Run(channel+"/legacy="+fmt.Sprint(legacy), func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2})
				scope := selectionScope(t, f, attemptProject, f.writer, "write")
				configure := func(revision int64, hosted, cli bool) {
					t.Helper()
					if _, err := f.e.ConfigureScheduling(t.Context(), scope, core.NewID("channel-basis"), SchedulingRequest{ExpectedRevision: &revision, AutomaticHosted: &hosted, AutomaticCLI: &cli}); err != nil {
						t.Fatal(err)
					}
				}
				configure(0, false, false)
				configure(1, channel == "hosted", channel == "cli")
				if legacy {
					admissionSQL(t, f, `UPDATE events SET payload_json=json_remove(payload_json,'$.hosted_basis_revision','$.cli_basis_revision') WHERE event_type='review.scheduling_updated'`)
				}
				generate := schedulingGenerate
				if channel == "cli" {
					generate = schedulingCLIGenerate
				}
				generate(t, f, true)
				selectionNext(t, f, &refreshTestReader{})
				r := schedulingLatest(t, f)
				if _, err := f.e.ControlOperation(t.Context(), scope, "cancel-basis", r.OperationID, OperationControl{ExpectedRevision: 1, Action: "cancel"}); err != nil {
					t.Fatal(err)
				}
				before := snapshotTable(t, f, "review_selections") + snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_automatic_selections")
				for revision := int64(2); revision <= 4; revision++ {
					other := revision%2 == 0
					hosted, cli := true, other
					if channel == "cli" {
						hosted, cli = other, true
					}
					configure(revision, hosted, cli)
					generate(t, f, false)
				}
				restarted, err := New(f.store, attemptCapabilities)
				if err != nil {
					t.Fatal(err)
				}
				f.e = restarted
				if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
					t.Fatal(err)
				}
				generate(t, f, false)
				if snapshotTable(t, f, "review_selections")+snapshotTable(t, f, "review_operations")+snapshotTable(t, f, "review_automatic_selections") != before {
					t.Fatal("other channel recreated cancelled intent")
				}
				// Reasserting the current settings deliberately rearms supplied channels.
				configure(5, true, true)
				generate(t, f, true)
			})
		}
	}
}

func TestSchedulingPriorityIsolatesOffendingCandidate(t *testing.T) {
	for _, state := range []string{"queued-operation", "queued-selection"} {
		for _, fault := range []string{"scheduling", "pause", "configuration", "selection", "sql"} {
			t.Run(state+"/"+fault, func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2})
				f.configure(t, otherAttemptProject, "test-capability", 0, false)
				low := f.enqueue(t, otherAttemptProject, "healthy-low", "cli", 2, 1)
				schedulingCLIEnable(t, f, 0, true)
				schedulingCLIGenerate(t, f, true)
				if state == "queued-operation" {
					selectionNext(t, f, &refreshTestReader{})
				}
				r := schedulingLatest(t, f)
				admissionSQL(t, f, `UPDATE projects SET repository='benhook1013/FireMUD' WHERE id=?`, attemptProject)
				switch fault {
				case "scheduling":
					admissionSQL(t, f, `UPDATE review_scheduling SET updated_at='malformed' WHERE project_id=?`, attemptProject)
				case "pause":
					admissionSQL(t, f, `INSERT INTO review_pull_scheduling VALUES(?,2,1,0,'invalid','actor',?)`, attemptProject, stamp())
				case "configuration":
					admissionSQL(t, f, `UPDATE review_execution_config SET body_json='not-json' WHERE project_id=?`, attemptProject)
				case "selection":
					admissionSQL(t, f, `UPDATE review_selections SET updated_at='malformed' WHERE id=?`, r.ID)
				case "sql":
					admissionSQL(t, f, `ALTER TABLE review_scheduling RENAME TO unavailable_scheduling`)
				}
				a, err := f.e.StartAttempt(t.Context(), otherAttemptProject, low.ID)
				if fault == "sql" {
					if err == nil || automaticProjectRefused(err) || a.ID != "" {
						t.Fatal("priority swallowed SQL error", a, err)
					}
					return
				}
				if err != nil || a.ID == "" || a.OperationID != low.ID {
					t.Fatal("other project's damage blocked healthy claim", a, err)
				}
				if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts WHERE project_id=?`, attemptProject) != 0 {
					t.Fatal("damaged preferred candidate claimed")
				}
			})
		}
	}
}

func TestSchedulingChannelBasisKeepsAuthorityAndLegacySemantics(t *testing.T) {
	for _, mode := range []string{"own-toggle", "actor", "worker", "credential", "omitted-cli", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			scope := selectionScope(t, f, attemptProject, f.writer, "write")
			on, off, zero, one := true, false, int64(0), int64(1)
			if _, err := f.e.ConfigureScheduling(t.Context(), scope, "initial-both", SchedulingRequest{ExpectedRevision: &zero, AutomaticHosted: &on, AutomaticCLI: &on}); err != nil {
				t.Fatal(err)
			}
			read := func() (int64, int64) {
				t.Helper()
				var hosted, cli int64
				if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					s, err := readScheduling(ctx, tx, attemptProject)
					if err != nil {
						return err
					}
					hosted, cli, err = schedulingBasisRevisions(ctx, tx, s)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return hosted, cli
			}
			input := SchedulingRequest{ExpectedRevision: &one, AutomaticHosted: &off, AutomaticCLI: &on}
			switch mode {
			case "actor":
				scope = selectionScope(t, f, attemptProject, f.admin, "write")
			case "worker":
				admissionSQL(t, f, `INSERT INTO workers VALUES(?,'new-authorizer','New authorizer')`, attemptProject)
				scope.WorkerID = "new-authorizer"
			case "credential":
				// Reauthorizing retained settings after credential rotation must
				// matter even when the principal and worker are unchanged.
				admissionSQL(t, f, `UPDATE review_scheduling SET credential_id='retired-credential' WHERE project_id=?`, attemptProject)
			case "omitted-cli":
				input.AutomaticHosted, input.AutomaticCLI = &on, nil
			case "legacy":
				admissionSQL(t, f, `UPDATE events SET payload_json=json_remove(payload_json,'$.hosted_basis_revision','$.cli_basis_revision') WHERE event_type='review.scheduling_updated'`)
				if h, c := read(); h != 1 || c != 1 {
					t.Fatal("legacy basis changed", h, c)
				}
			}
			if _, err := f.e.ConfigureScheduling(t.Context(), scope, "change-settings", input); err != nil {
				t.Fatal(err)
			}
			hosted, cli := read()
			wantCLI := int64(1)
			if mode == "actor" || mode == "worker" || mode == "credential" {
				wantCLI = 2
			}
			if hosted != 2 || cli != wantCLI {
				t.Fatal("wrong channel basis", hosted, cli, wantCLI)
			}
		})
	}
}

func TestSchedulingChannelBasisRejectsInvalidCurrentHistory(t *testing.T) {
	for _, fault := range []string{"missing", "duplicate", "actor", "bit", "null", "partial", "future", "zero", "malformed", "oversized", "sql"} {
		t.Run(fault, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			schedulingEnable(t, f, 0, true)
			switch fault {
			case "missing":
				admissionSQL(t, f, `DELETE FROM events WHERE event_type='review.scheduling_updated'`)
			case "duplicate":
				admissionSQL(t, f, `INSERT INTO events(project_id,event_type,resource_id,revision,actor_id,payload_json,created_at) SELECT project_id,event_type,resource_id,revision,actor_id,payload_json,created_at FROM events WHERE event_type='review.scheduling_updated'`)
			case "actor":
				admissionSQL(t, f, `UPDATE events SET actor_id='other-actor' WHERE event_type='review.scheduling_updated'`)
			case "bit":
				admissionSQL(t, f, `UPDATE events SET payload_json=json_set(payload_json,'$.automatic_hosted',json('false')) WHERE event_type='review.scheduling_updated'`)
			case "null":
				admissionSQL(t, f, `UPDATE events SET payload_json=json_set(payload_json,'$.hosted_basis_revision',NULL) WHERE event_type='review.scheduling_updated'`)
			case "partial":
				admissionSQL(t, f, `UPDATE events SET payload_json=json_remove(payload_json,'$.cli_basis_revision') WHERE event_type='review.scheduling_updated'`)
			case "future":
				admissionSQL(t, f, `UPDATE events SET payload_json=json_set(payload_json,'$.hosted_basis_revision',2) WHERE event_type='review.scheduling_updated'`)
			case "zero":
				admissionSQL(t, f, `UPDATE events SET payload_json=json_set(payload_json,'$.cli_basis_revision',0) WHERE event_type='review.scheduling_updated'`)
			case "malformed":
				admissionSQL(t, f, `UPDATE events SET payload_json='{}' WHERE event_type='review.scheduling_updated'`)
			case "oversized":
				admissionSQL(t, f, `UPDATE events SET payload_json=payload_json || ? WHERE event_type='review.scheduling_updated'`, strings.Repeat(" ", 4097))
			case "sql":
				admissionSQL(t, f, `ALTER TABLE events RENAME TO unavailable_events`)
			}
			err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				_, err := automaticPullBasisChannel(ctx, tx, attemptProject, 2, "hosted")
				return err
			})
			if err == nil || automaticProjectRefused(err) != (fault != "sql") {
				t.Fatal("invalid history classification", fault, err)
			}
		})
	}
}
