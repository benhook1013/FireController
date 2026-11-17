package reviewexec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type operationWaitResult struct {
	value    Operation
	terminal bool
	err      error
}

func operationReadScope(t *testing.T, f *attemptFixture) core.Scope {
	t.Helper()
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("project_id", attemptProject)
	r.Header.Set("Authorization", "Bearer "+f.reader)
	scope, err := f.store.Scope(r, "read")
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func operationWaitRegistered(t *testing.T, e *Engine, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e.operationMu.Lock()
		got := e.operationWaiterCount
		e.operationMu.Unlock()
		if got == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("operation wait registration did not reach expected count", count)
}

func startOperationWait(t *testing.T, f *attemptFixture, scope core.Scope, id string, seconds int) (<-chan operationWaitResult, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan operationWaitResult, 1)
	go func() {
		value, terminal, err := f.e.WaitOperation(ctx, scope, id, seconds)
		done <- operationWaitResult{value, terminal, err}
	}()
	return done, cancel
}

func receiveOperationWait(t *testing.T, done <-chan operationWaitResult) operationWaitResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("committed operation change did not wake its wait")
		return operationWaitResult{}
	}
}

func TestOperationWaitCommittedCompletionWithoutProjection(t *testing.T) {
	f := sourceHTTPFixture(t)
	r := f.enqueue(t, attemptProject, "wait-complete", "cli", 2, 1)
	a := f.start(t, attemptProject, r)
	a, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
	if err != nil {
		t.Fatal(err)
	}
	done, _ := startOperationWait(t, f, operationReadScope(t, f), r.ID, 900)
	operationWaitRegistered(t, f.e, 1)
	if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "controlled_completion", ""); err != nil {
		t.Fatal(err)
	}
	result := receiveOperationWait(t, done)
	if result.err != nil || !result.terminal || result.value.State != "completed" || result.value.ID != r.ID {
		t.Fatal(result)
	}
	operationWaitRegistered(t, f.e, 0)
	if f.e.publishAttempt != nil {
		t.Fatal("fixture unexpectedly depends on browser publication")
	}
}

func TestOperationWaitPreAttemptHoldAndRecovery(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "pre-attempt-hold", true: "interrupted-preparation"}[recovery], func(t *testing.T) {
			f := sourceHTTPFixture(t)
			r := f.enqueue(t, attemptProject, "wait-hold", "cli", 2, 1)
			if recovery {
				f.start(t, attemptProject, r)
			} else {
				f.configure(t, attemptProject, "test-capability", 1, true)
			}
			done, _ := startOperationWait(t, f, operationReadScope(t, f), r.ID, 900)
			operationWaitRegistered(t, f.e, 1)
			var err error
			if recovery {
				err = f.e.RecoverInterrupted(t.Context())
			} else {
				var attempt Attempt
				attempt, err = f.e.StartAttempt(t.Context(), attemptProject, r.ID)
				if err != nil || attempt.ID != "" {
					t.Fatal("paused queue created an attempt", attempt, err)
				}
				current, readErr := f.e.ReadOperation(t.Context(), operationReadScope(t, f), r.ID)
				if readErr != nil || current.State != "queued" || current.Revision != 1 || current.AttemptID != "" {
					t.Fatal("pause changed retained queued intent", current, readErr)
				}
				select {
				case result := <-done:
					t.Fatal("pause completed result collection", result)
				default:
				}
				// Resumption preserves this request; a later policy change is a real
				// pre-attempt hold that must wake its existing result collector.
				f.configure(t, attemptProject, "test-capability", 2, false)
				admissionPolicy(t, f, 1)
				_, err = f.e.StartAttempt(t.Context(), attemptProject, r.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			result := receiveOperationWait(t, done)
			want := map[bool]string{false: "held", true: "cancelled"}[recovery]
			if result.err != nil || !result.terminal || result.value.State != want {
				t.Fatal(result)
			}
		})
	}
}

