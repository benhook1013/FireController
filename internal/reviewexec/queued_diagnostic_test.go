package reviewexec

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestQueuedDiagnosticPastDueReservationPauseAndPrivacy(t *testing.T) {
	f, a, in, now := queuedCooldown(t)
	*now = in.Observation.RetryNotBefore.Add(time.Second)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	foreign := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "foreign-busy", "hosted", 4, 1))
	later := f.enqueue(t, attemptProject, "later-queued-diag", "hosted", 4, 1)
	laterStatus := statusHTTP(t, f, later.ID, "", "wait=0")
	if laterStatus.Code != 200 || laterStatus.Header().Get(queuedDiagnosticHeader) != "queue_order" {
		t.Fatal("earlier applicable Hosted intent was not distinguished", laterStatus.Code, laterStatus.Header())
	}
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	read := func(want string) *httptest.ResponseRecorder {
		t.Helper()
		before := map[string]string{}
		for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_reservations", "review_execution_config", "review_operation_waits", "receipts", "events"} {
			before[table] = snapshotTable(t, f, table)
		}
		w := statusHTTP(t, f, a.OperationID, "", "wait=0")
		if w.Code != 200 || w.Header().Get(queuedDiagnosticHeader) != want {
			t.Fatal("wrong current queued cause", w.Code, w.Header().Get(queuedDiagnosticHeader), want)
		}
		for _, private := range []string{otherAttemptProject, foreign.ID, "test-account", "coderabbit"} {
			if strings.Contains(w.Body.String(), private) || strings.Contains(w.Header().Get(queuedDiagnosticHeader), private) {
				t.Fatal("foreign identity escaped scheduling hint")
			}
		}
		for table, value := range before {
			if snapshotTable(t, f, table) != value {
				t.Fatal("diagnostic read wrote state", table)
			}
		}
		return w
	}
	// A Hosted holder in a different repository no longer shares capacity.
	busy := read("awaiting_scheduler")
	if busy.Header().Get(queuedReservationHeader) != "" {
		t.Fatal("independent repository acquired a reservation holder")
	}
	var value OperationStatus
	if json.Unmarshal(busy.Body.Bytes(), &value) != nil || value.State != "queued" || value.NextAction != "wait" {
		t.Fatal("hint changed state/action")
	}
	if _, err := f.e.Advance(t.Context(), otherAttemptProject, foreign.ID, foreign.Revision, "held", "controlled_release", ""); err != nil {
		t.Fatal(err)
	}
	free := read("awaiting_scheduler")
	if free.Body.String() != busy.Body.String() {
		t.Fatal("header hint changed legacy JSON wire shape")
	}
	*now = in.Observation.RetryNotBefore.Add(-time.Second)
	read("cooldown")
	laterStatus = statusHTTP(t, f, later.ID, "", "wait=0")
	if laterStatus.Code != 200 || laterStatus.Header().Get(queuedDiagnosticHeader) != "queue_order" {
		t.Fatal("future earlier timer lost queue priority")
	}
	timerControl(t, f, a.OperationID, "pause", "diag-pause")
	read("operation_paused")
	timerControl(t, f, a.OperationID, "resume", "diag-resume")
	f.configure(t, attemptProject, "test-capability", 1, true)
	read("project_paused")
	op, err := f.e.ReadOperation(t.Context(), scope, a.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if f.e.queuedDiagnostic(t.Context(), scope, op.ID, op.Revision-1) != "" {
		t.Fatal("stale response acquired a new cause")
	}
	if f.e.queuedDiagnostic(t.Context(), scope, foreign.OperationID, 1) != "" || f.e.queuedDiagnostic(t.Context(), core.Scope{}, op.ID, op.Revision) != "" {
		t.Fatal("foreign/unauthed read acquired hint")
	}
	timerControl(t, f, a.OperationID, "cancel", "diag-cancel")
	read("")
}

