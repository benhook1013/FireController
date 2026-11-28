package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func runHTTPCall(t *testing.T, f *attemptFixture, handler http.Handler, method, path, body, key, wait string, waitVersion ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+f.writer)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	r.Header.Set("X-FC-Worker", "selection-worker")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if wait != "" {
		r.Header.Set("Prefer", wait)
	}
	if len(waitVersion) > 0 {
		r.Header.Set(operationWaitHeader, waitVersion[0])
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestRunHTTPWaitNegotiationVary(t *testing.T) {
	f := selectionFixture(t)
	mux := http.NewServeMux()
	f.e.Register(mux)
	path := "/api/v1/projects/" + attemptProject + "/reviews/selections"
	versions := []string{"", "1", "2", "3", "4", "5", "unsupported"}
	assertVary := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		vary := strings.Split(strings.Join(w.Header().Values("Vary"), ","), ",")
		for _, header := range []string{"Prefer", operationWaitHeader} {
			if !slices.ContainsFunc(vary, func(field string) bool {
				return strings.EqualFold(strings.TrimSpace(field), header)
			}) {
				t.Errorf("missing Vary: %s in %v", header, w.Header())
			}
		}
	}
	var original []byte
	for _, version := range versions {
		t.Run("POST/"+version, func(t *testing.T) {
			w := runHTTPCall(t, f, mux, "POST", path, `{"channel":"cli"}`, "negotiated-run", "wait=0", version)
			if w.Code != http.StatusAccepted {
				t.Fatal("selection not accepted", w.Code, w.Body.String())
			}
			assertVary(t, w)
			if original == nil {
				original = append([]byte(nil), w.Body.Bytes()...)
			} else if !bytes.Equal(original, w.Body.Bytes()) {
				t.Fatal("negotiation changed original receipt")
			}
		})
	}
	var receipt Receipt
	if err := json.Unmarshal(original, &receipt); err != nil {
		t.Fatal(err)
	}
	// Select the later independent PR while the front PR is a draft, then
	// accept a front-PR intent before reading the original selected child.
	selectionNext(t, f, &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		draft := n == 2
		return reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", IsDraft: &draft, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}})
	f.enqueue(t, attemptProject, "front-run-intent", "cli", 2, 1)
	before := selectionCounts(t, f)
	for _, version := range versions {
		t.Run("GET/"+version, func(t *testing.T) {
			w := runHTTPCall(t, f, mux, "GET", path+"/"+receipt.ID, "", "", "wait=0", version)
			var state RunState
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil || state.Selection.ID != receipt.ID || state.Operation == nil || state.Operation.PullNumber != 4 || state.Operation.State != "queued" {
				t.Fatal("original selected child unavailable", w.Code, w.Body.String())
			}
			assertVary(t, w)
			want := "awaiting_scheduler"
			if version == "2" || version == "3" || version == "4" || version == "5" {
				want = "queue_order"
			}
			if got := w.Header().Get(queuedDiagnosticHeader); got != want {
				t.Fatalf("diagnostic=%q, want %q", got, want)
			}
		})
	}
	if selectionCounts(t, f) != before {
		t.Fatal("negotiated reads changed durable state")
	}
}

func TestRunHTTPStrictOptionalAssertionsAndOriginalReceipt(t *testing.T) {
	f := selectionFixture(t)
	mux := http.NewServeMux()
	f.e.Register(mux)
	path := "/api/v1/projects/" + attemptProject + "/reviews/selections"
	before := selectionCounts(t, f)
	for _, body := range []string{
		`{"channel":"cli","expected_pull_number":null}`,
		`{"channel":"cli","expected_configuration_revision":null}`,
		`{"channel":"cli","expected_pull_number":2.5}`,
		`{"channel":"cli","channel":"hosted"}`,
		`{"channel":"cli","worker_id":"other"}`,
	} {
		w := runHTTPCall(t, f, mux, "POST", path, body, "strict-once", "wait=0")
		if w.Code != 400 || selectionCounts(t, f) != before {
			t.Fatal("invalid selection changed durable state", w.Code)
		}
	}
	for _, wait := range []string{"wait=-1", "wait=901", "wait=1.5", "wait=1, wait=2"} {
		w := runHTTPCall(t, f, mux, "POST", path, `{"channel":"cli"}`, "strict-once", wait)
		if w.Code != 400 || selectionCounts(t, f) != before {
			t.Fatal("invalid wait accepted work", w.Code)
		}
	}
	first := runHTTPCall(t, f, mux, "POST", path, `{"channel":"cli"}`, "strict-once", "wait=0")
	if first.Code != 202 {
		t.Fatal("local acceptance failed", first.Code, first.Body.String())
	}
	var receipt Receipt
	if json.Unmarshal(first.Body.Bytes(), &receipt) != nil || !validRunID(receipt.ID) {
		t.Fatal("invalid original receipt")
	}
	after := selectionCounts(t, f)
	replay := runHTTPCall(t, f, mux, "POST", path, `{"channel":"cli"}`, "strict-once", "wait=1")
	if replay.Code != 202 || !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) || after != selectionCounts(t, f) {
		t.Fatal("wait preference changed accepted action", replay.Code)
	}
}

