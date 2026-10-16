package reviewexec

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/server"
)

func sourceHTTPFixture(t *testing.T) *attemptFixture {
	t.Helper()
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	return f
}
func sourceHTTPRequest(f *attemptFixture, method, path, token, key, body string, prefer *string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if prefer != nil {
		r.Header.Set("Prefer", *prefer)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func TestSourceRefreshHTTPStrictScopedBoundariesAndImmediateReceipt(t *testing.T) {
	f := sourceHTTPFixture(t)
	path := "/api/v1/projects/" + attemptProject + "/reviews/source-refreshes"
	zero := "wait=0"
	body := encoded(refreshRequest())
	for _, bad := range []string{`{}`, `null`, `{"expected_configuration_revision":null,"pull_number":2,"head_sha":"` + strings.Repeat("a", 40) + `"}`, `{"expected_configuration_revision":1,"pull_number":null,"head_sha":"` + strings.Repeat("a", 40) + `"}`, `{"expected_configuration_revision":1,"pull_number":2,"head_sha":null}`, strings.Replace(body, `"pull_number":2`, `"pull_number":2,"pull_number":2`, 1), strings.Replace(body, `"pull_number":2`, `"Pull_number":2`, 1), strings.Replace(body, `"pull_number":2`, `"pull_number":0`, 1), strings.Replace(body, `"head_sha":"`+strings.Repeat("a", 40)+`"`, `"head_sha":"`+strings.Repeat("A", 40)+`"`, 1), strings.TrimSuffix(body, "}") + `,"worker_id":"other"}`} {
		w := sourceHTTPRequest(f, "POST", path, f.writer, "strict", bad, &zero)
		if w.Code != 400 {
			t.Fatal("malformed source refresh accepted", w.Code, bad, w.Body.String())
		}
	}
	for _, bad := range []string{"", "wait=-1", "wait=61", "wait=1.0", "wait=01", "wait=+1", "wait=2, wait=3", "respond-async"} {
		w := sourceHTTPRequest(f, "POST", path, f.writer, "bad-prefer", body, &bad)
		if w.Code != 400 {
			t.Fatal("invalid Prefer header accepted", bad, w.Code)
		}
	}
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+f.writer)
	r.Header.Set("Idempotency-Key", "duplicate-pref")
	r.Header.Add("Prefer", "wait=0")
	r.Header.Add("Prefer", "wait=0")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("duplicate Prefer accepted")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM receipts WHERE operation='reviews.source_refresh'`) != 0 {
		t.Fatal("invalid request changed durable state")
	}
	for _, tc := range []struct {
		token, path string
		want        int
	}{{"", path, 401}, {f.reader, path, 403}, {f.other, path, 403}, {f.writer, path + "?refresh=true", 400}} {
		w := sourceHTTPRequest(f, "POST", tc.path, tc.token, "forbidden", body, &zero)
		if w.Code != tc.want {
			t.Fatal("authority/query boundary", w.Code, tc.want)
		}
	}
	w = sourceHTTPRequest(f, "POST", path, f.writer, "immediate", body, &zero)
	var receipt Receipt
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.Revision != 1 || receipt.State != "queued" || !validRefreshID(receipt.ID) {
		t.Fatal("not compact immediate receipt", w.Code, w.Body.String())
	}
	read := sourceHTTPRequest(f, "GET", path+"/"+receipt.ID, f.reader, "", "", nil)
	if read.Code != 200 || strings.Contains(read.Body.String(), "capability_fingerprint") || strings.Contains(read.Body.String(), "actor_id") || strings.Contains(read.Body.String(), f.writer) || strings.Contains(read.Body.String(), "observation") {
		t.Fatal("pending response private fields", read.Code, read.Body.String())
	}
	for _, tc := range []struct {
		path, token string
		want        int
	}{{path + "/bad", f.reader, 400}, {path + "/" + receipt.ID + "?x=1", f.reader, 400}, {strings.Replace(path, attemptProject, otherAttemptProject, 1) + "/" + receipt.ID, f.other, 404}, {"/api/v1/projects/" + attemptProject + "/reviews/source/02/policy", f.reader, 400}, {"/api/v1/projects/" + attemptProject + "/reviews/source/2/policy", f.reader, 409}} {
		w := sourceHTTPRequest(f, "GET", tc.path, tc.token, "", "", nil)
		if w.Code != tc.want {
			t.Fatal("read scope/shape", tc.path, w.Code, w.Body.String())
		}
	}
	if w := sourceHTTPRequest(f, "DELETE", path, f.admin, "", "", nil); w.Code != 405 {
		t.Fatal("unsupported method accepted")
	}
	wrong := httptest.NewRequest("GET", path+"/"+receipt.ID, nil)
	wrong.Header.Set("Authorization", "Bearer "+f.reader)
	wrong.Header.Set("X-FC-Instance", "instance_other")
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, wrong)
	if w.Code != 409 {
		t.Fatal("instance mismatch accepted", w.Code)
	}
}

func TestSourceRefreshHTTPWriteGrantRequiresReadBeforeAdmission(t *testing.T) {
	f := sourceHTTPFixture(t)
	mux := http.NewServeMux()
	mux.Handle("/", server.New(f.store))
	f.e.Register(mux)
	f.handler = mux
	counts := func() []int {
		t.Helper()
		result := make([]int, 0, 5)
		for _, table := range []string{"credentials", "grants", "review_source_refreshes", "receipts", "events"} {
			result = append(result, attemptCount(t, f.store, "SELECT count(*) FROM "+table))
		}
		return result
	}
	before := fmt.Sprint(counts())
	// The public grant API cannot create an initially write-only principal.
	w := sourceHTTPRequest(f, "POST", "/api/v1/projects/"+attemptProject+"/credentials", f.admin, "write-only-grant", `{"token_sha256":"`+strings.Repeat("f", 64)+`","permissions":["write"]}`, nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"validation_failed"`) || fmt.Sprint(counts()) != before {
		t.Fatal("write-only grant was admitted or changed storage", w.Code, w.Body.String())
	}
	// Although the base table permits this shape, both write and read
	// authorization require can_read. A damaged/manual grant must be refused
	// before enqueue, rather than becoming a post-commit wait failure.
	admissionSQL(t, f, `UPDATE grants SET can_read=0,can_write=1 WHERE credential_id='exec-writer-cred'`)
	before = fmt.Sprint(counts())
	path := "/api/v1/projects/" + attemptProject + "/reviews/source-refreshes"
	zero := "wait=0"
	for _, prefer := range []*string{nil, &zero} {
		w = sourceHTTPRequest(f, "POST", path, f.writer, "no-admission", encoded(refreshRequest()), prefer)
		if w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"forbidden"`) || strings.Contains(w.Body.String(), "outcome_unknown") || strings.Contains(w.Body.String(), "review_refresh_") || fmt.Sprint(counts()) != before {
			t.Fatal("write-only caller reached admission/wait or changed storage", w.Code, w.Body.String())
		}
	}
}

func TestSourceRefreshHTTPDefaultWaitReturnsFastTerminalAndExactReplay(t *testing.T) {
	f := sourceHTTPFixture(t)
	path := "/api/v1/projects/" + attemptProject + "/reviews/source-refreshes"
	reader := &refreshTestReader{}
	cancel, done := refreshStart(t, f, reader)
	start := time.Now()
	w := sourceHTTPRequest(f, "POST", path, f.writer, "fast", encoded(refreshRequest()), nil)
	var operation SourceRefreshOperation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &operation) != nil || operation.State != "completed" || operation.Observation == nil || time.Since(start) > time.Second {
		t.Fatal("default wait did not return fast result", w.Code, w.Body.String())
	}
	calls := reader.calls.Load()
	events := attemptCount(t, f.store, `SELECT count(*) FROM events`)
	w = sourceHTTPRequest(f, "POST", path, f.writer, "fast", encoded(refreshRequest()), nil)
	if w.Code != 200 || reader.calls.Load() != calls || attemptCount(t, f.store, `SELECT count(*) FROM events`) != events {
		t.Fatal("terminal exact replay reread provider/changed state", w.Code)
	}
	zero := "wait=0"
	w = sourceHTTPRequest(f, "POST", path, f.writer, "fast", encoded(refreshRequest()), &zero)
	var original Receipt
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &original) != nil || original.ID != operation.ID || original.State != "queued" || original.Revision != 1 {
		t.Fatal("wait preference entered idempotency or rewrote original receipt", w.Code, w.Body.String())
	}
	refreshStop(t, cancel, done)
	assessment := sourceHTTPRequest(f, "GET", "/api/v1/projects/"+attemptProject+"/reviews/source/2/policy", f.reader, "", "", nil)
	if assessment.Code != 200 || assessment.Body.Len() > 16<<10 || !strings.Contains(assessment.Body.String(), `"source":"observed_comparison"`) {
		t.Fatal("compact stored assessment unavailable", assessment.Code, assessment.Body.String())
	}
}
func TestSourceRefreshWaitCancellationCapacityAndAuthorization(t *testing.T) {
	f := sourceHTTPFixture(t)
	receipt := refreshEnqueue(t, f, "waiting", refreshRequest())
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	beforeEvents := attemptCount(t, f.store, `SELECT count(*) FROM events`)
	ctx, cancel := context.WithCancel(t.Context())
	results := make(chan error, 8)
	for range 8 {
		go func() { _, err := f.e.WaitSourceRefresh(ctx, scope, receipt.ID, time.Minute); results <- err }()
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		f.e.sourceMu.Lock()
		n := f.e.sourceWaiterCount
		f.e.sourceMu.Unlock()
		if n == 8 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	_, err := f.e.WaitSourceRefresh(t.Context(), scope, receipt.ID, time.Minute)
	attemptCode(t, err, "overloaded")
	cancel()
	for range 8 {
		select {
		case err := <-results:
			if err == nil {
				t.Fatal("cancelled waiter succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("cancelled waiter leaked")
		}
	}
	f.e.sourceMu.Lock()
	leaked := f.e.sourceWaiterCount != 0 || len(f.e.sourceWaiters) != 0 || len(f.e.sourceWaiterPrincipals) != 0
	f.e.sourceMu.Unlock()
	if leaked {
		t.Fatal("wait capacity leaked")
	}
	if got := refreshRead(t, f, receipt.ID); got.State != "queued" || got.Revision != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events`) != beforeEvents {
		t.Fatal("wait/cancellation mutated operation")
	}
	// Current grant is checked again after the bounded wait, not frozen at entry.
	done := make(chan error, 1)
	go func() {
		_, err := f.e.WaitSourceRefresh(t.Context(), scope, receipt.ID, 80*time.Millisecond)
		done <- err
	}()
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		f.e.sourceMu.Lock()
		n := f.e.sourceWaiterCount
		f.e.sourceMu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	admissionSQL(t, f, `UPDATE grants SET can_read=0 WHERE credential_id='exec-reader-cred'`)
	select {
	case err := <-done:
		attemptCode(t, err, "forbidden")
	case <-time.After(time.Second):
		t.Fatal("timeout did not reauthorize")
	}
	// A terminal transition between registration and snapshot cannot lose wakeup.
	scope = evidenceReadScope(t, f, attemptProject, f.admin)
	waitCtx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	terminal := make(chan SourceRefreshOperation, 1)
	go func() { value, _ := f.e.WaitSourceRefresh(waitCtx, scope, receipt.ID, time.Second); terminal <- value }()
	reader := &refreshTestReader{}
	shutdown, worker := refreshStart(t, f, reader)
	select {
	case value := <-terminal:
		if value.State != "completed" {
			t.Fatal("completion wake lost", value)
		}
	case <-time.After(time.Second):
		t.Fatal("completion not delivered")
	}
	refreshStop(t, shutdown, worker)
}
func TestSourceRefreshWaitSlowStillQueuedReceiptAndHeldResult(t *testing.T) {
	f := sourceHTTPFixture(t)
	receipt := refreshEnqueue(t, f, "slow", refreshRequest())
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	started := time.Now()
	value, err := f.e.WaitSourceRefresh(t.Context(), scope, receipt.ID, 15*time.Millisecond)
	if err != nil || value.State != "queued" || time.Since(started) > time.Second {
		t.Fatal("bounded wait did not return retained state", value, err)
	}
	reader := &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
		return reviewprovider.Repository{}, fmt.Errorf("PRIVATE TOKEN PATH")
	}}
	cancel, done := refreshStart(t, f, reader)
	held := refreshWait(t, f, receipt.ID, "held")
	refreshStop(t, cancel, done)
	if held.Reason != "source_unavailable" {
		t.Fatal("provider error leaked or not classified", held)
	}
	path := "/api/v1/projects/" + attemptProject + "/reviews/source-refreshes"
	w := sourceHTTPRequest(f, "POST", path, f.writer, "slow", encoded(refreshRequest()), nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "PRIVATE") {
		t.Fatal("held result unavailable or unsafe", w.Code, w.Body.String())
	}
}