func TestOperationWaitRollbackCancelAndAuthorization(t *testing.T) {
	f := sourceHTTPFixture(t)
	r := f.enqueue(t, attemptProject, "wait-rollback", "cli", 2, 1)
	a := f.start(t, attemptProject, r)
	scope := operationReadScope(t, f)
	done, cancel := startOperationWait(t, f, scope, r.ID, 900)
	operationWaitRegistered(t, f.e, 1)
	admissionSQL(t, f, `CREATE TRIGGER operation_wait_rollback BEFORE INSERT ON review_execution_transitions WHEN NEW.state='failed' BEGIN SELECT RAISE(ABORT,'controlled rollback'); END`)
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "failed", "controlled_failure", ""); err == nil {
		t.Fatal("failed transaction unexpectedly committed")
	}
	select {
	case result := <-done:
		t.Fatal("rollback completed wait", result)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	result := receiveOperationWait(t, done)
	if result.terminal || result.value.ID != "" {
		t.Fatal("canceled wait returned result", result)
	}
	attemptCode(t, result.err, "overloaded")
	operationWaitRegistered(t, f.e, 0)
	current, err := f.e.ReadOperation(t.Context(), scope, r.ID)
	if err != nil || current.State != "preparing" || current.Revision != 2 {
		t.Fatal("cancel or rollback changed operation", current, err)
	}
	foreign := f.scope(t, otherAttemptProject, f.other, false)
	_, terminal, err := f.e.WaitOperation(t.Context(), foreign, r.ID, 900)
	attemptCode(t, err, "not_found")
	if terminal {
		t.Fatal("foreign operation reported terminal")
	}
	done, _ = startOperationWait(t, f, scope, r.ID, 900)
	operationWaitRegistered(t, f.e, 1)
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`)
	if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "cancelled", "controlled_cancel", ""); err != nil {
		t.Fatal(err)
	}
	result = receiveOperationWait(t, done)
	attemptCode(t, result.err, "unauthorized")
	if result.value.ID != "" || result.terminal {
		t.Fatal("revoked wait exposed retained state", result)
	}
}

func TestOperationWaitExpiryCapacityAndTerminalUncertain(t *testing.T) {
	f := sourceHTTPFixture(t)
	r := f.enqueue(t, attemptProject, "wait-expiry", "cli", 2, 1)
	scope := operationReadScope(t, f)
	started := time.Now()
	value, terminal, err := f.e.WaitOperation(t.Context(), scope, r.ID, 1)
	if err != nil || terminal || value.State != "queued" || time.Since(started) < 900*time.Millisecond || time.Since(started) > 2*time.Second {
		t.Fatal(value, terminal, err, time.Since(started))
	}
	var waits []<-chan operationWaitResult
	var cancels []context.CancelFunc
	for i := range 8 {
		done, cancel := startOperationWait(t, f, scope, r.ID, 900)
		waits, cancels = append(waits, done), append(cancels, cancel)
		operationWaitRegistered(t, f.e, i+1)
	}
	_, terminal, err = f.e.WaitOperation(t.Context(), scope, r.ID, 1)
	attemptCode(t, err, "overloaded")
	if terminal {
		t.Fatal("capacity refusal reported terminal")
	}
	// An immediate read consumes no wait capacity.
	if _, _, err = f.e.WaitOperation(t.Context(), scope, r.ID, 0); err != nil {
		t.Fatal(err)
	}
	for _, cancel := range cancels {
		cancel()
	}
	for _, done := range waits {
		attemptCode(t, receiveOperationWait(t, done).err, "overloaded")
	}
	operationWaitRegistered(t, f.e, 0)
	// Simulate slots owned by other principals to exercise the independent
	// global admission ceiling without allocating hundreds of fixture identities.
	f.e.operationMu.Lock()
	f.e.operationWaiterCount = 256
	f.e.operationMu.Unlock()
	_, terminal, err = f.e.WaitOperation(t.Context(), scope, r.ID, 1)
	attemptCode(t, err, "overloaded")
	if terminal {
		t.Fatal("global capacity refusal reported terminal")
	}
	f.e.operationMu.Lock()
	f.e.operationWaiterCount = 0
	f.e.operationMu.Unlock()
	a := f.start(t, attemptProject, r)
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "controlled_uncertain", ""); err != nil {
		t.Fatal(err)
	}
	value, terminal, err = f.e.WaitOperation(t.Context(), scope, r.ID, 900)
	if err != nil || !terminal || value.State != "uncertain" {
		t.Fatal(value, terminal, err)
	}
}

func TestOperationHTTPWaitImmediateReplayAndHeaderAdmission(t *testing.T) {
	f := sourceHTTPFixture(t)
	path := "/api/v1/projects/" + attemptProject + "/reviews/requests"
	body := encoded(Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	for _, bad := range []string{"", "wait=-1", "wait=901", "wait=01", "wait=+1", "wait=1.0", "respond-async", "wait=0, wait=0"} {
		if got := sourceHTTPRequest(f, "POST", path, f.writer, "bad", body, &bad); got.Code != 400 {
			t.Fatal("invalid preference admitted", bad, got.Code)
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("invalid preferences accepted work")
	}
	zero := "wait=0"
	w := sourceHTTPRequest(f, "POST", path, f.writer, "same-action", body, &zero)
	var receipt Receipt
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.State != "queued" || receipt.Revision != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	f.configure(t, attemptProject, "test-capability", 1, true)
	if _, err := f.e.StartAttempt(t.Context(), attemptProject, receipt.ID); err != nil {
		t.Fatal(err)
	}
	current, err := f.e.ReadOperation(t.Context(), operationReadScope(t, f), receipt.ID)
	if err != nil || current.State != "queued" || current.Revision != 1 || current.AttemptID != "" {
		t.Fatal("pause changed queued receipt", current, err)
	}
	// Changing collection preference keeps the same pending action. Resume it
	// and collect a genuine preparation hold through the existing POST wait.
	postDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { postDone <- sourceHTTPRequest(f, "POST", path, f.writer, "same-action", body, nil) }()
	operationWaitRegistered(t, f.e, 1)
	select {
	case result := <-postDone:
		t.Fatal("paused action returned terminal result", result.Code, result.Body.String())
	default:
	}
	f.configure(t, attemptProject, "test-capability", 2, false)
	attempt := f.start(t, attemptProject, receipt)
	if _, err := f.e.Advance(t.Context(), attemptProject, attempt.ID, attempt.Revision, "held", "controlled_preparation_hold", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case w = <-postDone:
	case <-time.After(2 * time.Second):
		t.Fatal("committed hold did not wake replay collection")
	}
	var operation Operation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &operation) != nil || operation.ID != receipt.ID || operation.State != "held" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = sourceHTTPRequest(f, "POST", path, f.writer, "same-action", body, &zero)
	if w.Code != 202 || w.Body.String() != encoded(receipt)+"\n" || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
		t.Fatal("replay changed original receipt or repeated action", w.Code, w.Body.String())
	}
	long := "wait=900"
	w = sourceHTTPRequest(f, "GET", strings.Replace(path, "/requests", "/operations/"+receipt.ID, 1), f.reader, "", "", &long)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"held"`) {
		t.Fatal("terminal GET recovery failed", w.Code, w.Body.String())
	}
	admissionSQL(t, f, `UPDATE grants SET can_read=0,can_write=1 WHERE credential_id='exec-writer-cred'`)
	w = sourceHTTPRequest(f, "POST", path, f.writer, "no-read", body, nil)
	if w.Code != 403 || strings.Contains(w.Body.String(), "outcome_unknown") || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
		t.Fatal("write-only wait admitted new action", w.Code, w.Body.String())
	}
}