func TestRunHTTPWaitSharesBudgetAcrossSelectionAndExecution(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "two-stage-wait", "cli", nil)
	mux := http.NewServeMux()
	f.e.Register(mux)
	read := "/api/v1/projects/" + attemptProject + "/reviews/selections/" + receipt.ID
	// Selection consumes part of a two-second wait. Its queued child gets
	// only the remaining budget. Leave time for race-instrumented SQL.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := &refreshTestReader{repository: func(ctx context.Context, id int64) (reviewprovider.Repository, error) {
		select {
		case <-time.After(time.Second):
			return reviewprovider.Repository{ID: id, FullName: "owner/repo", DefaultBranch: "main"}, nil
		case <-ctx.Done():
			return reviewprovider.Repository{}, ctx.Err()
		}
	}}
	type response struct {
		recorder *httptest.ResponseRecorder
		elapsed  time.Duration
	}
	done := make(chan response, 1)
	go func() {
		started := time.Now()
		w := runHTTPCall(t, f, mux, "GET", read, "", "", "wait=2")
		done <- response{w, time.Since(started)}
	}()
	operationWaitRegistered(t, f.e, 1)
	if _, err := f.e.runNext(ctx, map[string]TargetReader{"test-capability": reader}); err != nil {
		t.Fatal(err)
	}
	var w *httptest.ResponseRecorder
	select {
	case result := <-done:
		w = result.recorder
		if result.elapsed > 2750*time.Millisecond {
			t.Fatal("each stage allocated a separate wait", result.elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("combined wait did not finish")
	}
	var state RunState
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &state) != nil || state.Selection.State != "selected" || state.Operation == nil || state.Operation.State != "queued" {
		t.Fatal("budget expiry was not a useful pending read", w.Code, w.Body.String())
	}
	before := selectionCounts(t, f)
	a, err := f.e.StartAttempt(t.Context(), attemptProject, state.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "failed", "synthetic_terminal", ""); err != nil {
		t.Fatal(err)
	}
	if selectionCounts(t, f) == before {
		t.Fatal("terminal journal did not change")
	}
	terminal := runHTTPCall(t, f, mux, "POST", "/api/v1/projects/"+attemptProject+"/reviews/selections", `{"channel":"cli"}`, "two-stage-wait", "wait=900")
	var result RunState
	if terminal.Code != 200 || json.Unmarshal(terminal.Body.Bytes(), &result) != nil || result.Selection.ID != receipt.ID || result.Operation == nil || result.Operation.ID != state.Operation.ID || result.Operation.State != "failed" {
		t.Fatal("original key did not follow the original operation", terminal.Code, terminal.Body.String())
	}
	var n int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_operations`).Scan(&n); err != nil || n != 1 {
		t.Fatal("wait/replay created another action", n, err)
	}
}

// Exercise a real socket deadline: an optional hint must not turn a valid
// retained response into EOF when the read pool is occupied.
func TestQueuedRunDiagnosticPreservesNetworkResponseUnderContention(t *testing.T) {
	f := selectionFixture(t)
	scope := f.scope(t, attemptProject, f.writer, false)
	f.store.DB().SetMaxOpenConns(1)
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Model the remaining budget after required selection and operation reads.
		deadline := time.Now().Add(750 * time.Millisecond)
		if err := operationResponseDeadlineAt(w, deadline); err != nil {
			t.Error(err)
			return
		}
		e := f.e
		e.queuedRunResponseHeader(w, r, scope, RunState{Operation: &Operation{ID: "review_operation_test", State: "queued", Channel: "cli", Revision: 1}}, deadline)
		w.Write([]byte("retained response"))
	}))
	defer func() {
		// Release the held connection before waiting for the handler to exit.
		conn.Close()
		server.Close()
	}()
	// Bound dial, response headers and body reads without shortening the handler's budget.
	client := *server.Client()
	client.Timeout = 2 * time.Second
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal("optional diagnostic lost response", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "retained response" {
		t.Fatal(string(body), err)
	}
	if response.Header.Get(queuedDiagnosticHeader) != "" {
		t.Fatal("unavailable diagnostic must be omitted")
	}
}

func TestQueuedRunDiagnosticOmitsHintBeforeRetainedResponseDeadline(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "queued-diagnostic-deadline", "cli", nil)
	selectionNext(t, f, &refreshTestReader{})
	scope := selectionScope(t, f, attemptProject, f.writer, "read")
	state, terminal, err := f.e.readRunState(t.Context(), scope, receipt.ID)
	if err != nil || terminal || state.Operation == nil || state.Operation.State != "queued" {
		t.Fatalf("expected the original pending queued child: %+v (%v)", state, err)
	}

	// A normal hint proves this fixture is otherwise eligible for diagnostics.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	f.e.queuedRunResponseHeader(w, req, scope, state, time.Now().Add(time.Second))
	if w.Header().Get(queuedDiagnosticHeader) == "" {
		t.Fatal("eligible queued diagnostic missing with ample budget")
	}

	// A lawful retained response has already used most of its write budget.
	// The optional diagnostic must not wait for the reserved reader connection.
	f.store.DB().SetMaxOpenConns(1)
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	before := f.store.DB().Stats().WaitCount
	w = httptest.NewRecorder()
	deadline := time.Now().Add(queuedDiagnosticBudget / 2)
	f.e.queuedRunResponseHeader(w, req, scope, state, deadline)
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(state); err != nil {
		t.Fatal(err)
	}
	var result RunState
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Selection.ID != receipt.ID || result.Operation == nil || result.Operation.ID != state.Operation.ID || w.Code != http.StatusOK {
		t.Fatalf("could not return original retained child: %s (%v)", w.Body, err)
	}
	if w.Header().Get(queuedDiagnosticHeader) != "" || f.store.DB().Stats().WaitCount != before {
		t.Fatal("optional diagnostic should be omitted without trying reader admission")
	}
}

func TestRunHTTPProjectsSelectedCLIWait(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "selected-cli-wait", "cli", nil)
	selectionNext(t, f, &refreshTestReader{})
	a := f.start(t, attemptProject, Receipt{ID: selectionRead(t, f, receipt.ID).OperationID})
	anchor := attemptAnchor(attemptProject, 2)
	in := CLIAllowanceObservation{Anchor: anchor, ObservedAt: stamp(), Remaining: 0, Limit: 3, FullCapacityInMS: 86400000, RollingWindowMS: 86400000, ChangedFiles: 2}
	if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	original := cliWaitRead(t, f, a.OperationID).Wait
	if original == nil || original.CLIAllowance == nil || original.RetryNotBefore == "" || original.BaselineAttemptID == "" {
		t.Fatal("fixture did not retain a CLI quota wait", original)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	read := "/api/v1/projects/" + attemptProject + "/reviews/selections/" + receipt.ID
	before := selectionCounts(t, f)
	for _, tc := range []struct {
		version                           string
		hasWait, hasBaseline, hasEstimate bool
	}{
		{"", false, false, false},
		{"1", true, false, false},
		{"2", true, true, false},
		{"3", true, true, true},
		{"5", true, true, true},
		{"unsupported", false, false, false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			w := runHTTPCall(t, f, mux, "GET", read, "", "", "wait=0", tc.version)
			var state RunState
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil || state.Selection.ID != receipt.ID || state.Operation == nil || state.Operation.ID != a.OperationID {
				t.Fatal("selected CLI operation unavailable", w.Code, w.Body.String())
			}
			wait := state.Operation.Wait
			if (wait != nil) != tc.hasWait {
				t.Fatal("incorrect wait projection", w.Body.String())
			}
			if wait == nil {
				return
			}
			if (wait.BaselineAttemptID != "") != tc.hasBaseline || (wait.RetryNotBefore != "") != tc.hasBaseline || (wait.CLIAllowance != nil) != tc.hasEstimate {
				t.Fatal("incorrect wait detail projection", w.Body.String())
			}
			if tc.hasBaseline && (wait.BaselineAttemptID != original.BaselineAttemptID || wait.RetryNotBefore != original.RetryNotBefore) {
				t.Fatal("projection changed retained wait identity", wait)
			}
			if tc.hasEstimate && (wait.CLIAllowance.ObservedAt != original.CLIAllowance.ObservedAt || wait.CLIAllowance.FullCapacityInMS == nil || *wait.CLIAllowance.FullCapacityInMS != in.FullCapacityInMS) {
				t.Fatal("projection changed retained allowance", wait.CLIAllowance)
			}
		})
	}
	if encoded(original) != encoded(cliWaitRead(t, f, a.OperationID).Wait) || selectionCounts(t, f) != before {
		t.Fatal("response projection changed retained state")
	}
}

func TestRunHTTPProjectsHeldSelectionSubmission(t *testing.T) {
	f := selectionFixture(t)
	const key = "held-selection-post"
	receipt, _ := selectionEnqueue(t, f, key, "cli", nil)
	selectionNext(t, f, &refreshTestReader{})
	a := f.start(t, attemptProject, Receipt{ID: selectionRead(t, f, receipt.ID).OperationID})
	in := CLIAllowanceObservation{Anchor: attemptAnchor(attemptProject, 2), ObservedAt: stamp(), Remaining: 0, Limit: 3, FullCapacityInMS: 0, RollingWindowMS: 86400000, ChangedFiles: 2}
	if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "allowance_exhausted", ""); err != nil {
		t.Fatal(err)
	}
	original := cliWaitRead(t, f, a.OperationID)
	if original.Allowance == nil || original.Allowance.FullCapacityInMS == nil || *original.Allowance.FullCapacityInMS != 0 {
		t.Fatal("fixture did not retain a held allowance", original)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	path := "/api/v1/projects/" + attemptProject + "/reviews/selections"
	before := selectionCounts(t, f)
	for _, version := range []string{"", "1", "2", "3", "4", "5", "unsupported"} {
		t.Run(version, func(t *testing.T) {
			w := runHTTPCall(t, f, mux, "POST", path, `{"channel":"cli"}`, key, "wait=1", version)
			var state RunState
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil || state.Selection.ID != receipt.ID || state.Operation == nil || state.Operation.ID != a.OperationID || state.Operation.State != "held" {
				t.Fatal("original held selection unavailable", w.Code, w.Body.String())
			}
			if version == "3" || version == "5" {
				if encoded(state.Operation.Allowance) != encoded(original.Allowance) {
					t.Fatal("negotiated held allowance changed", w.Body.String())
				}
			} else if state.Operation.Allowance != nil || strings.Contains(w.Body.String(), `"allowance":`) {
				t.Fatal("held allowance escaped version negotiation", w.Body.String())
			}
		})
	}
	if encoded(original) != encoded(cliWaitRead(t, f, a.OperationID)) || selectionCounts(t, f) != before {
		t.Fatal("response projection changed retained state or created new work")
	}
}