func TestQueuedReservationSameProjectVerifiedBindingAndUnchangedBody(t *testing.T) {
	f, holder, _, allowance := hostedAllowanceFixture(t)
	waiting := f.enqueue(t, attemptProject, "waiting-on-hosted", "hosted", 4, 1)
	read := func(kind, state string) *httptest.ResponseRecorder {
		t.Helper()
		before := selectionCounts(t, f)
		w := statusHTTP(t, f, waiting.ID, "", "wait=0")
		var h queuedReservation
		raw := w.Header().Get(queuedReservationHeader)
		if w.Code != 200 || w.Header().Get(queuedDiagnosticHeader) != "reservation_busy" || len(raw) > 512 || json.Unmarshal([]byte(raw), &h) != nil || h.Kind != kind || selectionCounts(t, f) != before {
			t.Fatal("holder missing or read changed work", w.Code, raw)
		}
		if kind == "same_project" && (h.OperationID != holder.OperationID || h.PullNumber != 2 || h.Channel != "hosted" || h.State != state) {
			t.Fatal("wrong same-project holder", h)
		}
		if kind != "same_project" && h != (queuedReservation{Kind: kind}) {
			t.Fatal("unverified holder identity escaped", h)
		}
		return w
	}
	initial := read("same_project", "preparing")
	if err := f.e.RecordHostedAllowance(t.Context(), attemptProject, holder.ID, holder.Revision, allowance); err != nil {
		t.Fatalf("allowance recording failed: %v; input_valid=%v observed=%s anchor=%s query=%s response=%s now=%s revision=%d", err, validHostedAllowanceInput(allowance), allowance.ObservedAt, allowance.Anchor.ObservedAt, allowance.Observation.QueryCreatedAt, allowance.Observation.ResponseCreatedAt, stamp(), holder.Revision)
	}
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: holder.OperationID, AttemptID: holder.ID, HeadSHA: allowance.Anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	holder, err = f.e.Admit(t.Context(), attemptProject, holder.ID, holder.Revision, allowance.Anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	read("same_project", "posting")
	holder, err = f.e.Advance(t.Context(), attemptProject, holder.ID, holder.Revision, "active", "synthetic_posted", "123")
	if err != nil {
		t.Fatal(err)
	}
	read("same_project", "active")
	// Corrupt only the disposable fixture: no holder identity may be presented
	// when the operation pointer, account or admitted repository disagrees.
	admissionSQL(t, f, `UPDATE review_operations SET attempt_id='' WHERE id=?`, holder.OperationID)
	read("reservation_unproven", "")
	admissionSQL(t, f, `UPDATE review_operations SET attempt_id=? WHERE id=?`, holder.ID, holder.OperationID)
	admissionSQL(t, f, `UPDATE review_execution_attempts SET account='different-account' WHERE id=?`, holder.ID)
	read("reservation_unproven", "")
	admissionSQL(t, f, `UPDATE review_execution_attempts SET account=? WHERE id=?`, holder.Account, holder.ID)
	anchor := allowance.Anchor
	anchor.RepositoryID++
	admissionSQL(t, f, `UPDATE review_execution_attempts SET anchor_json=? WHERE id=?`, encoded(anchor), holder.ID)
	read("reservation_unproven", "")
	admissionSQL(t, f, `UPDATE review_execution_attempts SET anchor_json=? WHERE id=?`, encoded(allowance.Anchor), holder.ID)
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	if cause, h := f.e.queuedDiagnosticDetails(t.Context(), scope, waiting.ID, waiting.Revision+1); cause != "" || h != nil {
		t.Fatal("stale response received current holder")
	}
	if cause, h := f.e.queuedDiagnosticDetails(t.Context(), core.Scope{}, waiting.ID, waiting.Revision); cause != "" || h != nil {
		t.Fatal("unauthorized holder read")
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, holder.ID, holder.Revision, "failed", "controlled_failure", ""); err != nil {
		t.Fatal(err)
	}
	free := statusHTTP(t, f, waiting.ID, "", "wait=0")
	if free.Header().Get(queuedDiagnosticHeader) != "awaiting_scheduler" || free.Header().Get(queuedReservationHeader) != "" || free.Body.String() != initial.Body.String() {
		t.Fatal("reservation diagnostics changed original response body")
	}
}

func TestQueuedReservationCrossProjectCLIPrivacy(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	holder := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "foreign-cli-holder", "cli", 2, 1))
	waiting := f.enqueue(t, attemptProject, "waiting-on-global-cli", "cli", 4, 1)
	w := statusHTTP(t, f, waiting.ID, "", "wait=0")
	if w.Code != 200 || w.Header().Get(queuedDiagnosticHeader) != "reservation_busy" || w.Header().Get(queuedReservationHeader) != `{"kind":"other_project"}` {
		t.Fatal("shared CLI account boundary or privacy lost", w.Code, w.Header())
	}
	for _, private := range []string{otherAttemptProject, holder.ID, holder.OperationID, holder.Account, holder.Provider} {
		if strings.Contains(w.Body.String(), private) || strings.Contains(w.Header().Get(queuedReservationHeader), private) {
			t.Fatal("foreign reservation details escaped")
		}
	}
}

