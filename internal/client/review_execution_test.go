package client_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

const executionOperationID = "review_operation_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// queuedReviewRequest keeps receipt/replay tests explicit about requesting no wait.
func queuedReviewRequest(t *testing.T, c *client.Client, ctx context.Context, key string, request client.ReviewRequest) (client.ReviewRequestReceipt, error) {
	t.Helper()
	result, err := c.RequestReview(ctx, key, request, 0)
	if err != nil {
		return client.ReviewRequestReceipt{}, err
	}
	if result.Receipt == nil || result.Operation != nil {
		t.Fatalf("expected original queued receipt: %+v", result)
	}
	return *result.Receipt, nil
}

func sampleExecutionConfiguration() client.ReviewExecutionConfiguration {
	return client.ReviewExecutionConfiguration{ProjectID: "project-one", Revision: 1, CapabilityID: "synthetic-cli", Mode: "independent", Queue: []int64{2}, Channels: []string{"cli"}}
}

func sampleReviewOperation() client.ReviewOperation {
	return client.ReviewOperation{ID: executionOperationID, ProjectID: "project-one", ActorID: "principal_one", WorkerID: "other_worker", ConfigurationRevision: 1, PolicyRevision: 1, CapabilityFingerprint: strings.Repeat("a", 64), PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40), State: "queued", Revision: 1, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"}
}

func executionFixture(t *testing.T) (*mutationFixture, *reviewexec.Engine) {
	t.Helper()
	f := newMutationFixture(t)
	if err := reviewexec.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "synthetic-cli", Provider: "coderabbit", Account: "private-test-account", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	e.Register(mux)
	f.handler = mux
	base := "/api/v1/projects/" + f.project + "/reviews"
	f.call(t, "POST", base+"/policy", f.admin, f.sender, map[string]any{"expected_revision": 0, "required_channels": []string{"cli", "hosted"}, "paused_channels": []string{}, "max_review_files": 0})
	f.call(t, "POST", base+"/execution/configuration", f.admin, f.sender, map[string]any{"expected_revision": 0, "capability_id": "synthetic-cli", "mode": "independent", "queue": []int64{2, 4}, "channels": []string{"cli"}, "paused": false})
	return f, e
}

