package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func schedulingFixture(t *testing.T) *attemptFixture {
	return schedulingFixtureWithQueue(t, []int64{2, 4})
}

func schedulingFixtureWithQueue(t *testing.T, queue []int64) *attemptFixture {
	t.Helper()
	f := selectionFixtureWithQueue(t, queue)
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	return f
}

func schedulingPause(t *testing.T, f *attemptFixture, pull, revision int64, paused bool) {
	t.Helper()
	_, err := f.e.ConfigurePullScheduling(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), core.NewID("pause"), pull, PullSchedulingRequest{ExpectedRevision: &revision, Paused: &paused, Reason: "Fix in progress"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSchedulingHTTPDefaultsStrictCASAndPermissions(t *testing.T) {
	f := schedulingFixture(t)
	call := func(method, path, token, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/v1/projects/"+attemptProject+"/reviews/"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-FC-Worker", "selection-worker")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	project := call("GET", "scheduling", f.reader, "", "")
	if project.Code != 200 || strings.TrimSpace(project.Body.String()) != `{"paused_pulls":[],"project_id":"exec-project-one","revision":0,"automatic_cli":false,"automatic_hosted":false}` {
		t.Fatal(project.Code, project.Body.String())
	}
	checkPullScheduling := func(w *httptest.ResponseRecorder, revision int64, paused bool, reason string) {
		t.Helper()
		var p PullScheduling
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &p) != nil ||
			p.ProjectID != attemptProject || p.PullNumber != 2 || p.Paused != paused || p.Revision != revision || p.Reason != reason {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	const pull = "pulls/2/scheduling"
	checkPullScheduling(call("GET", pull, f.reader, "", ""), 0, false, "")
	for _, path := range []string{"scheduling", pull} {
		head := call("HEAD", path, f.reader, "", "")
		if head.Code != http.StatusOK || !strings.HasPrefix(head.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("HEAD %s must use the read path: status=%d body=%s", path, head.Code, head.Body.String())
		}
	}
	journal := snapshotTable(t, f, "events")
	// Even a project manager cannot turn on generation through this foundation slice.
	admissionSQL(t, f, `UPDATE grants SET can_manage=1 WHERE credential_id='exec-writer-cred'`)
	if w := call("POST", "scheduling", f.writer, "disabled-project-post", `{"expected_revision":0,"automatic_hosted":true,"automatic_cli":true}`); w.Code != http.StatusMethodNotAllowed {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("GET", "scheduling", f.reader, "", ""); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), project.Body.Bytes()) || journal != snapshotTable(t, f, "events") {
		t.Fatal("project POST changed scheduling or history", w.Code, w.Body.String())
	}
	admissionSQL(t, f, `UPDATE grants SET can_manage=0 WHERE credential_id='exec-writer-cred'`)
	const pause = `{"expected_revision":0,"paused":true,"reason":"Fixing"}`
	if w := call("POST", pull, f.reader, "reader-pause", pause); w.Code != http.StatusForbidden {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, body := range []string{`{"expected_revision":0,"paused":null}`, `{"expected_revision":0,"paused":true,"reason":null}`, `{"expected_revision":0,"paused":true,"reason":"\n"}`} {
		if w := call("POST", pull, f.writer, core.NewID("key"), body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	first := call("POST", pull, f.writer, "pause", pause)
	var receipt Receipt
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &receipt) != nil || receipt.ID != "2" || receipt.State != "pull_scheduling_updated" || receipt.Revision != 1 {
		t.Fatal(first.Code, first.Body.String())
	}
	checkPullScheduling(call("GET", pull, f.reader, "", ""), 1, true, "Fixing")
	if w := call("POST", pull, f.writer, "stale", `{"expected_revision":0,"paused":false}`); w.Code != http.StatusConflict {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("POST", pull, f.writer, "pause", pause); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), first.Body.Bytes()) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("POST", pull, f.writer, "resume", `{"expected_revision":1,"paused":false,"reason":"discarded"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	checkPullScheduling(call("GET", pull, f.reader, "", ""), 2, false, "")
}

func TestSchedulingPausedBothChannelsPreservesQueuedIdentityAndOperationPause(t *testing.T) {
	f := schedulingFixture(t)
	r := f.enqueue(t, attemptProject, "existing", "cli", 2, 1)
	schedulingPause(t, f, 2, 0, true)
	before := roundTables(t, f)
	for _, channel := range []string{"cli", "hosted"} {
		_, err := f.e.Enqueue(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), core.NewID("key"), Request{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: strings.Repeat("a", 40), Channel: channel})
		attemptCode(t, err, "pull_scheduling_paused")
	}
	a, err := f.e.StartAttempt(t.Context(), attemptProject, r.ID)
	if err != nil || a.ID != "" {
		t.Fatal(a, err)
	}
	if roundTables(t, f) != before {
		t.Fatal("paused admission changed retained state")
	}
	schedulingPause(t, f, 2, 1, false)
	a, err = f.e.StartAttempt(t.Context(), attemptProject, r.ID)
	if err != nil || a.ID == "" {
		t.Fatal(a, err)
	}
	schedulingPause(t, f, 2, 2, true)
	_, err = f.e.ExecutionContext(t.Context(), attemptProject, r.ID)
	attemptCode(t, err, "pull_scheduling_paused")
	// A separate operation pause is not cleared by resuming PR scheduling.
	other := f.enqueue(t, attemptProject, "other", "hosted", 4, 1)
	scope := selectionScope(t, f, attemptProject, f.writer, "write")
	_, err = f.e.ControlOperation(t.Context(), scope, "operation-pause", other.ID, OperationControl{ExpectedRevision: 1, Action: "pause"})
	if err != nil {
		t.Fatal(err)
	}
	schedulingPause(t, f, 4, 0, true)
	schedulingPause(t, f, 4, 1, false)
	op, err := f.e.ReadOperation(t.Context(), scope, other.ID)
	if err != nil || op.Wait == nil || !op.Wait.Paused {
		t.Fatal(op, err)
	}
}

func TestSchedulingCooldownPauseKeepsOriginalTimerAndCollectionContinues(t *testing.T) {
	f, a, in, now := queuedCooldown(t)
	scope := f.scope(t, attemptProject, f.writer, false)
	pause := true
	zero := int64(0)
	if _, err := f.e.ConfigurePullScheduling(t.Context(), scope, "pr-pause", 2, PullSchedulingRequest{ExpectedRevision: &zero, Paused: &pause}); err != nil {
		t.Fatal(err)
	}
	original := snapshotTable(t, f, "review_operation_waits") + snapshotTable(t, f, "review_hosted_preflight") + snapshotTable(t, f, "review_hosted_allowance_queries") + snapshotTable(t, f, "review_hosted_observations")
	*now = in.Observation.RetryNotBefore
	pending, err := f.e.pending(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	next, err := f.e.StartAttempt(t.Context(), attemptProject, a.OperationID)
	if err != nil || next.ID != "" {
		t.Fatal(next, err)
	}
	pause = false
	one := int64(1)
	if _, err = f.e.ConfigurePullScheduling(t.Context(), scope, "pr-resume", 2, PullSchedulingRequest{ExpectedRevision: &one, Paused: &pause}); err != nil {
		t.Fatal(err)
	}
	if original != snapshotTable(t, f, "review_operation_waits")+snapshotTable(t, f, "review_hosted_preflight")+snapshotTable(t, f, "review_hosted_allowance_queries")+snapshotTable(t, f, "review_hosted_observations") {
		t.Fatal("PR pause rewrote original cooldown")
	}
	next, err = f.e.StartAttempt(t.Context(), attemptProject, a.OperationID)
	if err != nil || next.ID == "" || next.OperationID != a.OperationID || next.ID == a.ID {
		t.Fatal(next, err)
	}
	// A pause after issuance never prevents retaining/settling the original result.
	g, active, capture := hostedPublicationFixture(t, true)
	pause = true
	if _, err = g.e.ConfigurePullScheduling(t.Context(), g.scope(t, attemptProject, g.writer, false), "pause-issued", 2, PullSchedulingRequest{ExpectedRevision: &zero, Paused: &pause}); err != nil {
		t.Fatal(err)
	}
	retained, err := g.e.RecordReviewEvidence(t.Context(), attemptProject, active.ID, capture)
	if err != nil || retained.ID == "" {
		t.Fatal(retained, err)
	}
	if _, err = g.e.Advance(t.Context(), attemptProject, active.ID, active.Revision, "completed", "captured_original_result", active.ExternalID); err != nil {
		t.Fatal(err)
	}
	if count := attemptCount(t, g.store, `SELECT count(*) FROM review_execution_evidence`); count != 1 {
		t.Fatal(count)
	}
}

func TestSchedulingFinalActionPauseRace(t *testing.T) {
	f, a, _ := hostedActionFixture(t)
	if err := f.e.VerifyAction(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal("fixture was not dispatchable", err)
	}
	zero := int64(0)
	paused := true
	_, err := f.e.ConfigurePullScheduling(t.Context(), f.scope(t, attemptProject, f.writer, false), "pause-final-race", 2, PullSchedulingRequest{ExpectedRevision: &zero, Paused: &paused})
	if err != nil {
		t.Fatal(err)
	}
	before := attemptState(t, f, a)
	err = f.e.VerifyAction(t.Context(), attemptProject, a.ID, a.Revision)
	attemptCode(t, err, "pull_scheduling_paused")
	if attemptState(t, f, a) != before {
		t.Fatal("final guard changed journal")
	}
}

func TestSchedulingPauseBeforeAnyIntentBulkReadAndUnenrolledIsolation(t *testing.T) {
	f := schedulingFixture(t)
	schedulingPause(t, f, 4, 0, true)
	schedulingPause(t, f, 99, 0, true)
	schedulingPause(t, f, 2, 0, true)
	scope := selectionScope(t, f, attemptProject, f.writer, "read")
	view, err := f.e.ReadScheduling(t.Context(), scope)
	if err != nil || len(view.PausedPulls) != 2 || view.PausedPulls[0].PullNumber != 2 || view.PausedPulls[1].PullNumber != 4 {
		t.Fatal("bulk pauses lost queue order or leaked unenrolled PR", view, err)
	}
	p, err := f.e.ReadPullScheduling(t.Context(), scope, 99)
	if err != nil || !p.Paused {
		t.Fatal("pause before enrollment was lost", p, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("manual pause created review work")
	}
}

func TestSchedulingQueuedPauseDiagnosticPrecedence(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, wait := range []string{"scheduler", "queue", "draft"} {
			t.Run(channel+"/"+wait, func(t *testing.T) {
				f := schedulingFixture(t)
				pull := int64(2)
				if wait == "queue" {
					f.enqueue(t, attemptProject, "earlier", channel, 2, 1)
					pull = 4
				}
				r := f.enqueue(t, attemptProject, "queued-pause", channel, pull, 1)
				scope := selectionScope(t, f, attemptProject, f.writer, "write")
				admissionSQL(t, f, `INSERT INTO review_operation_waits(operation_id,baseline_attempt_id,retry_not_before,paused) VALUES(?,NULL,'',0)`, r.ID)
				if wait == "draft" {
					admissionSQL(t, f, `UPDATE review_operations SET reason=? WHERE id=?`, cliDraftWait, r.ID)
				}
				schedulingPause(t, f, pull, 0, true)
				check := func(want string) {
					t.Helper()
					if got, _ := f.e.queuedDiagnosticDetails(t.Context(), scope, r.ID, r.Revision); got != want {
						t.Fatalf("diagnostic=%q want=%q", got, want)
					}
				}
				check("pull_scheduling_paused")
				admissionSQL(t, f, `UPDATE review_execution_config SET body_json=json_set(body_json,'$.paused',json('true')) WHERE project_id=?`, attemptProject)
				check("project_paused")
				var originalBody string
				if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_config WHERE project_id=?`, attemptProject).Scan(&originalBody); err != nil {
					t.Fatal(err)
				}
				for _, invalid := range []string{strings.TrimSuffix(originalBody, "}") + `,"paused":true}`, originalBody + strings.Repeat(" ", 9000)} {
					admissionSQL(t, f, `UPDATE review_execution_config SET body_json=? WHERE project_id=?`, invalid, attemptProject)
					check("")
				}
				admissionSQL(t, f, `UPDATE review_execution_config SET body_json=? WHERE project_id=?`, originalBody, attemptProject)
				capability := f.e.capabilities["test-capability"]
				delete(f.e.capabilities, "test-capability")
				check("")
				f.e.capabilities["test-capability"] = capability
				check("project_paused")
				admissionSQL(t, f, `UPDATE review_operation_waits SET paused=1 WHERE operation_id=?`, r.ID)
				check("operation_paused")
			})
		}
	}
}

