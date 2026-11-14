package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func schedulingFixture(t *testing.T) *attemptFixture {
	return schedulingFixtureWithQueue(t, []int64{2, 4})
}
func schedulingFixtureWithQueue(t *testing.T, queue []int64) *attemptFixture {
	f := selectionFixtureWithQueue(t, queue)
	admissionSQL(t, f, `UPDATE grants SET can_manage=1 WHERE credential_id='exec-writer-cred'`)
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	return f
}
func schedulingEnable(t *testing.T, f *attemptFixture, revision int64, on bool) {
	t.Helper()
	_, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "automatic-"+core.NewID("key"), SchedulingRequest{ExpectedRevision: &revision, AutomaticHosted: &on})
	if err != nil {
		t.Fatal(err)
	}
}
func schedulingPause(t *testing.T, f *attemptFixture, pull, revision int64, paused bool) {
	t.Helper()
	_, err := f.e.ConfigurePullScheduling(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), core.NewID("pause"), pull, PullSchedulingRequest{ExpectedRevision: &revision, Paused: &paused, Reason: "Fix in progress"})
	if err != nil {
		t.Fatal(err)
	}
}
func schedulingGenerate(t *testing.T, f *attemptFixture, want bool) {
	t.Helper()
	worked, err := f.e.generateAutomaticHosted(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
	if err != nil || worked != want {
		t.Fatalf("automatic selection worked=%v want=%v: %v", worked, want, err)
	}
}
func schedulingLatest(t *testing.T, f *attemptFixture) Run {
	t.Helper()
	var id string
	if err := f.store.DB().QueryRow(`SELECT selection_id FROM review_automatic_selections ORDER BY rowid DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return selectionRead(t, f, id)
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
	w := call("GET", "scheduling", f.reader, "", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"paused_pulls":[],"project_id":"exec-project-one","revision":0,"automatic_cli":false,"automatic_hosted":false}` {
		t.Fatal(w.Code, w.Body.String())
	}
	w = call("GET", "pulls/2/scheduling", f.reader, "", "")
	var p PullScheduling
	if json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Paused || p.Revision != 0 || p.Reason != "" {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, body := range []string{`{}`, `{"expected_revision":0}`, `{"expected_revision":0,"automatic_hosted":null}`, `{"expected_revision":0,"automatic_hosted":false,"automatic_cli":null}`, `{"expected_revision":0,"automatic_hosted":true,"token":"private"}`, `{"expected_revision":0,"automatic_hosted":true,"automatic_hosted":false}`} {
		if w = call("POST", "scheduling", f.writer, core.NewID("key"), body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	admissionSQL(t, f, `UPDATE grants SET can_manage=0 WHERE credential_id='exec-writer-cred'`)
	if w = call("POST", "scheduling", f.writer, "permission", `{"expected_revision":0,"automatic_hosted":true}`); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	admissionSQL(t, f, `UPDATE grants SET can_manage=1 WHERE credential_id='exec-writer-cred'`)
	w = call("POST", "scheduling", f.writer, "enable", `{"expected_revision":0,"automatic_hosted":true}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	original := append([]byte(nil), w.Body.Bytes()...)
	if w = call("POST", "scheduling", f.writer, "stale", `{"expected_revision":0,"automatic_hosted":false}`); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if w = call("POST", "scheduling", f.writer, "enable", `{"expected_revision":0,"automatic_hosted":true}`); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), original) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = call("GET", "scheduling", f.reader, "", "")
	if strings.Contains(w.Body.String(), "credential") || strings.Contains(w.Body.String(), "principal") || !strings.Contains(w.Body.String(), `"authorizing_worker_id":"selection-worker"`) {
		t.Fatal(w.Body.String())
	}
	for _, body := range []string{`{"expected_revision":0,"paused":null}`, `{"expected_revision":0,"paused":true,"reason":null}`, `{"expected_revision":0,"paused":true,"reason":"\n"}`} {
		if w = call("POST", "pulls/2/scheduling", f.writer, core.NewID("key"), body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w = call("POST", "pulls/2/scheduling", f.writer, "pause", `{"expected_revision":0,"paused":true,"reason":"Fixing"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = call("POST", "pulls/2/scheduling", f.writer, "resume", `{"expected_revision":1,"paused":false,"reason":"discarded"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = call("GET", "pulls/2/scheduling", f.reader, "", "")
	if json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Paused || p.Reason != "" || p.Revision != 2 {
		t.Fatal(w.Body.String())
	}
	admissionSQL(t, f, `UPDATE grants SET can_manage=0 WHERE credential_id='exec-writer-cred'`)
	if w = call("POST", "scheduling", f.writer, "enable", `{"expected_revision":0,"automatic_hosted":true}`); w.Code != 403 {
		t.Fatal("replay bypassed current management", w.Code)
	}
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
func TestSchedulingAutomaticDefaultOffCancellationAndRestart(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingGenerate(t, f, false)
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	r := schedulingLatest(t, f)
	if r.Channel != "hosted" || r.ActorID != "exec-writer-principal" || r.WorkerID != "selection-worker" {
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
	schedulingGenerate(t, f, false)
	restarted, err := New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	f.e = restarted
	if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
		t.Fatal(err)
	}
	schedulingGenerate(t, f, false)
	if selectionCounts(t, f) != counts {
		t.Fatal("cancelled intent regenerated")
	}
	// A deliberate scheduling revision authorizes a new semantic intent, not a tick.
	schedulingEnable(t, f, 1, true)
	schedulingGenerate(t, f, true)
}
func TestSchedulingAutomaticAuthorizerWithdrawalAndRestoreHold(t *testing.T) {
	for _, kind := range []string{"management", "revoked", "retired", "disabled", "restore"} {
		t.Run(kind, func(t *testing.T) {
			f := schedulingFixture(t)
			schedulingEnable(t, f, 0, true)
			switch kind {
			case "management":
				admissionSQL(t, f, `UPDATE grants SET can_manage=0 WHERE credential_id='exec-writer-cred'`)
			case "revoked":
				admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
			case "retired":
				admissionSQL(t, f, `INSERT INTO worker_retirements VALUES(?,'selection-worker',?,'synthetic-actor')`, attemptProject, stamp())
			case "disabled":
				schedulingEnable(t, f, 1, false)
			case "restore":
				admissionSQL(t, f, `INSERT INTO metadata VALUES('restore:provider_state','held')`)
			}
			schedulingGenerate(t, f, false)
			if attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 0 {
				t.Fatal("withdrawn authority generated selection")
			}
		})
	}
	// Authorization is also checked after a selection has created its child.
	f := schedulingFixture(t)
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	r := schedulingLatest(t, f)
	admissionSQL(t, f, `UPDATE grants SET can_manage=0 WHERE credential_id='exec-writer-cred'`)
	a, err := f.e.StartAttempt(t.Context(), attemptProject, r.OperationID)
	if err != nil || a.ID != "" {
		t.Fatal("withdrawn manager claimed attempt", a, err)
	}
}
func TestSchedulingPausedFrontSkipsIndependentAndStackedReviews(t *testing.T) {
	f := schedulingFixture(t)
	schedulingPause(t, f, 2, 0, true)
	r, _ := selectionEnqueue(t, f, "independent-paused", "hosted", nil)
	selectionNext(t, f, &refreshTestReader{})
	got := selectionRead(t, f, r.ID)
	if got.State != "selected" {
		t.Fatal(got)
	}
	op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), got.OperationID)
	if err != nil || op.PullNumber != 4 {
		t.Fatal(op, err)
	}
	// The parent pause stays local; selecting the child grants no parent completion.
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		base := strings.Repeat("b", 40)
		head := strings.Repeat("a", 40)
		plans := []SourcePlan{{Pull: schedulingPull(2), ParentSHA: base}, {Pull: schedulingPull(4), ParentPullNumber: 2, ParentSHA: head}}
		for i := range plans {
			plans[i].Repository.ID = 801
		}
		plans[1].Pull.BaseSHA = head
		front, reason, err := f.e.runFront(ctx, tx, Run{ProjectID: attemptProject, Channel: "hosted"}, 801, plans)
		if err != nil {
			return err
		}
		if front != 4 || reason != "" {
			t.Fatalf("ancestor pause blocked child review: %d %s", front, reason)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func schedulingPull(n int64) reviewprovider.Pull {
	return reviewprovider.Pull{RepositoryID: 801, Number: n, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}
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
	if _, err = g.e.ConfigurePullScheduling(t.Context(), g.scope(t, attemptProject, g.writer, false), "pause-issued", 2, PullSchedulingRequest{ExpectedRevision: &zero, Paused: func() *bool { v := true; return &v }()}); err != nil {
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
func TestSchedulingFinalActionPauseRaceAndAuthorizerRecheck(t *testing.T) {
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
	g := schedulingFixture(t)
	schedulingEnable(t, g, 0, true)
	schedulingGenerate(t, g, true)
	selectionNext(t, g, &refreshTestReader{})
	r := schedulingLatest(t, g)
	claimed := g.start(t, attemptProject, Receipt{ID: r.OperationID})
	schedulingEnable(t, g, 1, false)
	_, err = g.e.ExecutionContext(t.Context(), attemptProject, r.OperationID)
	attemptCode(t, err, "automation_authorization_changed")
	// The exact automatic child is still retained, not cancelled/replaced.
	current, err := g.e.ReadOperation(t.Context(), selectionScope(t, g, attemptProject, g.writer, "read"), r.OperationID)
	if err != nil || current.AttemptID != claimed.ID || current.State != "preparing" {
		t.Fatal(current, err)
	}
}
func TestSchedulingProjectionRefreshDoesNotRegenerateAndValidatorPreservesRevocation(t *testing.T) {
	f := frontFixture(t)
	one := int64(1)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "projected-one-pr", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `UPDATE grants SET can_manage=1 WHERE credential_id='exec-writer-cred'`)
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	r := schedulingLatest(t, f)
	_, err := f.e.ControlOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "cancel-projected", r.OperationID, OperationControl{ExpectedRevision: 1, Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	schedulingGenerate(t, f, false)
	// Refreshing a source timestamp, or another projection revision, is not a
	// deliberate new review intent. Source facts remain the same exact values.
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		c, _, err := readConfiguration(ctx, tx, attemptProject)
		if err != nil {
			return err
		}
		before, _, err := automaticPlanningBasis(ctx, tx, attemptProject, c, "")
		if err != nil {
			return err
		}
		frame, found, err := readFrontFrame(ctx, tx, attemptProject, "hosted")
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("missing genuine source frame")
		}
		at := stamp()
		for i := range frame.prefix {
			frame.prefix[i].ObservedAt = at
		}
		body, err := json.Marshal(frame.prefix)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE review_front_frames SET checked_at=?,source_observed_at=?,prefix_json=? WHERE project_id=?`, at, at, string(body), attemptProject); err != nil {
			return err
		}
		after, _, err := automaticPlanningBasis(ctx, tx, attemptProject, c, "")
		if err != nil {
			return err
		}
		if before != after {
			t.Fatal("timestamp changed semantic basis")
		}
		return ValidateSchedulingSnapshot(ctx, tx)
	})
	if err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	if err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error { return ValidateSchedulingSnapshot(ctx, tx) }); err != nil {
		t.Fatal("valid revoked history refused", err)
	}
	schedulingGenerate(t, f, false)
}
func TestSchedulingAutomaticIndependentProgressAcrossRetainedEvidence(t *testing.T) {
	for _, dependent := range []bool{false, true} {
		t.Run(map[bool]string{false: "independent", true: "dependent"}[dependent], func(t *testing.T) {
			f, a, capture := hostedPublicationFixture(t, false)
			admissionSQL(t, f, `INSERT INTO workers VALUES(?,'selection-worker','Selector')`, attemptProject)
			admissionSQL(t, f, `UPDATE grants SET can_manage=1 WHERE credential_id='exec-writer-cred'`)
			if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, capture); err != nil {
				t.Fatal(err)
			}
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "captured_native_result", a.ExternalID); err != nil {
				t.Fatal(err)
			}
			if dependent {
				one := int64(1)
				_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "stacked", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			schedulingEnable(t, f, 0, true)
			schedulingGenerate(t, f, true)
			reader := &refreshTestReader{}
			if dependent {
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
			}
			selectionNext(t, f, reader)
			r := schedulingLatest(t, f)
			if r.State != "selected" {
				t.Fatal("ancestor evidence blocked a distinct review target", r)
			}
			op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), r.OperationID)
			if err != nil || op.PullNumber != 4 {
				t.Fatal(op, err)
			}
			schedulingGenerate(t, f, false)
		})
	}
}
func TestSchedulingSnapshotRejectsForgedAttribution(t *testing.T) {
	for _, sqlText := range []string{
		`UPDATE review_scheduling SET credential_id='missing-credential'`,
		`UPDATE review_scheduling SET worker_id='foreign-worker'`,
		`UPDATE review_automatic_selections SET credential_id='exec-reader-cred'`,
		`UPDATE review_automatic_selections SET actor_id='exec-reader-principal'`,
		`UPDATE review_automatic_selections SET pull_number=4`,
		`UPDATE review_automatic_selections SET settled_basis_sha256='not-a-digest'`,
	} {
		t.Run(sqlText, func(t *testing.T) {
			f := schedulingFixture(t)
			schedulingEnable(t, f, 0, true)
			schedulingGenerate(t, f, true)
			selectionNext(t, f, &refreshTestReader{})
			admissionSQL(t, f, sqlText)
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error { return ValidateSchedulingSnapshot(ctx, tx) })
			attemptCode(t, err, "storage_unavailable")
		})
	}
}
func TestSchedulingCancelledExactIntentLetsIndependentPeerProceed(t *testing.T) {
	f := schedulingFixture(t)
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	first := schedulingLatest(t, f)
	_, err := f.e.ControlOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "cancel-first-only", first.OperationID, OperationControl{ExpectedRevision: 1, Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	second := schedulingLatest(t, f)
	op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), second.OperationID)
	if err != nil || op.PullNumber != 4 || second.ID == first.ID {
		t.Fatal("independent peer did not progress", second, op, err)
	}
	old, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), first.OperationID)
	if err != nil || old.State != "cancelled" {
		t.Fatal("cancelled intent was replaced", old, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE pull_number=2`) != 1 {
		t.Fatal("same intent recreated")
	}
}
func TestSchedulingAutomaticUsesExistingWorkerLifecycle(t *testing.T) {
	f := schedulingFixture(t)
	schedulingEnable(t, f, 0, true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.e.StartReviewRuns(ctx, map[string]TargetReader{"test-capability": &refreshTestReader{}})
	if err != nil {
		t.Fatal(err)
	}
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("selection worker did not join")
			}
		}
	}()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var selected int
		if err = f.store.DB().QueryRow(`SELECT count(*) FROM review_selections WHERE state='selected' AND channel='hosted'`).Scan(&selected); err != nil {
			t.Fatal(err)
		}
		if selected == 1 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("automatic coordinator never handed off to normal selection worker")
		case <-tick.C:
		}
	}
	cancel()
	select {
	case err = <-done:
		joined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("selection worker leaked")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatal("automatic selection created duplicate child or bypassed supervisor")
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
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, false)
	if attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("paused PRs created automatic intent")
	}
	schedulingPause(t, f, 4, 1, false)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	r := schedulingLatest(t, f)
	op, err := f.e.ReadOperation(t.Context(), scope, r.OperationID)
	if err != nil || op.PullNumber != 4 {
		t.Fatal(op, err)
	}
}

func TestSchedulingCancelledHostedIgnoresCleanCLIRound(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	r := schedulingLatest(t, f)
	_, err := f.e.ControlOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "cancel-before-cli", r.OperationID, OperationControl{ExpectedRevision: 1, Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	a, id := roundCapture(t, f, 74, false)
	roundFinalize(t, f, a, id, 74, false)
	schedulingGenerate(t, f, false)
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE channel='hosted'`) != 1 {
		t.Fatal("clean CLI round recreated cancelled Hosted intent")
	}
}
func schedulingCleanHosted(t *testing.T, f *attemptFixture, revision, n int64) {
	t.Helper()
	r := f.enqueue(t, attemptProject, core.NewID("hosted-clean"), "hosted", 2, revision)
	a := f.start(t, attemptProject, r)
	anchor := attemptAnchor(attemptProject, 2)
	anchor.ParentSHA = anchor.BaseSHA
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	external := strconv.FormatInt(100+n, 10)
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "synthetic_original_post", external)
	if err != nil {
		t.Fatal(err)
	}
	h, _, parsed := hostedEvidenceMetadataFixture()
	h.PublicationID = fmt.Sprintf("%08x-1111-1111-1111-111111111111", n)
	h.ProviderAttemptID = fmt.Sprintf("%08x-2222-2222-2222-222222222222", n)
	h.TriggerCommentID = 100 + n
	h.ReviewIDs = []int64{200 + n}
	h.ObservedBaseSHA = anchor.ParentSHA
	h.Sources = []HostedFindingSource{}
	parsed.Findings = []reviewprovider.ParsedFinding{}
	parsed.ReportedFindings = 0
	digest := fmt.Sprintf("%064x", n+200)
	in := ReviewEvidenceInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024, StartedAt: stamp(), FinishedAt: stamp(), ExpectedAnchor: anchor, Review: parsed, Hosted: h}
	captured, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "synthetic_original_complete", external)
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	_, err = f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), core.NewID("finalize"), captured.ID, FinalizeReviewRequest{ExpectedRevision: &zero})
	if err != nil {
		t.Fatal(err)
	}
	roundCompleteHook(t, f, a.OperationID)
}
func schedulingStackReader() *refreshTestReader {
	r := &refreshTestReader{}
	r.pull = func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		p := schedulingPull(n)
		p.HeadRef = "branch-two"
		if n == 4 {
			p.HeadSHA = strings.Repeat("e", 40)
			p.HeadRef = "branch-four"
			p.BaseRef = "branch-two"
			p.BaseSHA = strings.Repeat("a", 40)
		}
		return p, nil
	}
	r.parents = map[string]reviewprovider.Pull{"branch-two": func() reviewprovider.Pull { p := schedulingPull(2); p.HeadRef = "branch-two"; return p }()}
	return r
}
func TestSchedulingAncestorRoundChangesKeepOriginalChild(t *testing.T) {
	f := schedulingFixture(t)
	schedulingCleanHosted(t, f, 1, 81)
	one := int64(1)
	_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "stack-for-dependency", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}})
	if err != nil {
		t.Fatal(err)
	}
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	reader := schedulingStackReader()
	selectionNext(t, f, reader)
	first := schedulingLatest(t, f)
	child, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), first.OperationID)
	if err != nil || child.PullNumber != 4 {
		t.Fatal("fixture did not select dependent child", first, child, err)
	}
	// Parent is deliberately reopened after selection but before final dispatch.
	// Its comparison and B's binding stay unchanged throughout this scenario.
	roundGrant(t, f, 2, "hosted", 0, 1, 2, "parent-needs-one-more")
	retainHistoricalRoundTaper(t, f, 2, "hosted")
	a := f.start(t, attemptProject, Receipt{ID: child.ID})
	anchor := attemptAnchor(attemptProject, 4)
	anchor.CandidateSHA = strings.Repeat("e", 40)
	anchor.BaseSHA = strings.Repeat("a", 40)
	anchor.ParentSHA = anchor.BaseSHA
	anchor.ParentPullNumber = 2
	anchor.BaseRef = "branch-two"
	parent := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: schedulingPull(2), ParentSHA: strings.Repeat("b", 40), ObservedAt: stamp()}
	parent.Pull.HeadRef = "branch-two"
	err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		op, err := readOperation(ctx, tx, attemptProject, child.ID)
		if err != nil {
			return err
		}
		current, err := f.e.currentExecutionContext(ctx, tx, op)
		if err != nil {
			return err
		}
		return verifyPredecessors(ctx, tx, attemptProject, current, anchor, []SourcePlan{parent})
	})
	if err != nil {
		t.Fatal("ancestor round work stopped final child source verification", err)
	}
	// Change parent progress after the child has an admitted operation/attempt.
	control := roundRead(t, f, 2, "hosted")
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close-parent-during-child", 2, "hosted", RoundControlRequest{ExpectedRevision: &control.Revision, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	schedulingGenerate(t, f, false)
	var children int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_operations WHERE project_id=? AND pull_number=4 AND channel='hosted'`, attemptProject).Scan(&children); err != nil || children != 1 {
		t.Fatal("ancestor progress created another child request", children, err)
	}
	current, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), child.ID)
	if err != nil || current.ID != child.ID || current.AttemptID != a.ID {
		t.Fatal("ancestor progress replaced the child operation", current, err)
	}
}

func TestSchedulingCancelledChildIgnoresAncestorRoundProgress(t *testing.T) {
	for _, change := range []string{"close", "grant-and-taper", "completed-review"} {
		t.Run(change, func(t *testing.T) {
			f := schedulingFixture(t)
			schedulingCleanHosted(t, f, 1, 81)
			one := int64(1)
			_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "stack-for-cancelled-child", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}})
			if err != nil {
				t.Fatal(err)
			}
			schedulingEnable(t, f, 0, true)
			schedulingGenerate(t, f, true)
			selectionNext(t, f, schedulingStackReader())
			first := schedulingLatest(t, f)
			scope := selectionScope(t, f, attemptProject, f.writer, "write")
			child, err := f.e.ReadOperation(t.Context(), scope, first.OperationID)
			if err != nil || child.PullNumber != 4 {
				t.Fatal("dependent child not selected", child, err)
			}
			if _, err := f.e.ControlOperation(t.Context(), scope, "cancel-dependent-child", child.ID, OperationControl{ExpectedRevision: child.Revision, Action: "cancel"}); err != nil {
				t.Fatal(err)
			}
			// Cancellation removes busy suppression; a changed ancestor digest
			// would now create a replacement selection for this same child.
			childBasis := func() string {
				var basis string
				if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					var err error
					basis, err = automaticPullBasisChannel(ctx, tx, attemptProject, 4, "hosted")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return basis
			}
			before := childBasis()
			switch change {
			case "close":
				control := roundRead(t, f, 2, "hosted")
				if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close-parent-after-child", 2, "hosted", RoundControlRequest{ExpectedRevision: &control.Revision, Action: "close"}); err != nil {
					t.Fatal(err)
				}
			case "grant-and-taper":
				roundGrant(t, f, 2, "hosted", 0, 1, 2, "parent-reopened-after-child")
				retainHistoricalRoundTaper(t, f, 2, "hosted")
			case "completed-review":
				roundGrant(t, f, 2, "hosted", 0, 1, 2, "parent-reopened-after-child")
				schedulingCleanHosted(t, f, 2, 82)
				control := roundRead(t, f, 2, "hosted")
				if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close-completed-parent", 2, "hosted", RoundControlRequest{ExpectedRevision: &control.Revision, Action: "close"}); err != nil {
					t.Fatal(err)
				}
			}
			if after := childBasis(); after != before {
				t.Fatal("ancestor progress changed terminal child's basis", before, after)
			}
			// The parent's own changed basis may justify a planning pass. Its
			// source read must not recreate the cancelled child.
			schedulingGenerate(t, f, true)
			selectionNext(t, f, schedulingStackReader())
			if count := attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE pull_number=4 AND channel='hosted'`); count != 1 {
				t.Fatal("ancestor progress recreated terminal child operation", count)
			}
			for range 3 {
				schedulingGenerate(t, f, false)
			}
			current, err := f.e.ReadOperation(t.Context(), scope, child.ID)
			if err != nil || current.State != "cancelled" {
				t.Fatal("cancelled child identity changed", current, err)
			}
		})
	}
}

