package client_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/server"
)

type mutationFixture struct {
	store                  *core.Store
	admin, project, sender string
	recipient, tokenFile   string
	handler                http.Handler
	providerCalls          atomic.Int64
}

func newMutationFixture(t *testing.T) *mutationFixture {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "private")
	store, err := core.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := jobs.EnsureSchema(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureSchema(ctx, store); err != nil {
		t.Fatal(err)
	}
	adminData, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f := &mutationFixture{store: store, admin: strings.TrimSpace(string(adminData)), tokenFile: filepath.Join(dir, "bootstrap-admin.token")}
	base := server.New(store, server.WithRepositoryResolver(func(_ context.Context, repository string) (server.RepositoryIdentity, error) {
		return server.RepositoryIdentity{ID: f.providerCalls.Add(1), FullName: repository}, nil
	}))
	mux := http.NewServeMux()
	mux.Handle("/", base)
	jobs.Register(mux, store)
	inbox.Register(mux, store)
	f.handler = mux
	f.project = f.call(t, http.MethodPost, "/api/v1/projects", f.admin, "", map[string]string{"name": "project", "repository": "owner/repository"})["id"].(string)
	f.sender = f.call(t, http.MethodPost, "/api/v1/projects/"+f.project+"/workers", f.admin, "", map[string]string{"name": "Sender"})["id"].(string)
	f.recipient = f.call(t, http.MethodPost, "/api/v1/projects/"+f.project+"/workers", f.admin, "", map[string]string{"name": "Recipient"})["id"].(string)
	return f
}

func (f *mutationFixture) call(t *testing.T, method, path, token, worker string, body any) map[string]any {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(string(data)))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	if worker != "" {
		r.Header.Set("X-FC-Worker", worker)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "fixture-"+core.NewID("key"))
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code < 200 || w.Code >= 300 {
		t.Fatalf("fixture %s %s status=%d body=%s", method, path, w.Code, w.Body.String())
	}
	var value map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatalf("fixture JSON: %v", err)
	}
	return value
}