func TestQueuedDiagnosticRequestReceiptFullAndSelectedRun(t *testing.T) {
	f := selectionFixture(t)
	reader := &refreshTestReader{}
	receipt, _ := selectionEnqueue(t, f, "queued-diag-selection", "cli", nil)
	selectionNext(t, f, reader)
	selected := selectionRead(t, f, receipt.ID)
	sourceCalls := reader.calls.Load()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetPathValue("project_id", attemptProject)
	r.SetPathValue("selection_id", receipt.ID)
	r.Header.Set("Prefer", "wait=0")
	r.Header.Set("Authorization", "Bearer "+f.reader)
	w := httptest.NewRecorder()
	f.e.readRunHTTP(w, r)
	if w.Code != 200 || w.Header().Get(queuedDiagnosticHeader) != "awaiting_scheduler" {
		t.Fatal("selected child cause missing", w.Code, w.Header())
	}
	var state RunState
	if json.Unmarshal(w.Body.Bytes(), &state) != nil || state.Operation == nil || state.Operation.ID != selected.OperationID || state.Operation.State != "queued" {
		t.Fatal("selection child changed")
	}
	full := httptest.NewRequest(http.MethodGet, "/", nil)
	full.SetPathValue("project_id", attemptProject)
	full.SetPathValue("operation_id", selected.OperationID)
	full.Header.Set("Prefer", "wait=0")
	full.Header.Set("Authorization", "Bearer "+f.reader)
	before := selectionCounts(t, f)
	out := httptest.NewRecorder()
	f.e.readOperationHTTP(out, full)
	if out.Code != 200 || out.Header().Get(queuedDiagnosticHeader) != "awaiting_scheduler" || selectionCounts(t, f) != before || reader.calls.Load() != sourceCalls {
		t.Fatal("full read changed work or lost hint")
	}
	input := Request{ExpectedConfigurationRevision: 1, PullNumber: 4, Channel: "hosted", HeadSHA: strings.Repeat("a", 40)}
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(encoded(input)))
		r.SetPathValue("project_id", attemptProject)
		r.Header.Set("Authorization", "Bearer "+f.writer)
		r.Header.Set("Idempotency-Key", "queued-diag-original")
		r.Header.Set("Prefer", "wait=0")
		w := httptest.NewRecorder()
		f.e.enqueueHTTP(w, r)
		return w
	}
	first := call()
	if first.Code != 202 || first.Header().Get(queuedDiagnosticHeader) != "awaiting_scheduler" {
		t.Fatal("original queued receipt hint missing", first.Code, first.Header())
	}
	before = selectionCounts(t, f)
	replay := call()
	if replay.Code != 202 || !reflect.DeepEqual(replay.Body.Bytes(), first.Body.Bytes()) || selectionCounts(t, f) != before {
		t.Fatal("hint changed original receipt/replay")
	}
}