func TestOperationHTTPDefaultWaitAndRetainedGETWake(t *testing.T) {
	f := sourceHTTPFixture(t)
	path := "/api/v1/projects/" + attemptProject + "/reviews/requests"
	body := encoded(Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	postDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { postDone <- sourceHTTPRequest(f, "POST", path, f.writer, "fast-default", body, nil) }()
	operationWaitRegistered(t, f.e, 1)
	var id string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_operations`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	f.configure(t, attemptProject, "test-capability", 1, true)
	if _, err := f.e.StartAttempt(t.Context(), attemptProject, id); err != nil {
		t.Fatal(err)
	}
	current, err := f.e.ReadOperation(t.Context(), operationReadScope(t, f), id)
	if err != nil || current.State != "queued" || current.Revision != 1 || current.AttemptID != "" {
		t.Fatal("pause changed pending POST intent", current, err)
	}
	select {
	case result := <-postDone:
		t.Fatal("pause completed default POST wait", result.Code, result.Body.String())
	default:
	}
	f.configure(t, attemptProject, "test-capability", 2, false)
	admissionPolicy(t, f, 1)
	if _, err := f.e.StartAttempt(t.Context(), attemptProject, id); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-postDone:
		if result.Code != http.StatusOK || result.Header().Get("Preference-Applied") != "wait=2" || !strings.Contains(result.Body.String(), `"state":"held"`) {
			t.Fatal(result.Code, result.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("default POST wait missed fast hold")
	}
	operationWaitRegistered(t, f.e, 0)
	// A separate operation can be collected by GET without another enqueue.
	r := f.enqueue(t, attemptProject, "recover-get", "cli", 2, 3)
	getDone := make(chan *httptest.ResponseRecorder, 1)
	prefer := "wait=900"
	go func() {
		getDone <- sourceHTTPRequest(f, "GET", strings.Replace(path, "/requests", "/operations/"+r.ID, 1), f.reader, "", "", &prefer)
	}()
	operationWaitRegistered(t, f.e, 1)
	a := f.start(t, attemptProject, r)
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "failed", "controlled_prepare_failure", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-getDone:
		if result.Code != 200 || !strings.Contains(result.Body.String(), `"state":"failed"`) {
			t.Fatal(result.Code, result.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("GET wait missed preparation failure")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 2 {
		t.Fatal("result collection started another action")
	}
}

type operationDeadlineFailure struct{ *httptest.ResponseRecorder }

func (operationDeadlineFailure) SetWriteDeadline(time.Time) error {
	return errors.New("controlled response deadline failure")
}

func TestOperationHTTPDeadlineAndDuplicatePreferenceRefuseBeforeAcceptance(t *testing.T) {
	f := sourceHTTPFixture(t)
	path := "/api/v1/projects/" + attemptProject + "/reviews/requests"
	body := encoded(Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	request := func() *http.Request {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+f.writer)
		r.Header.Set("Idempotency-Key", "refuse-before-acceptance")
		return r
	}
	r := request()
	w := operationDeadlineFailure{httptest.NewRecorder()}
	f.handler.ServeHTTP(w, r)
	if w.Code != 500 || strings.Contains(w.Body.String(), "outcome_unknown") {
		t.Fatal(w.Code, w.Body.String())
	}
	r = request()
	r.Header.Add("Prefer", "wait=0")
	r.Header.Add("Prefer", "wait=0")
	duplicate := httptest.NewRecorder()
	f.handler.ServeHTTP(duplicate, r)
	if duplicate.Code != 400 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM receipts WHERE operation='reviews.request'`) != 0 {
		t.Fatal("deadline/header refusal accepted an action", duplicate.Code, duplicate.Body.String())
	}
}