func TestSchedulingPausePreservesRetainedCooldown(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			var f *attemptFixture
			var a Attempt
			var now *time.Time
			if channel == "cli" {
				f, a, _, now = cliWaitFixture(t)
				if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
					t.Fatal(err)
				}
			} else {
				f, a, _, now = queuedCooldown(t)
			}
			scope := f.scope(t, attemptProject, f.writer, false)
			pause := func(revision int64, paused bool) {
				_, err := f.e.ConfigurePullScheduling(t.Context(), scope, core.NewID("timer-pause"), 2, PullSchedulingRequest{ExpectedRevision: &revision, Paused: &paused})
				if err != nil {
					t.Fatal(err)
				}
			}
			before := cliWaitRead(t, f, a.OperationID)
			pause(0, true)
			if got, _ := f.e.queuedDiagnosticDetails(t.Context(), scope, before.ID, before.Revision); got != "pull_scheduling_paused" {
				t.Fatal("quota/cooldown hid PR pause", got)
			}
			pause(1, false)
			*now = cliWaitDue(t, before).Add(time.Second)
			driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
				pause(2, true)
				_, err := f.e.ExecutionContext(ctx, o.ProjectID, o.ID)
				return Preparation{}, err
			}}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal(err)
			}
			after := cliWaitRead(t, f, a.OperationID)
			if driver.prepares.Load() != 1 || driver.calls.Load() != 0 || after.State != "queued" || after.Reason != "pull_scheduling_paused" || !reflect.DeepEqual(after.Wait, before.Wait) {
				t.Fatal("lost original cooldown", before, after)
			}
			assertCLIWaitSnapshot(t, f)
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("leaked reservation")
			}
		})
	}
}

