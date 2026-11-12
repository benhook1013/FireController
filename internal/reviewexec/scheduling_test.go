package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
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
				t.Fatal(r)
			}
			op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), r.OperationID)
			if err != nil || op.PullNumber != 4 || op.AllowOutOfOrder {
				t.Fatal("Hosted selected an incorrect target or bypass intent", op, err)
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

func TestSchedulingPrepareControlRaceRefusesBeforeDispatch(t *testing.T) {
	for _, change := range []string{"pause", "disable", "withdraw"} {
		t.Run(change, func(t *testing.T) {
			f := schedulingFixture(t)
			schedulingEnable(t, f, 0, true)
			schedulingGenerate(t, f, true)
			selectionNext(t, f, &refreshTestReader{})
			r := schedulingLatest(t, f)
			externalHosted := 0
			completedCLI := 0
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
				externalHosted++
				return Result{State: "failed", Reason: "synthetic_unexpected_external_action"}, nil
			}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal("control race stopped supervisor", err)
			}
			if externalHosted != 0 {
				t.Fatal("admission sent an external action after a control change", externalHosted)
			}
			cli := f.enqueue(t, attemptProject, "independent-cli-after-control-race", "cli", 4, 1)
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal("independent CLI work could not continue", err)
			}
			scope := selectionScope(t, f, attemptProject, f.writer, "read")
			hosted, err := f.e.ReadOperation(t.Context(), scope, r.OperationID)
			wantReason := "automation_authorization_changed"
			if change == "pause" {
				wantReason = "pull_scheduling_paused"
			}
			if err != nil || hosted.State != "queued" || hosted.AttemptID != "" || hosted.Reason != wantReason {
				t.Fatal(hosted, err)
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