func TestOperationHTTPPostAcceptanceCancelKeepsOriginalRecovery(t *testing.T) {
	f := sourceHTTPFixture(t)
	path := "/api/v1/projects/" + attemptProject + "/reviews/requests"
	body := encoded(Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := httptest.NewRequest("POST", path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+f.writer)
	r.Header.Set("Idempotency-Key", "cancel-collection")
	r.Header.Set("Prefer", "wait=900")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		done <- w
	}()
	operationWaitRegistered(t, f.e, 1)
	cancel()
	select {
	case w := <-done:
		if w.Code != 503 || !strings.Contains(w.Body.String(), `"outcome_unknown":true`) || strings.Contains(w.Body.String(), "review_operation_") {
			t.Fatal("post-accept cancellation lost recovery semantics", w.Code, w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("canceled POST collection did not detach")
	}
	zero := "wait=0"
	w := sourceHTTPRequest(f, "POST", path, f.writer, "cancel-collection", body, &zero)
	if w.Code != 202 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE state='queued'`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM receipts WHERE operation='reviews.request'`) != 1 {
		t.Fatal("canceling collection changed accepted work", w.Code, w.Body.String())
	}
}

func TestOperationHTTPLifecycleCancellationDetachesPostAndGetWaits(t *testing.T) {
	newLifecycleHTTP := func(t *testing.T, f *attemptFixture, lifetime context.Context) (*httptest.Server, *Engine) {
		t.Helper()
		engine, err := New(f.store, attemptCapabilities, WithResultWaitLifetime(lifetime))
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		engine.Register(mux)
		return httptest.NewServer(mux), engine
	}
	request := func(t *testing.T, server *httptest.Server, f *attemptFixture, method, path, key, body, prefer string) *http.Response {
		t.Helper()
		r, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+f.writer)
		r.Header.Set("X-FC-Instance", f.store.InstanceID())
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		if prefer != "" {
			r.Header.Set("Prefer", prefer)
		}
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	path := "/api/v1/projects/" + attemptProject + "/reviews/requests"
	body := encoded(Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	long := "wait=900"

	t.Run("POST preserves accepted receipt", func(t *testing.T) {
		f := sourceHTTPFixture(t)
		lifetime, cancelLifetime := context.WithCancel(context.Background())
		defer cancelLifetime()
		httpServer, engine := newLifecycleHTTP(t, f, lifetime)
		defer httpServer.Close()
		f.e = engine
		done := make(chan *http.Response, 1)
		go func() { done <- request(t, httpServer, f, http.MethodPost, path, "lifecycle-post", body, long) }()
		operationWaitRegistered(t, engine, 1)
		cancelLifetime()
		var original []byte
		select {
		case response := <-done:
			defer response.Body.Close()
			data, _ := io.ReadAll(response.Body)
			var accepted Receipt
			if response.StatusCode != http.StatusAccepted || json.Unmarshal(data, &accepted) != nil || accepted.State != "queued" || accepted.Revision != 1 || !strings.HasPrefix(accepted.ID, "review_operation_") || strings.Contains(string(data), "outcome_unknown") {
				t.Fatal("lifecycle POST lost its known committed receipt", response.StatusCode, string(data))
			}
			original = data
		case <-time.After(time.Second):
			t.Fatal("lifecycle cancellation did not detach POST wait")
		}
		operationWaitRegistered(t, engine, 0)
		if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM receipts WHERE operation='reviews.request'`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE state='queued'`) != 1 {
			t.Fatal("canceling result collection changed accepted operation or receipt")
		}
		// A fresh daemon lifetime can recover the exact original receipt/key.
		recoveredEngine, err := New(f.store, attemptCapabilities)
		if err != nil {
			t.Fatal(err)
		}
		recoveryMux := http.NewServeMux()
		recoveredEngine.Register(recoveryMux)
		recovery := httptest.NewServer(recoveryMux)
		defer recovery.Close()
		response := request(t, recovery, f, http.MethodPost, path, "lifecycle-post", body, "wait=0")
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusAccepted || string(data) != string(original) || !strings.Contains(string(data), `"state":"queued"`) || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
			t.Fatal("original request receipt did not survive lifecycle wait", response.StatusCode, string(data))
		}
	})

	t.Run("GET releases waiter without changing operation", func(t *testing.T) {
		f := sourceHTTPFixture(t)
		queued := f.enqueue(t, attemptProject, "lifecycle-get", "cli", 2, 1)
		lifetime, cancelLifetime := context.WithCancel(context.Background())
		defer cancelLifetime()
		httpServer, engine := newLifecycleHTTP(t, f, lifetime)
		defer httpServer.Close()
		getPath := strings.Replace(path, "/requests", "/operations/"+queued.ID, 1)
		done := make(chan *http.Response, 1)
		go func() { done <- request(t, httpServer, f, http.MethodGet, getPath, "", "", long) }()
		operationWaitRegistered(t, engine, 1)
		before := attemptCount(t, f.store, `SELECT count(*) FROM receipts WHERE operation='reviews.request'`)
		cancelLifetime()
		select {
		case response := <-done:
			defer response.Body.Close()
			data, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(data), `"code":"overloaded"`) {
				t.Fatal("lifecycle GET lost safe canceled-wait response", response.StatusCode, string(data))
			}
		case <-time.After(time.Second):
			t.Fatal("lifecycle cancellation did not detach GET wait")
		}
		operationWaitRegistered(t, engine, 0)
		if attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE id=? AND state='queued'`, queued.ID) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM receipts WHERE operation='reviews.request'`) != before {
			t.Fatal("canceling GET changed operation or receipt state")
		}
	})
}