func TestSchedulingPreparationGuardCannotRequeueAdmittedAttempt(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, reason := range []string{"pull_scheduling_paused", "automation_authorization_changed"} {
			t.Run(channel+"/"+reason, func(t *testing.T) {
				f := supervisorFixture(t)
				r := f.enqueue(t, attemptProject, "admitted-guard", channel, 2, 1)
				a := f.start(t, attemptProject, r)
				intent := ""
				if channel == "hosted" {
					intent = "synthetic-trigger"
				}
				admitted, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), intent)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, admitted.Revision, "held", reason, ""); err == nil {
					t.Fatal("admitted attempt accepted a preparation hold")
				}
				if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, admitted.Revision, "failed", reason, ""); err != nil {
					t.Fatal(err)
				}
				after := cliWaitRead(t, f, r.ID)
				if after.State != "failed" || after.AttemptID != a.ID {
					t.Fatal("admitted action silently requeued", after)
				}
			})
		}
	}
}

func TestSchedulingDetachedGuardHistoryRejectsCorruption(t *testing.T) {
	for _, corrupt := range []string{"anchor", "intent", "external", "state", "revision", "reason"} {
		t.Run(corrupt, func(t *testing.T) {
			f := schedulingFixture(t)
			r := f.enqueue(t, attemptProject, "detached-guard", "hosted", 2, 1)
			a := f.start(t, attemptProject, r)
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "pull_scheduling_paused", ""); err != nil {
				t.Fatal(err)
			}
			assertCLIWaitSnapshot(t, f)
			o := cliWaitRead(t, f, r.ID)
			current, err := readAttempt(t.Context(), f.store.DB(), attemptProject, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch corrupt {
			case "anchor":
				anchor := attemptAnchor(attemptProject, 2)
				current.Anchor = &anchor
			case "intent":
				current.Intent = "possible-post"
			case "external":
				current.ExternalID = "trigger"
			case "state":
				current.State = "active"
			case "revision":
				current.Revision = 1
			case "reason":
				admissionSQL(t, f, `UPDATE review_execution_transitions SET reason='preparation_failed' WHERE attempt_id=? AND revision=2`, a.ID)
			}
			_, recognized, err := historicalUnusedPreparationOperation(t.Context(), f.store.DB(), o, current, true)
			if err == nil && recognized {
				t.Fatal("accepted corrupt detached attempt", corrupt)
			}
		})
	}
}