func TestSchedulingPrepareControlRaceFinalVerificationKeepsSupervisorHealthy(t *testing.T) {
	for _, change := range []string{"pause", "disable", "withdraw"} {
		t.Run(change, func(t *testing.T) {
			f := schedulingFixture(t)
			schedulingEnable(t, f, 0, true)
			schedulingGenerate(t, f, true)
			selectionNext(t, f, &refreshTestReader{})
			r := schedulingLatest(t, f)
			cli := f.enqueue(t, attemptProject, "independent-cli-after-control-race", "cli", 4, 1)
			externalHosted := 0
			completedCLI := 0
			verified := false
			driver := &controlledDriver{}
			driver.prepare = func(ctx context.Context, o Operation, _ Configuration, a Attempt) (Preparation, error) {
				anchor := attemptAnchor(o.ProjectID, o.PullNumber)
				anchor.ParentSHA = anchor.BaseSHA
				if o.Channel == "cli" {
					return Preparation{Anchor: anchor}, nil
				}
				// This exact control mutation commits after the supervisor claimed and
				// prepared the original operation, before it admits/dispatches that action.
				switch change {
				case "pause":
					schedulingPause(t, f, 2, 0, true)
				case "disable":
					schedulingEnable(t, f, 1, false)
				case "withdraw":
					admissionSQL(t, f, `UPDATE grants SET can_manage=0 WHERE credential_id='exec-writer-cred'`)
				}
				intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: o.ID, AttemptID: a.ID, HeadSHA: o.HeadSHA})
				return Preparation{Anchor: anchor, Intent: intent}, err
			}
			driver.dispatch = func(ctx context.Context, o Operation, a Attempt, _ func(context.Context, string) error) (Result, error) {
				if o.Channel == "cli" {
					completedCLI++
					return Result{State: "completed", Reason: "synthetic_independent_result"}, nil
				}
				err := f.e.VerifyAction(ctx, o.ProjectID, a.ID, a.Revision)
				expected := "automation_authorization_changed"
				if change == "pause" {
					expected = "pull_scheduling_paused"
				}
				if err != nil {
					attemptCode(t, err, expected)
					verified = true
					return Result{State: "failed", Reason: "admission_changed"}, nil
				}
				externalHosted++
				return Result{State: "failed", Reason: "synthetic_unexpected_external_action"}, nil
			}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal("control race stopped supervisor", err)
			}
			if verified || externalHosted != 0 || driver.prepares.Load() != 1 {
				t.Fatal("admission failed to refuse before external dispatch", verified, externalHosted)
			}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal("independent CLI work could not continue", err)
			}
			scope := selectionScope(t, f, attemptProject, f.writer, "read")
			hosted, err := f.e.ReadOperation(t.Context(), scope, r.OperationID)
			expected := "automation_authorization_changed"
			if change == "pause" {
				expected = "pull_scheduling_paused"
			}
			if err != nil || hosted.State != "queued" || hosted.Reason != expected || hosted.AttemptID != "" || hosted.ID != r.OperationID {
				t.Fatal("refused preparation lost queued identity", hosted, err)
			}
			other, err := f.e.ReadOperation(t.Context(), scope, cli.ID)
			if err != nil || other.State != "completed" || completedCLI != 1 {
				t.Fatal("sibling did not continue", other, completedCLI, err)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("aborted dispatch invented evidence/credit or leaked reservation")
			}
		})
	}
}