func (f *mutationFixture) newClient(t *testing.T, token string, worker string, base string) *client.Client {
	t.Helper()
	credential := f.tokenFile
	if token != f.admin {
		credential = filepath.Join(t.TempDir(), "credential")
		if err := os.WriteFile(credential, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(context.Background(), profile.Binding{Context: profile.Context{Profile: "mutation-test", ServerURL: base, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: worker}, CredentialFile: credential})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func (f *mutationFixture) eventCount(t *testing.T) int {
	t.Helper()
	var count int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM events WHERE project_id=?", f.project).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestTypedMutationsUsePublicScopedAPIAndReceipts(t *testing.T) {
	f := newMutationFixture(t)
	httpServer := httptest.NewServer(f.handler)
	defer httpServer.Close()
	c := f.newClient(t, f.admin, f.sender, httpServer.URL)
	initialEvents := f.eventCount(t)

	created, err := c.CreateJob(context.Background(), "job-create-1", client.CreateJobRequest{Title: "  Build release  ", Brief: "  preserve brief\n", WorkerID: " " + f.sender + " ", Status: ""})
	if err != nil || created.ID == "" || created.Outcome != "created" || created.Revision != 1 {
		t.Fatalf("create receipt=%+v err=%v", created, err)
	}
	job, err := c.Job(context.Background(), created.ID)
	if err != nil || job.Title != "Build release" || job.Brief != "  preserve brief\n" || job.Status != "active" || job.WorkerID != f.sender {
		t.Fatalf("normalized job=%+v err=%v", job, err)
	}
	replayed, err := c.CreateJob(context.Background(), "job-create-1", client.CreateJobRequest{Title: "Build release", Brief: "  preserve brief\n", WorkerID: f.sender, Status: "active"})
	if err != nil || replayed != created {
		t.Fatalf("normalized exact replay=%+v err=%v", replayed, err)
	}
	_, err = c.CreateJob(context.Background(), "job-create-1", client.CreateJobRequest{Title: "different", Status: "active"})
	checkError(t, err, "idempotency_conflict")

	newTitle := "Ship release"
	updated, err := c.UpdateJob(context.Background(), "job-edit-1", created.ID, client.UpdateJobRequest{ExpectedRevision: 1, Title: &newTitle})
	if err != nil || updated.ID != created.ID || updated.Revision != 2 || updated.Outcome != "updated" {
		t.Fatalf("edit receipt=%+v err=%v", updated, err)
	}
	replayedEdit, err := c.UpdateJob(context.Background(), "job-edit-1", created.ID, client.UpdateJobRequest{ExpectedRevision: 1, Title: &newTitle})
	if err != nil || replayedEdit != updated {
		t.Fatalf("edit replay=%+v err=%v", replayedEdit, err)
	}
	differentTitle := "different body"
	_, err = c.UpdateJob(context.Background(), "job-edit-1", created.ID, client.UpdateJobRequest{ExpectedRevision: 1, Title: &differentTitle})
	checkError(t, err, "idempotency_conflict")
	emptyBrief, emptyWorker := "", ""
	cleared, err := c.UpdateJob(context.Background(), "job-edit-clear", created.ID, client.UpdateJobRequest{ExpectedRevision: 2, Brief: &emptyBrief, WorkerID: &emptyWorker})
	if err != nil || cleared.Revision != 3 || cleared.Outcome != "updated" {
		t.Fatalf("explicit clear receipt=%+v err=%v", cleared, err)
	}
	clearedJob, err := c.Job(context.Background(), created.ID)
	if err != nil || clearedJob.Brief != "" || clearedJob.WorkerID != "" {
		t.Fatalf("empty edit fields were omitted: %+v err=%v", clearedJob, err)
	}
	_, err = c.UpdateJob(context.Background(), "job-edit-stale", created.ID, client.UpdateJobRequest{ExpectedRevision: 1, Title: &newTitle})
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "revision_conflict" || failure.OutcomeUnknown || failure.Details.(map[string]any)["current_revision"] != int64(3) {
		t.Fatalf("stale edit classification/details: %#v, %v", failure, err)
	}

	sent, err := c.SendMessage(context.Background(), "message-send-1", client.SendMessageRequest{RecipientWorkerID: f.recipient, Body: "  hello\n"})
	if err != nil || sent.ID == "" || sent.ConversationID == "" || sent.Outcome != "sent" {
		t.Fatalf("send receipt=%+v err=%v", sent, err)
	}
	replayedMessage, err := c.SendMessage(context.Background(), "message-send-1", client.SendMessageRequest{RecipientWorkerID: f.recipient, Body: "  hello\n"})
	if err != nil || replayedMessage != sent {
		t.Fatalf("send replay=%+v err=%v", replayedMessage, err)
	}
	reply, err := c.SendMessage(context.Background(), "message-reply-1", client.SendMessageRequest{RecipientWorkerID: f.recipient, Body: "reply", ReplyToMessageID: sent.ID})
	if err != nil || reply.ConversationID != sent.ConversationID || reply.ID == sent.ID || reply.Outcome != "sent" {
		t.Fatalf("reply receipt=%+v err=%v", reply, err)
	}
	received := f.newClient(t, f.admin, f.recipient, httpServer.URL)
	_, err = c.MarkSeen(context.Background(), "message-wrong-recipient", sent.ID)
	checkError(t, err, "forbidden")
	seen, err := received.MarkSeen(context.Background(), "message-seen-1", sent.ID)
	if err != nil || seen.Outcome != "seen" || seen.ID != sent.ID || seen.ConversationID != sent.ConversationID {
		t.Fatalf("seen receipt=%+v err=%v", seen, err)
	}
	ack, err := received.Acknowledge(context.Background(), "message-ack-1", sent.ID)
	if err != nil || ack.Outcome != "acknowledged" || ack.ID != sent.ID {
		t.Fatalf("ack receipt=%+v err=%v", ack, err)
	}
	unchanged, err := received.Acknowledge(context.Background(), "message-ack-2", sent.ID)
	if err != nil || unchanged.Outcome != "unchanged" {
		t.Fatalf("unchanged receipt=%+v err=%v", unchanged, err)
	}
	if got := f.eventCount(t) - initialEvents; got != 7 {
		t.Fatalf("expected one create, two edits, two sends, seen and ack events, got %d", got)
	}
	if f.providerCalls.Load() != 1 {
		t.Fatalf("ordinary mutations unexpectedly performed provider work: %d", f.providerCalls.Load())
	}
}

func TestMutationValidationAndRefusalAreKnownBeforeOrAfterDispatch(t *testing.T) {
	f := newMutationFixture(t)
	var calls atomic.Int64
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		f.handler.ServeHTTP(w, r)
	}))
	defer httpServer.Close()
	c := f.newClient(t, f.admin, f.sender, httpServer.URL)
	before := calls.Load()
	for _, key := range []string{"", strings.Repeat("k", 129), "line\nbreak"} {
		_, err := c.CreateJob(context.Background(), key, client.CreateJobRequest{Title: "title"})
		checkError(t, err, "validation_failed")
	}
	_, err := c.UpdateJob(context.Background(), "valid-key", "job-one", client.UpdateJobRequest{ExpectedRevision: 0, Title: ptr("title")})
	checkError(t, err, "invalid_revision")
	_, err = c.SendMessage(context.Background(), "valid-key", client.SendMessageRequest{RecipientWorkerID: f.recipient, Body: " \n"})
	checkError(t, err, "validation_failed")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.CreateJob(canceled, "cancel-before", client.CreateJobRequest{Title: "title"})
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "canceled" || failure.OutcomeUnknown {
		t.Fatalf("pre-dispatch cancel should be known: %#v, %v", failure, err)
	}
	if calls.Load() != before {
		t.Fatal("invalid/pre-canceled mutation reached HTTP")
	}

	readOnlyToken := "read-only-synthetic-token"
	sum := sha256.Sum256([]byte(readOnlyToken))
	f.call(t, http.MethodPost, "/api/v1/projects/"+f.project+"/credentials", f.admin, "", map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": []string{"read"}})
	readonly := f.newClient(t, readOnlyToken, f.sender, httpServer.URL)
	_, err = readonly.CreateJob(context.Background(), "read-only-write", client.CreateJobRequest{Title: "no"})
	if !errors.As(err, &failure) || failure.Code != "forbidden" || failure.OutcomeUnknown {
		t.Fatalf("verified authorization refusal classification: %#v, %v", failure, err)
	}
}

func TestMutationsCannotCrossProjectAndReadOnlyCredentialsCannotWrite(t *testing.T) {
	f := newMutationFixture(t)
	server := httptest.NewServer(f.handler)
	defer server.Close()
	clientOne := f.newClient(t, f.admin, f.sender, server.URL)

	foreignProject := f.call(t, http.MethodPost, "/api/v1/projects", f.admin, "", map[string]string{"name": "other", "repository": "other/repository"})["id"].(string)
	foreignWorker := f.call(t, http.MethodPost, "/api/v1/projects/"+foreignProject+"/workers", f.admin, "", map[string]string{"name": "Other"})["id"].(string)
	foreignJob := f.call(t, http.MethodPost, "/api/v1/projects/"+foreignProject+"/jobs", f.admin, foreignWorker, map[string]string{"title": "foreign", "brief": "", "worker_id": foreignWorker, "status": "active"})["id"].(string)
	newTitle := "cross project"
	_, err := clientOne.UpdateJob(context.Background(), "cross-project-edit", foreignJob, client.UpdateJobRequest{ExpectedRevision: 1, Title: &newTitle})
	checkError(t, err, "not_found")
	foreignMessage := f.call(t, http.MethodPost, "/api/v1/projects/"+foreignProject+"/messages", f.admin, foreignWorker, map[string]string{"recipient_worker_id": foreignWorker, "body": "foreign"})["id"].(string)
	_, err = clientOne.SendMessage(context.Background(), "cross-project-reply", client.SendMessageRequest{RecipientWorkerID: f.recipient, Body: "cross project reply", ReplyToMessageID: foreignMessage})
	checkError(t, err, "not_found")

	readOnlyToken := "read-only-synthetic-token-two"
	sum := sha256.Sum256([]byte(readOnlyToken))
	f.call(t, http.MethodPost, "/api/v1/projects/"+f.project+"/credentials", f.admin, "", map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": []string{"read"}})
	readonly := f.newClient(t, readOnlyToken, f.sender, server.URL)
	_, err = readonly.CreateJob(context.Background(), "read-only-create", client.CreateJobRequest{Title: "denied"})
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "forbidden" || failure.OutcomeUnknown {
		t.Fatalf("read-only mutation refusal: %#v, %v", failure, err)
	}
}

func TestLostMutationResponseIsUnknownAndExactKeyRecoversWithoutRetry(t *testing.T) {
	f := newMutationFixture(t)
	var drop atomic.Bool
	var requests atomic.Int64
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			f.handler.ServeHTTP(w, r)
			return
		}
		requests.Add(1)
		if !drop.Load() {
			f.handler.ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		f.handler.ServeHTTP(recorder, r)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer httpServer.Close()
	c := f.newClient(t, f.admin, f.sender, httpServer.URL)
	beforeEvents := f.eventCount(t)
	drop.Store(true)
	request := client.CreateJobRequest{Title: "recover me", Brief: "same body"}
	_, err := c.CreateJob(context.Background(), "lost-response-key", request)
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || failure.Code != "connection_failed" {
		t.Fatalf("lost response must be unknown: %#v, %v", failure, err)
	}
	checkError(t, err, "connection_failed", "lost-response-key", "same body")
	if requests.Load() != 1 {
		t.Fatalf("client automatically retried mutation: %d requests", requests.Load())
	}
	drop.Store(false)
	recovered, err := c.CreateJob(context.Background(), "lost-response-key", request)
	if err != nil || recovered.ID == "" || recovered.Outcome != "created" {
		t.Fatalf("exact key recovery=%+v err=%v", recovered, err)
	}
	if requests.Load() != 2 || f.eventCount(t)-beforeEvents != 1 {
		t.Fatalf("recovery effects requests=%d events=%d", requests.Load(), f.eventCount(t)-beforeEvents)
	}
}

func TestMalformedMutationReceiptIsUnknownAndDeadlinesAreClassified(t *testing.T) {
	var block atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if block.Load() {
			time.Sleep(100 * time.Millisecond)
			return
		}
		write(w, http.StatusCreated, map[string]any{"id": "job-one", "revision": 1, "outcome": "created", "unexpected": "ignored?"})
	}))
	defer server.Close()
	c := newClient(t, server)
	_, err := c.CreateJob(context.Background(), "bad-receipt", client.CreateJobRequest{Title: "title"})
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "invalid_response" || !failure.OutcomeUnknown {
		t.Fatalf("malformed receipt classification: %#v, %v", failure, err)
	}
	checkError(t, err, "invalid_response", "bad-receipt")
	block.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = c.CreateJob(ctx, "deadline-key", client.CreateJobRequest{Title: "title"})
	if !errors.As(err, &failure) || failure.Code != "deadline_exceeded" || !failure.OutcomeUnknown {
		t.Fatalf("dispatched deadline classification: %#v, %v", failure, err)
	}
	cancelCtx, cancelRequest := context.WithCancel(context.Background())
	timer := time.AfterFunc(20*time.Millisecond, cancelRequest)
	_, err = c.CreateJob(cancelCtx, "cancel-after-dispatch", client.CreateJobRequest{Title: "title"})
	timer.Stop()
	cancelRequest()
	if !errors.As(err, &failure) || failure.Code != "canceled" || !failure.OutcomeUnknown {
		t.Fatalf("dispatched cancel classification: %#v, %v", failure, err)
	}
}