func TestSchedulingDetachedGuardProjectsOriginalAttempt(t *testing.T) {
	for _, reason := range []string{"pull_scheduling_paused", "automation_authorization_changed"} {
		t.Run(reason, func(t *testing.T) {
			f := attemptProjectionFixture(t, true)
			r := f.enqueue(t, attemptProject, "guard-projection", "hosted", 2, 1)
			a := f.start(t, attemptProject, r)
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", reason, ""); err != nil {
				t.Fatal(err)
			}
			_, projected := projectedAttempt(t, f, a)
			if projected.State != "held" || projected.Reason != reason {
				t.Fatal(projected)
			}
			assertCLIWaitSnapshot(t, f)
			o := cliWaitRead(t, f, r.ID)
			if _, err := f.e.ControlOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), "cancel-guard", o.ID, OperationControl{ExpectedRevision: o.Revision, Action: "cancel"}); err != nil {
				t.Fatal(err)
			}
			assertCLIWaitSnapshot(t, f)
			if _, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, nil); err != nil {
				t.Fatal("historical projection after cancellation", err)
			}
		})
	}
}

func validateSchedulingSnapshotForTest(t *testing.T, f *attemptFixture) error {
	t.Helper()
	return f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return ValidateSchedulingSnapshot(ctx, tx)
	})
}

