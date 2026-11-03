package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func runHTTPCall(t *testing.T, f *attemptFixture, handler http.Handler, method, path, body, key, wait string) *httptest.ResponseRecorder {
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
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
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
	// Selection consumes most of a one-second wait; the still-queued child
	// receives the remaining budget rather than another full second.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := &refreshTestReader{repository: func(ctx context.Context, id int64) (reviewprovider.Repository, error) {
		select {
		case <-time.After(800 * time.Millisecond):
			return reviewprovider.Repository{ID: id, FullName: "owner/repo", DefaultBranch: "main"}, nil
		case <-ctx.Done():
			return reviewprovider.Repository{}, ctx.Err()
		}
	}}
	started := time.Now()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- runHTTPCall(t, f, mux, "GET", read, "", "", "wait=1") }()
	operationWaitRegistered(t, f.e, 1)
	if _, err := f.e.runNext(ctx, map[string]TargetReader{"test-capability": reader}); err != nil {
		t.Fatal(err)
	}
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("combined wait did not finish")
	}
	if elapsed := time.Since(started); elapsed > 1600*time.Millisecond {
		t.Fatal("each stage allocated a separate wait", elapsed)
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