func frontFixture(t *testing.T) *attemptFixture {
	t.Helper()
	f := attemptProjectionFixture(t, true)
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, attemptProject)
	return f
}
func frontMergedPull() reviews.PullInput { p := projectionPull(1); p.State = "merged"; return p }

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
					assessmentPolicy(t, f, 1, []string{"cli", "hosted"}, []string{channel})
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

func TestSchedulingPreparationGuardRetainsOperation(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, change := range []string{"pause", "disable", "withdraw"} {
			t.Run(channel+"/"+change, func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2})
				on := true
				revision := int64(0)
				scope := selectionScope(t, f, attemptProject, f.writer, "write")
				_, err := f.e.ConfigureScheduling(t.Context(), scope, "enable-both", SchedulingRequest{ExpectedRevision: &revision, AutomaticHosted: &on, AutomaticCLI: &on})
				if err != nil {
					t.Fatal(err)
				}
				// Select the requested automatic channel through the normal scheduler.
				readers := map[string]TargetReader{"test-capability": &refreshTestReader{}}
				if channel == "cli" {
					_, err = f.e.generateAutomaticCLI(t.Context(), readers)
				} else {
					_, err = f.e.generateAutomaticHosted(t.Context(), readers)
				}
				if err != nil {
					t.Fatal(err)
				}
				selectionNext(t, f, &refreshTestReader{})
				r := schedulingLatest(t, f)
				if r.OperationID == "" || r.Channel != channel {
					t.Fatal(r)
				}
				deadline := ""
				admissionSQL(t, f, `INSERT INTO review_operation_waits(operation_id,baseline_attempt_id,retry_not_before,paused) VALUES(?,NULL,?,0)`, r.OperationID, deadline)
				before, err := f.e.ReadOperation(t.Context(), scope, r.OperationID)
				if err != nil {
					t.Fatal(err)
				}
				expected := "automation_authorization_changed"
				if change == "pause" {
					expected = "pull_scheduling_paused"
				}
				var original Attempt
				driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, _ Configuration, a Attempt) (Preparation, error) {
					original = a
					switch change {
					case "pause":
						schedulingPause(t, f, 2, 0, true)
					case "disable":
						off, rev := false, int64(1)
						_, err := f.e.ConfigureScheduling(ctx, scope, "disable-both", SchedulingRequest{ExpectedRevision: &rev, AutomaticHosted: &off, AutomaticCLI: &off})
						if err != nil {
							t.Fatal(err)
						}
					case "withdraw":
						admissionSQL(t, f, `UPDATE grants SET can_manage=0 WHERE credential_id='exec-writer-cred'`)
					}
					_, err := f.e.ExecutionContext(ctx, o.ProjectID, o.ID)
					attemptCode(t, err, expected)
					return Preparation{}, err
				}}
				if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
					t.Fatal(err)
				}
				after, err := f.e.ReadOperation(t.Context(), scope, r.OperationID)
				if err != nil {
					t.Fatal(err)
				}
				if after.State != "queued" || after.Reason != expected || after.AttemptID != "" {
					t.Fatalf("guard lost retained request: %+v", after)
				}
				if after.ID != before.ID || after.ActorID != before.ActorID || after.WorkerID != before.WorkerID || after.HeadSHA != before.HeadSHA || after.ConfigurationRevision != before.ConfigurationRevision || !reflect.DeepEqual(after.Wait, before.Wait) {
					t.Fatal("changed original binding or wait", before, after)
				}
				if driver.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
					t.Fatal("dispatched or leaked reservation")
				}
				var state, reason string
				if err := f.store.DB().QueryRow(`SELECT state,reason FROM review_execution_transitions WHERE attempt_id=? AND revision=2`, original.ID).Scan(&state, &reason); err != nil || state != "held" || reason != expected {
					t.Fatal(state, reason, err)
				}
				if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil || driver.prepares.Load() != 1 {
					t.Fatal("blocked operation prepared again", err)
				}
				if change == "pause" {
					schedulingPause(t, f, 2, 1, false)
					retry, err := f.e.StartAttempt(t.Context(), attemptProject, after.ID)
					if err != nil || retry.ID == "" || retry.ID == original.ID || retry.OperationID != before.ID {
						t.Fatal("same operation could not resume", retry, err)
					}
				}
			})
		}
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
				deadline := ""

				admissionSQL(t, f, `INSERT INTO review_operation_waits(operation_id,baseline_attempt_id,retry_not_before,paused) VALUES(?,NULL,?,0)`, r.ID, deadline)
				if wait == "draft" {
					admissionSQL(t, f, `UPDATE review_operations SET reason=? WHERE id=?`, cliDraftWait, r.ID)
				}
				schedulingPause(t, f, pull, 0, true)
				check := func(want string) {
					t.Helper()
					if got := f.e.queuedDiagnostic(t.Context(), scope, r.ID, r.Revision); got != want {
						t.Fatalf("diagnostic=%q want=%q", got, want)
					}
				}
				check("pull_scheduling_paused")
				admissionSQL(t, f, `UPDATE review_execution_config SET body_json=json_set(body_json,'$.paused',json('true')) WHERE project_id=?`, attemptProject)
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
				_, err := f.e.ConfigurePullScheduling(t.Context(), scope, "timer-pause-"+time.Now().Format("150405.000000000"), 2, PullSchedulingRequest{ExpectedRevision: &revision, Paused: &paused})
				if err != nil {
					t.Fatal(err)
				}
			}
			before := cliWaitRead(t, f, a.OperationID)
			pause(0, true)
			if got := f.e.queuedDiagnostic(t.Context(), scope, before.ID, before.Revision); got != "pull_scheduling_paused" {
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

func TestValidateSchedulingSnapshotAcceptsSupportedVersions(t *testing.T) {
	for _, tc := range []struct {
		name           string
		version        string
		legacyPhysical bool
	}{
		{name: "legacy-v1", version: "1", legacyPhysical: true},
		{name: "current-v2", version: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := schedulingFixture(t)
			schedulingEnable(t, f, 0, true)
			if tc.legacyPhysical {
				admissionSQL(t, f, `ALTER TABLE review_scheduling DROP COLUMN automatic_cli`)
			}
			admissionSQL(t, f, `UPDATE metadata SET value=? WHERE key='schema:review_scheduling'`, tc.version)
			if err := validateSchedulingSnapshotForTest(t, f); err != nil {
				t.Fatalf("valid schema version %s rejected: %v", tc.version, err)
			}
		})
	}
}

func TestValidateSchedulingSnapshotRejectsMalformedAndFutureVersions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
	}{
		{name: "malformed", version: "not-a-version"},
		{name: "future", version: "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := schedulingFixture(t)
			admissionSQL(t, f, `UPDATE metadata SET value=? WHERE key='schema:review_scheduling'`, tc.version)
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