func TestValidateSchedulingSnapshotRejectsStaleProjectProjection(t *testing.T) {
	f := schedulingFixture(t)
	scope := selectionScope(t, f, attemptProject, f.writer, "write")
	at := stamp()
	// Retained inputs only: model an accepted enable followed by a disable.
	// The current row and both journal events must agree before simulating a
	// stale projection. No selection or provider action is constructed.
	admissionSQL(t, f, `INSERT INTO review_scheduling
		(project_id,revision,automatic_hosted,automatic_cli,actor_id,worker_id,credential_id,updated_at)
		VALUES(?,2,0,0,?,'','',?)`, attemptProject, scope.PrincipalID, at)
	admissionSQL(t, f, `INSERT INTO events(project_id,event_type,resource_id,revision,actor_id,payload_json,created_at) VALUES
		(?,'review.scheduling_updated',?,1,?,'{"automatic_hosted":true,"automatic_cli":false}',?),
		(?,'review.scheduling_updated',?,2,?,'{"automatic_hosted":false,"automatic_cli":false}',?)`,
		attemptProject, attemptProject, scope.PrincipalID, at, attemptProject, attemptProject, scope.PrincipalID, at)
	if err := validateSchedulingSnapshotForTest(t, f); err != nil {
		t.Fatalf("healthy rev-2 projection rejected: %v", err)
	}
	admissionSQL(t, f, `UPDATE review_scheduling SET revision=1,automatic_hosted=1,worker_id='selection-worker',credential_id='exec-writer-cred' WHERE project_id=?`, attemptProject)
	attemptCode(t, validateSchedulingSnapshotForTest(t, f), "storage_unavailable")
}

func TestValidateSchedulingSnapshotRejectsMissingPausedPullProjection(t *testing.T) {
	f := schedulingFixture(t)
	schedulingPause(t, f, 2, 0, true)
	schedulingPause(t, f, 4, 0, true)
	schedulingPause(t, f, 4, 1, false)
	if err := validateSchedulingSnapshotForTest(t, f); err != nil {
		t.Fatalf("healthy paused and resumed projections rejected: %v", err)
	}
	admissionSQL(t, f, `DELETE FROM review_pull_scheduling WHERE project_id=? AND pull_number=4`, attemptProject)
	if err := validateSchedulingSnapshotForTest(t, f); err != nil {
		t.Fatalf("absent projection for a latest genuine resume refused: %v", err)
	}
	admissionSQL(t, f, `DELETE FROM review_pull_scheduling WHERE project_id=? AND pull_number=2`, attemptProject)
	attemptCode(t, validateSchedulingSnapshotForTest(t, f), "storage_unavailable")
}

func TestValidateSchedulingSnapshotRejectsMalformedAndFutureVersions(t *testing.T) {
	for _, version := range []string{"not-a-version", "3"} {
		t.Run(version, func(t *testing.T) {
			f := schedulingFixture(t)
			admissionSQL(t, f, `UPDATE metadata SET value=? WHERE key='schema:review_scheduling'`, version)
			attemptCode(t, validateSchedulingSnapshotForTest(t, f), "storage_unavailable")
		})
	}
}

func TestValidateSchedulingSnapshotRequiresPresentDomainMetadata(t *testing.T) {
	f := schedulingFixture(t)
	admissionSQL(t, f, `DELETE FROM metadata WHERE key='schema:review_scheduling'`)
	if err := validateSchedulingSnapshotForTest(t, f); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing optional domain metadata should not be mistaken for a schema version: %v", err)
	}
}

func TestSchedulingWaitSnapshotRequiresActualDraftProof(t *testing.T) {
	f := schedulingFixture(t)
	r := f.enqueue(t, attemptProject, "scheduling-only-cli", "cli", 2, 1)
	a := f.start(t, attemptProject, r)
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "pull_scheduling_paused", ""); err != nil {
		t.Fatal(err)
	}
	assertCLIWaitSnapshot(t, f)
	o := cliWaitRead(t, f, r.ID)
	o.Reason = cliDraftWait
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return validateOperationWaitHistory(ctx, tx, o)
	})
	if err == nil {
		t.Fatal("scheduling-only history was accepted as draft proof")
	}
}
