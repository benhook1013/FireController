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
	busy := read("reservation_busy")
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