func TestSourceRefreshHTTPWaitCapacityFallsBackAfterCommitAndReauthorizes(t *testing.T) {
	f := sourceHTTPFixture(t)
	receipt := refreshEnqueue(t, f, "existing-wait", refreshRequest())
	scope := f.scope(t, attemptProject, f.writer, false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 8)
	for range 8 {
		go func() { _, err := f.e.WaitSourceRefresh(ctx, scope, receipt.ID, time.Minute); done <- err }()
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		f.e.sourceMu.Lock()
		n := f.e.sourceWaiterCount
		f.e.sourceMu.Unlock()
		if n == 8 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	path := "/api/v1/projects/" + attemptProject + "/reviews/source-refreshes"
	w := sourceHTTPRequest(f, "POST", path, f.writer, "capacity-accepted", encoded(refreshRequest()), nil)
	var accepted Receipt
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &accepted) != nil || accepted.ID != receipt.ID || accepted.Revision != 1 || accepted.State != "queued" {
		t.Fatal("full waiter capacity hid already accepted action", w.Code, w.Body.String())
	}
	if w.Header().Get("Vary") != "Prefer" || w.Header().Get("Preference-Applied") != "wait=2" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("source wait metadata/cache headers missing")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM receipts WHERE operation='reviews.source_refresh'`) != 2 {
		t.Fatal("fallback repeated or dropped accepted work")
	}
	cancel()
	for range 8 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("wait cleanup leaked")
		}
	}
	zero := "wait=0"
	w = sourceHTTPRequest(f, "POST", path, f.writer, "capacity-accepted", encoded(refreshRequest()), &zero)
	if w.Code != 202 || !strings.Contains(w.Body.String(), receipt.ID) {
		t.Fatal("fallback key not replayable")
	}
}
func TestSourceRefreshHTTPPostCommitCancellationAndRevocationKeepOutcomeUnknown(t *testing.T) {
	for _, kind := range []string{"cancel", "revoke"} {
		t.Run(kind, func(t *testing.T) {
			f := sourceHTTPFixture(t)
			path := "/api/v1/projects/" + attemptProject + "/reviews/source-refreshes"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := httptest.NewRequest("POST", path, strings.NewReader(encoded(refreshRequest()))).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+f.writer)
			r.Header.Set("Idempotency-Key", "accepted-before-wait")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { f.handler.ServeHTTP(w, r); close(done) }()
			deadline := time.Now().Add(time.Second)
			registered := false
			for time.Now().Before(deadline) {
				f.e.sourceMu.Lock()
				registered = f.e.sourceWaiterCount == 1
				f.e.sourceMu.Unlock()
				if registered {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !registered {
				t.Fatal("HTTP never entered postcommit wait")
			}
			if kind == "cancel" {
				cancel()
			} else {
				admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
				var id string
				if err := f.store.DB().QueryRow(`SELECT id FROM review_source_refreshes`).Scan(&id); err != nil {
					t.Fatal(err)
				}
				f.e.notifySourceRefresh(id)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("postcommit wait not interrupted")
			}
			var body struct {
				Error struct {
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error.Details["outcome_unknown"] != true || strings.Contains(w.Body.String(), "review_refresh_") {
				t.Fatal("accepted-result failure implied rollback/exposed ID", w.Code, w.Body.String())
			}
			if kind == "revoke" && w.Code != 401 {
				t.Fatal("revocation authority changed", w.Code)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes WHERE state='queued' AND revision=1`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM receipts WHERE operation='reviews.source_refresh'`) != 1 {
				t.Fatal("waiting cancellation erased or repeated accepted operation")
			}
			if kind == "cancel" {
				raw, err := f.e.EnqueueSourceRefresh(t.Context(), f.scope(t, attemptProject, f.writer, false), "accepted-before-wait", refreshRequest())
				if err != nil || !strings.Contains(string(raw), "review_refresh_") {
					t.Fatal("lost response not recoverable by original key", err)
				}
			}
		})
	}
}

type sourceDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
	failure  error
}

func (w *sourceDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return w.failure
}
func TestSourceRefreshHTTPScopedWriteDeadlineBeforeEnqueue(t *testing.T) {
	f := sourceHTTPFixture(t)
	path := "/api/v1/projects/" + attemptProject + "/reviews/source-refreshes"
	for _, fail := range []bool{true, false} {
		r := httptest.NewRequest("POST", path, strings.NewReader(encoded(refreshRequest())))
		r.Header.Set("Authorization", "Bearer "+f.writer)
		r.Header.Set("Idempotency-Key", "deadline")
		r.Header.Set("Prefer", "wait=0")
		w := &sourceDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		if fail {
			w.failure = fmt.Errorf("private unsupported connection failure")
		}
		before := time.Now()
		f.handler.ServeHTTP(w, r)
		if w.deadline.Before(before.Add(5*time.Second)) || w.deadline.After(time.Now().Add(5*time.Second)) {
			t.Fatal("source-only deadline not bounded")
		}
		if fail {
			if w.Code != 500 || attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes`) != 0 || strings.Contains(w.Body.String(), "private unsupported") {
				t.Fatal("deadline failure accepted work/exposed diagnostics", w.Code)
			}
		} else if w.Code != 202 {
			t.Fatal("supported source deadline rejected", w.Code, w.Body.String())
		}
	}
}