func ptr(value string) *string { return &value }

func TestMutationServiceErrorsPreserveSafeDiagnosticsAndUncertainty(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		code     string
		mode     string
		wantCode string
		unknown  bool
	}{
		{"overloaded", 503, "overloaded", "valid", "overloaded", true},
		{"internal", 500, "internal_error", "valid", "internal_error", true},
		{"known-refusal", 409, "revision_conflict", "valid", "revision_conflict", false},
		{"malformed", 503, "overloaded", "malformed", "unexpected_status", true},
		{"invalid-code", 500, "InvalidCode", "valid", "unexpected_status", true},
		{"missing-request-id", 500, "internal_error", "missing-id", "unexpected_status", true},
		{"credential-reflection", 503, "overloaded", "credential", "unexpected_status", true},
		{"path-reflection", 500, "internal_error", "path", "unexpected_status", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected := binding(t, "http://127.0.0.1:1")
			var calls atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				calls.Add(1)
				if r.Method != "POST" || r.Header.Get("Idempotency-Key") != "same-key" {
					t.Error("mutation identity changed")
				}
				if test.mode == "malformed" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(test.status)
					_, _ = w.Write([]byte(`{"error":`))
					return
				}
				message := "RAW_PROVIDER_FAILURE_MUST_NOT_REACH_OUTPUT"
				requestID := "request-safe-id"
				details := map[string]any{"current_revision": 3, "project_id": "project-one", "provider_debug": "FILTERED_SECRET_DETAIL"}
				switch test.mode {
				case "missing-id":
					requestID = ""
				case "credential":
					details["private_credential"] = testToken
				case "path":
					details["private_path"] = selected.CredentialFile
				}
				write(w, test.status, map[string]any{"error": map[string]any{"code": test.code, "message": message, "details": details}, "request_id": requestID})
			}))
			defer service.Close()
			selected.Context.ServerURL = service.URL
			c, err := client.New(context.Background(), selected)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_, err = c.CreateJob(context.Background(), "same-key", client.CreateJobRequest{Title: "request title"})
			checkError(t, err, test.wantCode, selected.CredentialFile, "RAW_PROVIDER_FAILURE_MUST_NOT_REACH_OUTPUT", "FILTERED_SECRET_DETAIL")
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Status != test.status || failure.OutcomeUnknown != test.unknown || calls.Load() != 1 {
				t.Fatalf("classification or no-retry changed: %#v calls=%d", failure, calls.Load())
			}
			valid := test.wantCode == test.code
			if valid {
				if failure.RequestID != "request-safe-id" {
					t.Fatal("safe request ID was discarded")
				}
				fields, ok := failure.Details.(map[string]any)
				if !ok || len(fields) != 2 || fields["current_revision"] != int64(3) || fields["project_id"] != "project-one" {
					t.Fatalf("filtered details lost: %#v", failure.Details)
				}
			} else if failure.RequestID != "" || failure.Details != nil {
				t.Fatal("invalid service body leaked diagnostics")
			}
			if test.unknown && !strings.Contains(failure.Action, "exact same idempotency key") {
				t.Fatal("uncertain mutation lost same-key recovery guidance")
			}
		})
	}
}