func executionCounts(t *testing.T, f *mutationFixture) [3]int {
	t.Helper()
	var counts [3]int
	for i, table := range []string{"review_operations", "events", "receipts"} {
		if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&counts[i]); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

func TestReviewExecutionPublicAPIRequestReplayAndLostCommittedResponse(t *testing.T) {
	f, e := executionFixture(t)
	var drop atomic.Bool
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Error("execution calls added unexpected refresh/query flags")
		}
		if r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.URL.Path != "/api/v1/instance" && r.Header.Get("X-FC-Worker") != f.sender {
			t.Error("execution client lost selected instance/worker binding")
		}
		if r.Method == "POST" {
			posts.Add(1)
		}
		if r.Method == "POST" && drop.Swap(false) {
			recorder := httptest.NewRecorder()
			f.handler.ServeHTTP(recorder, r)
			if recorder.Code != 202 {
				t.Errorf("lost-response request did not commit: %d %s", recorder.Code, recorder.Body.String())
			}
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := f.newClient(t, f.admin, f.sender, srv.URL)
	provider := f.providerCalls.Load()
	before := executionCounts(t, f)
	configuration, err := c.ReviewExecution(t.Context())
	if err != nil || configuration.ProjectID != f.project || configuration.Revision != 1 || configuration.CapabilityID != "synthetic-cli" || configuration.Paused || executionCounts(t, f) != before {
		t.Fatal("stored configuration read changed facts", configuration, err)
	}
	request := client.ReviewRequest{ExpectedConfigurationRevision: configuration.Revision, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}
	drop.Store(true)
	_, err = queuedReviewRequest(t, c, t.Context(), "request-once", request)
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || posts.Load() != 1 || executionCounts(t, f)[0] != 1 {
		t.Fatal("lost request was retried or not marked unknown", err)
	}
	var id string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_operations`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	committed := executionCounts(t, f)
	receipt, err := queuedReviewRequest(t, c, t.Context(), "request-once", request)
	if err != nil || receipt != (client.ReviewRequestReceipt{ID: id, State: "queued", Revision: 1}) || executionCounts(t, f) != committed || posts.Load() != 2 {
		t.Fatal("exact retry changed request effect", receipt, err)
	}
	operation, err := c.ReviewOperation(t.Context(), id)
	if err != nil || operation.ID != id || operation.ProjectID != f.project || operation.WorkerID != f.sender || operation.PolicyRevision != 1 || operation.State != "queued" || executionCounts(t, f) != committed {
		t.Fatal("operation read changed state or lost attribution", operation, err)
	}
	// Another worker in the same project may inspect this operation.
	srvSameProject := httptest.NewServer(f.handler)
	defer srvSameProject.Close()
	other := f.newClient(t, f.admin, f.recipient, srvSameProject.URL)
	if got, err := other.ReviewOperation(t.Context(), id); err != nil || got.WorkerID != f.sender {
		t.Fatal("same-project other-worker attribution rejected", got, err)
	}
	attempt, err := e.StartAttempt(t.Context(), f.project, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Advance(t.Context(), f.project, attempt.ID, attempt.Revision, "held", "synthetic_hold", ""); err != nil {
		t.Fatal(err)
	}
	advanced := executionCounts(t, f)
	replay, err := queuedReviewRequest(t, c, t.Context(), "request-once", request)
	if err != nil || replay != receipt || executionCounts(t, f) != advanced {
		t.Fatal("advanced operation changed original compact receipt", replay, err)
	}
	operation, err = c.ReviewOperation(t.Context(), id)
	if err != nil || operation.State != "held" || operation.AttemptID != attempt.ID {
		t.Fatal("advanced operation facts unavailable", operation, err)
	}
	changed := request
	changed.HeadSHA = strings.Repeat("b", 40)
	_, err = queuedReviewRequest(t, c, t.Context(), "request-once", changed)
	checkError(t, err, "idempotency_conflict")
	f.call(t, "POST", "/api/v1/projects/"+f.project+"/reviews/execution/configuration", f.admin, f.sender, map[string]any{"expected_revision": 1, "capability_id": "synthetic-cli", "mode": "independent", "queue": []int64{2}, "channels": []string{"cli"}, "paused": false})
	before = executionCounts(t, f)
	_, err = queuedReviewRequest(t, c, t.Context(), "stale-new-key", request)
	checkError(t, err, "review_conflict")
	if replay, err := queuedReviewRequest(t, c, t.Context(), "request-once", request); err != nil || replay != receipt {
		t.Fatal("later configuration changed original request replay", replay, err)
	}
	if executionCounts(t, f) != before || f.providerCalls.Load() != provider {
		t.Fatal("queued requests/read/stale configuration contacted provider or added effects")
	}
}

func TestReviewExecutionStrictConfigurationWire(t *testing.T) {
	valid, _ := json.Marshal(sampleExecutionConfiguration())
	for name, change := range map[string]func(string) string{
		"foreign-project": func(s string) string {
			return strings.Replace(s, `"project_id":"project-one"`, `"project_id":"other"`, 1)
		},
		"missing-paused":     func(s string) string { return strings.Replace(s, `,"paused":false`, "", 1) },
		"null-paused":        func(s string) string { return strings.Replace(s, `"paused":false`, `"paused":null`, 1) },
		"alias":              func(s string) string { return strings.Replace(s, `"revision":1`, `"revision":1,"Revision":2`, 1) },
		"null-queue":         func(s string) string { return strings.Replace(s, `"queue":[2]`, `"queue":null`, 1) },
		"duplicate-pull":     func(s string) string { return strings.Replace(s, `"queue":[2]`, `"queue":[2,2]`, 1) },
		"bad-pull":           func(s string) string { return strings.Replace(s, `"queue":[2]`, `"queue":[2147483648]`, 1) },
		"empty-channels":     func(s string) string { return strings.Replace(s, `"channels":["cli"]`, `"channels":[]`, 1) },
		"duplicate-channels": func(s string) string { return strings.Replace(s, `"channels":["cli"]`, `"channels":["cli","cli"]`, 1) },
		"bad-mode":           func(s string) string { return strings.Replace(s, `"mode":"independent"`, `"mode":"automatic"`, 1) },
		"bad-capability": func(s string) string {
			return strings.Replace(s, `"capability_id":"synthetic-cli"`, `"capability_id":"../private"`, 1)
		},
		"zero-revision":       func(s string) string { return strings.Replace(s, `"revision":1`, `"revision":0`, 1) },
		"fractional-revision": func(s string) string { return strings.Replace(s, `"revision":1`, `"revision":1.5`, 1) },
		"unknown": func(s string) string {
			return strings.Replace(s, `"paused":false`, `"paused":false,"account":"private"`, 1)
		},
		"oversized": func(s string) string { return strings.Repeat(" ", 4<<20) + s },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(change(string(valid))))
			}))
			defer srv.Close()
			c := newClient(t, srv)
			defer c.Close()
			_, err := c.ReviewExecution(t.Context())
			checkError(t, err, "invalid_response")
		})
	}
}

func TestReviewExecutionStrictOperationWireAndLegacyFacts(t *testing.T) {
	valid, _ := json.Marshal(sampleReviewOperation())
	for name, change := range map[string]func(string) string{
		"different-id": func(s string) string {
			return strings.Replace(s, executionOperationID, "review_operation_"+strings.Repeat("b", 32), 1)
		},
		"foreign-project": func(s string) string {
			return strings.Replace(s, `"project_id":"project-one"`, `"project_id":"other"`, 1)
		},
		"bad-state": func(s string) string { return strings.Replace(s, `"state":"queued"`, `"state":"ready"`, 1) },
		"bad-actor": func(s string) string {
			return strings.Replace(s, `"actor_id":"principal_one"`, `"actor_id":"bad actor"`, 1)
		},
		"bad-worker": func(s string) string {
			return strings.Replace(s, `"worker_id":"other_worker"`, `"worker_id":"../worker"`, 1)
		},
		"bad-attempt": func(s string) string {
			return strings.Replace(s, `"attempt_id":""`, `"attempt_id":"review_attempt_nothex"`, 1)
		},
		"negative-policy": func(s string) string { return strings.Replace(s, `"policy_revision":1`, `"policy_revision":-1`, 1) },
		"bad-fingerprint": func(s string) string { return strings.Replace(s, strings.Repeat("a", 64), strings.Repeat("A", 64), 1) },
		"bad-head": func(s string) string {
			return strings.Replace(s, `"head_sha":"`+strings.Repeat("a", 40)+`"`, `"head_sha":"unknown"`, 1)
		},
		"bad-time": func(s string) string {
			return strings.Replace(s, `"updated_at":"2026-01-01T00:00:00Z"`, `"updated_at":"now"`, 1)
		},
		"null-reason":    func(s string) string { return strings.Replace(s, `"reason":""`, `"reason":null`, 1) },
		"missing-reason": func(s string) string { return strings.Replace(s, `,"reason":""`, "", 1) },
		"alias": func(s string) string {
			return strings.Replace(s, `"state":"queued"`, `"state":"queued","State":"completed"`, 1)
		},
		"unknown": func(s string) string {
			return strings.Replace(s, `"reason":""`, `"reason":"","catalog_path":"private"`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(change(string(valid))))
			}))
			defer srv.Close()
			c := newClient(t, srv)
			defer c.Close()
			_, err := c.ReviewOperation(t.Context(), executionOperationID)
			checkError(t, err, "invalid_response")
		})
	}
	legacy := sampleReviewOperation()
	legacy.PolicyRevision = 0
	legacy.CapabilityFingerprint = ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(legacy)
	}))
	defer srv.Close()
	c := newClient(t, srv)
	defer c.Close()
	if got, err := c.ReviewOperation(t.Context(), executionOperationID); err != nil || got != legacy {
		t.Fatal("retained unbound facts were rewritten or rejected", got, err)
	}
}

func TestReviewExecutionValidationBeforeCredentialOrHTTP(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); handshake(w) }))
	defer srv.Close()
	c := newClient(t, srv)
	defer c.Close()
	before := calls.Load()
	valid := client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}
	for _, request := range []client.ReviewRequest{{}, {ExpectedConfigurationRevision: 1, PullNumber: 2147483648, Channel: "cli", HeadSHA: valid.HeadSHA}, {ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "automatic", HeadSHA: valid.HeadSHA}, {ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("A", 40)}} {
		if client.ValidateReviewRequest(request) == nil {
			t.Fatal("validator accepted invalid request")
		}
		_, err := queuedReviewRequest(t, c, t.Context(), "valid-key", request)
		checkError(t, err, "validation_failed")
	}
	for _, key := range []string{"", " padded ", "line\nbreak", strings.Repeat("k", 129)} {
		_, err := queuedReviewRequest(t, c, t.Context(), key, valid)
		checkError(t, err, "validation_failed")
	}
	for _, id := range []string{"", "../operation", "review_operation_" + strings.Repeat("A", 32), executionOperationID + "extra"} {
		if client.ValidateReviewOperationID(id) == nil {
			t.Fatal("validator accepted invalid operation id")
		}
		_, err := c.ReviewOperation(t.Context(), id)
		checkError(t, err, "validation_failed")
	}
	if calls.Load() != before {
		t.Fatal("invalid arguments reached HTTP")
	}
	// Nil receiver proves admission does not need even an already-loaded client.
	var unloaded *client.Client
	_, err := queuedReviewRequest(t, unloaded, t.Context(), "", valid)
	checkError(t, err, "validation_failed")
	_, err = unloaded.ReviewOperation(t.Context(), "invalid")
	checkError(t, err, "validation_failed")
	if err := client.ValidateReviewRequest(valid); err != nil {
		t.Fatal(err)
	}
}

func TestReviewExecutionInvalidReceiptsRemainUnknownAndSingleShot(t *testing.T) {
	valid := `{"id":"` + executionOperationID + `","state":"queued","revision":1}`
	for _, body := range []string{strings.Replace(valid, `"state":"queued"`, `"state":"active"`, 1), strings.Replace(valid, `"revision":1`, `"revision":2`, 1), strings.Replace(valid, `"revision":1`, `"revision":null`, 1), strings.Replace(valid, `"revision":1`, `"revision":1,"Revision":2`, 1), strings.Replace(valid, executionOperationID, "operation", 1)} {
		var posts atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			posts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(202)
			_, _ = w.Write([]byte(body))
		}))
		c := newClient(t, srv)
		_, err := queuedReviewRequest(t, c, t.Context(), "explicit-key", client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
		var failure *client.Error
		if !errors.As(err, &failure) || !failure.OutcomeUnknown || failure.Code != "invalid_response" || posts.Load() != 1 {
			t.Fatal("invalid receipt lost ambiguity or auto-retried", err)
		}
		c.Close()
		srv.Close()
	}
}

func TestReviewExecutionReadOnlyAndForeignProjectIsolation(t *testing.T) {
	f, _ := executionFixture(t)
	readerToken := "synthetic-execution-reader-token-0000000000"
	sum := sha256.Sum256([]byte(readerToken))
	f.call(t, "POST", "/api/v1/projects/"+f.project+"/credentials", f.admin, "", map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": []string{"read"}})
	otherProject := f.call(t, "POST", "/api/v1/projects", f.admin, "", map[string]string{"name": "Other", "repository": "owner/other"})["id"].(string)
	srv := httptest.NewServer(f.handler)
	defer srv.Close()
	writer := f.newClient(t, f.admin, f.sender, srv.URL)
	reader := f.newClient(t, readerToken, f.recipient, srv.URL)
	request := client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}
	receipt, err := queuedReviewRequest(t, writer, t.Context(), "scoped-request", request)
	if err != nil {
		t.Fatal(err)
	}
	before := executionCounts(t, f)
	if _, err := reader.ReviewExecution(t.Context()); err != nil {
		t.Fatal("reader could not inspect stored configuration", err)
	}
	if got, err := reader.ReviewOperation(t.Context(), receipt.ID); err != nil || got.WorkerID != f.sender {
		t.Fatal("reader could not inspect same-project other-worker operation", got, err)
	}
	_, err = queuedReviewRequest(t, reader, t.Context(), "reader-must-not-dispatch", request)
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "forbidden" || failure.OutcomeUnknown {
		t.Fatal("reader request not known refused", err)
	}
	for _, path := range []string{"/api/v1/projects/" + otherProject + "/reviews/execution/configuration", "/api/v1/projects/" + otherProject + "/reviews/operations/" + receipt.ID} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+readerToken)
		r.Header.Set("X-FC-Instance", f.store.InstanceID())
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code < 400 || strings.Contains(w.Body.String(), "synthetic-cli") || strings.Contains(w.Body.String(), receipt.ID) {
			t.Fatal("foreign project read exposed facts", w.Code, w.Body.String())
		}
	}
	// Administrators can read the foreign project but cannot move an operation
	// across its original project identity merely by changing the URL.
	r := httptest.NewRequest("GET", "/api/v1/projects/"+otherProject+"/reviews/operations/"+receipt.ID, nil)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("operation leaked across administrator project selection", w.Code, w.Body.String())
	}
	if executionCounts(t, f) != before {
		t.Fatal("reads/refused requests changed domain state")
	}
}

func TestReviewExecutionSafeFailureRequestIDAndErrorBudget(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(map[bool]string{false: "safe-error", true: "oversized-error"}[oversized], func(t *testing.T) {
			var posts atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				posts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(503)
				body := `{"error":{"code":"overloaded","message":"PRIVATE_PROVIDER_MESSAGE"},"request_id":"request-safe-id"}`
				if oversized {
					body = strings.Repeat(" ", 64<<10) + body
				}
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			c := newClient(t, srv)
			defer c.Close()
			_, err := queuedReviewRequest(t, c, t.Context(), "same-key", client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
			var failure *client.Error
			if !errors.As(err, &failure) || !failure.OutcomeUnknown || posts.Load() != 1 || strings.Contains(err.Error(), "PRIVATE_PROVIDER_MESSAGE") {
				t.Fatal("failure privacy/outcome changed", err)
			}
			if !oversized && (failure.Code != "overloaded" || failure.RequestID != "request-safe-id") {
				t.Fatal("safe request ID discarded", failure)
			}
			if oversized && (failure.Code != "invalid_response" || failure.RequestID != "") {
				t.Fatal("oversized error was trusted", failure)
			}
		})
	}
}

func TestReviewExecutionCancellationUsesSharedRequestBudget(t *testing.T) {
	for _, mutation := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "request"}[mutation], func(t *testing.T) {
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				started <- struct{}{}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer srv.Close()
			defer close(release)
			c := newClient(t, srv)
			defer c.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if mutation {
					_, err := queuedReviewRequest(t, c, ctx, "same-key", client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
					done <- err
				} else {
					_, err := c.ReviewExecution(ctx)
					done <- err
				}
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("request did not reach controlled handler")
			}
			cancel()
			select {
			case err := <-done:
				var failure *client.Error
				if !errors.As(err, &failure) || failure.Code != "canceled" || failure.OutcomeUnknown != mutation {
					t.Fatal("cancellation lost read/write outcome semantics", err)
				}
			case <-time.After(time.Second):
				t.Fatal("request ignored caller cancellation")
			}
		})
	}
}

func TestReviewExecutionStoredReadsWorkWithoutProviderCatalog(t *testing.T) {
	f, _ := executionFixture(t)
	configured := httptest.NewServer(f.handler)
	writer := f.newClient(t, f.admin, f.sender, configured.URL)
	receipt, err := queuedReviewRequest(t, writer, t.Context(), "offline-retained-request", client.ReviewRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	configured.Close()
	if err != nil {
		t.Fatal(err)
	}
	unavailable, err := reviewexec.New(f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	unavailable.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	reader := f.newClient(t, f.admin, f.recipient, srv.URL)
	before, providers := executionCounts(t, f), f.providerCalls.Load()
	if got, err := reader.ReviewExecution(t.Context()); err != nil || got.CapabilityID != "synthetic-cli" || got.Revision != 1 {
		t.Fatal("stored configuration depended on catalog availability", got, err)
	}
	if got, err := reader.ReviewOperation(t.Context(), receipt.ID); err != nil || got.ID != receipt.ID || got.State != "queued" || got.WorkerID != f.sender {
		t.Fatal("stored operation depended on catalog availability", got, err)
	}
	if executionCounts(t, f) != before || f.providerCalls.Load() != providers {
		t.Fatal("ordinary reads performed provider work or domain mutation")
	}
}
