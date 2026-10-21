package mcpadapter_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/benhook1013/FireController/internal/mcpadapter"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mutationAPI struct {
	store                                  *core.Store
	handler                                http.Handler
	project, sender, recipient, credential string
	admin                                  string
	providers                              atomic.Int64
}

func mutationFixture(t *testing.T) *mutationAPI {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	store, err := core.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = jobs.EnsureSchema(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if err = inbox.EnsureSchema(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	f := &mutationAPI{store: store, credential: credential(t)}
	mux := server.New(store, server.WithRepositoryResolver(func(_ context.Context, repo string) (server.RepositoryIdentity, error) {
		return server.RepositoryIdentity{ID: f.providers.Add(1), FullName: repo}, nil
	}))
	jobs.Register(mux, store)
	inbox.Register(mux, store)
	f.handler = mux
	data, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	admin := strings.TrimSpace(string(data))
	f.admin = admin
	setup := func(route string, body any) map[string]any {
		t.Helper()
		encoded, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", route, bytes.NewReader(encoded))
		r.Header.Set("Authorization", "Bearer "+admin)
		r.Header.Set("Idempotency-Key", core.NewID("fixture"))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("setup HTTP %d: %s", w.Code, w.Body.String())
		}
		var result map[string]any
		if json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal("setup JSON")
		}
		return result
	}
	f.project = setup("/api/v1/projects", map[string]string{"name": "project", "repository": "example/adapter"})["id"].(string)
	f.sender = setup("/api/v1/projects/"+f.project+"/workers", map[string]string{"name": "Sender"})["id"].(string)
	f.recipient = setup("/api/v1/projects/"+f.project+"/workers", map[string]string{"name": "Recipient"})["id"].(string)
	sum := sha256.Sum256([]byte(token))
	setup("/api/v1/projects/"+f.project+"/credentials", map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": []string{"read", "write"}})
	return f
}
func (f *mutationAPI) adapter(t *testing.T, base, worker string) *mcp.Server {
	t.Helper()
	c := profile.Context{Profile: "chosen", ServerURL: base, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: worker}
	return adapter(t, profiles(t, map[string]profile.Context{"chosen": c}, f.credential), "chosen")
}
func diagnosticResult(t *testing.T, value *mcp.CallToolResult, code string, unknown bool) mcpadapter.Diagnostic {
	t.Helper()
	errorResult(t, value, code, token)
	data, _ := json.Marshal(value.StructuredContent)
	var out struct {
		Error mcpadapter.Diagnostic `json:"error"`
	}
	if json.Unmarshal(data, &out) != nil || out.Error.OutcomeUnknown != unknown {
		t.Fatal("outcome uncertainty lost or invented")
	}
	return out.Error
}

func TestMutationReceiptsReplayRevisionAndBoundMessageTransitions(t *testing.T) {
	f := mutationFixture(t)
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
			if r.Header.Get("Idempotency-Key") == "" || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("X-FC-Worker") == "" {
				t.Error("mutation binding/key missing")
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	sender := connect(t, f.adapter(t, service.URL, f.sender), "")
	recipient := connect(t, f.adapter(t, service.URL, f.recipient), "")
	// This JSON expands beyond the read-argument bound; the whole accepted brief
	// must survive the mutation transport rather than being silently truncated.
	brief := strings.Repeat("<", 32<<10)
	create := map[string]any{"idempotency_key": "create-explicit", "title": "  First  ", "brief": brief, "worker_id": f.sender}
	var initial, replay client.JobReceipt
	decodeResult(t, call(t, sender, "jobs_create", create), &initial)
	decodeResult(t, call(t, sender, "jobs_create", create), &replay)
	if initial.ID == "" || initial.Revision != 1 || initial.Outcome != "created" || initial != replay {
		t.Fatal("creation receipt/replay")
	}
	compact, _ := json.Marshal(initial)
	if len(compact) > 512 || strings.Contains(string(compact), "brief") {
		t.Fatal("receipt expanded private brief")
	}
	diagnosticResult(t, call(t, sender, "jobs_create", map[string]any{"idempotency_key": "create-explicit", "title": "Changed"}), "idempotency_conflict", false)
	edit := map[string]any{"idempotency_key": "edit-explicit", "job_id": initial.ID, "expected_revision": 1, "brief": "", "worker_id": "", "status": "blocked"}
	var edited, editReplay client.JobReceipt
	decodeResult(t, call(t, sender, "job_update", edit), &edited)
	decodeResult(t, call(t, sender, "job_update", edit), &editReplay)
	if edited.ID != initial.ID || edited.Revision != 2 || edited.Outcome != "updated" || editReplay != edited {
		t.Fatal("edit receipt/replay")
	}
	conflict := diagnosticResult(t, call(t, sender, "job_update", map[string]any{"idempotency_key": "stale-edit", "job_id": initial.ID, "expected_revision": 1, "title": "stale"}), "revision_conflict", false)
	if conflict.RequestID == "" || conflict.Details.(map[string]any)["current_revision"] != float64(2) {
		t.Fatal("safe revision/request identity lost")
	}
	var job client.Job
	decodeResult(t, call(t, sender, "job_read", map[string]any{"job_id": initial.ID}), &job)
	if job.Title != "First" || job.Brief != "" || job.WorkerID != "" || job.Status != "blocked" {
		t.Fatal("normalized edit/clear semantics changed")
	}
	var history client.Page[client.JobHistoryEntry]
	decodeResult(t, call(t, sender, "job_history", map[string]any{"job_id": initial.ID}), &history)
	if len(history.Items) != 2 || history.Items[0].State.Brief != brief || history.Items[1].State.Brief != "" {
		t.Fatal("mutation/history consistency")
	}
	body := strings.Repeat("<", 16<<10)
	send := map[string]any{"idempotency_key": "send-explicit", "recipient_worker_id": f.recipient, "body": body}
	var message, messageReplay client.MessageReceipt
	decodeResult(t, call(t, sender, "inbox_send", send), &message)
	decodeResult(t, call(t, sender, "inbox_send", send), &messageReplay)
	if message.ID == "" || message.ConversationID == "" || message.Outcome != "sent" || message != messageReplay {
		t.Fatal("message receipt/replay")
	}
	var reply client.MessageReceipt
	decodeResult(t, call(t, recipient, "inbox_send", map[string]any{"idempotency_key": "reply-explicit", "recipient_worker_id": f.sender, "body": "  reply body\n", "reply_to_message_id": message.ID}), &reply)
	if reply.ConversationID != message.ConversationID || reply.ID == message.ID {
		t.Fatal("reply routing changed")
	}
	diagnosticResult(t, call(t, sender, "inbox_ack", map[string]any{"idempotency_key": "wrong-recipient", "message_id": message.ID}), "forbidden", false)
	var seen, ack, unchanged client.MessageReceipt
	decodeResult(t, call(t, recipient, "inbox_seen", map[string]any{"idempotency_key": "seen-explicit", "message_id": message.ID}), &seen)
	decodeResult(t, call(t, recipient, "inbox_ack", map[string]any{"idempotency_key": "ack-explicit", "message_id": message.ID}), &ack)
	decodeResult(t, call(t, recipient, "inbox_seen", map[string]any{"idempotency_key": "seen-again", "message_id": message.ID}), &unchanged)
	if seen.ID != message.ID || seen.Outcome != "seen" || ack.Outcome != "acknowledged" || unchanged.Outcome != "unchanged" {
		t.Fatal("transition outcomes changed")
	}
	var count, seenState, ackState int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM jobs").Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate job")
	}
	if err := f.store.DB().QueryRow("SELECT count(*) FROM inbox_messages").Scan(&count); err != nil || count != 2 {
		t.Fatal("duplicate message")
	}
	if err := f.store.DB().QueryRow("SELECT seen,acknowledged FROM inbox_recipient_state WHERE message_id=?", message.ID).Scan(&seenState, &ackState); err != nil || seenState != 1 || ackState != 1 {
		t.Fatal("recipient transition not durable")
	}
	if writes.Load() != 13 || f.providers.Load() != 1 {
		t.Fatalf("unexpected retry/provider work: writes=%d providers=%d", writes.Load(), f.providers.Load())
	}
}

func TestMutationArgumentsRejectBeforeCredentialAndHTTP(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer service.Close()
	c := localContext()
	c.ServerURL = service.URL
	path := profiles(t, map[string]profile.Context{"alpha": c}, "absent-credential")
	session := connect(t, adapter(t, path, "alpha"), "")
	cases := []struct{ name, raw string }{
		{"jobs_create", `{"title":"x"}`}, {"jobs_create", `{"idempotency_key":"","title":"x"}`}, {"jobs_create", `{"idempotency_key":" key","title":"x"}`}, {"jobs_create", `{"idempotency_key":"é","title":"x"}`}, {"jobs_create", `{"idempotency_key":"a\nb","title":"x"}`}, {"jobs_create", `{"idempotency_key":"` + strings.Repeat("k", 129) + `","title":"x"}`}, {"jobs_create", `{"idempotency_key":"k","title":" "}`}, {"jobs_create", `{"idempotency_key":"k","title":"` + strings.Repeat("x", 241) + `"}`}, {"jobs_create", `{"idempotency_key":"k","title":"x","brief":"` + strings.Repeat("x", (32<<10)+1) + `"}`}, {"jobs_create", `{"idempotency_key":"k","title":"x","worker_id":"../other"}`}, {"jobs_create", `{"idempotency_key":"k","title":"x","status":"ready"}`}, {"jobs_create", `{"idempotency_key":"k","title":"x","profile":"other"}`}, {"jobs_create", `{"idempotency_key":"k","idempotency_key":"other","title":"x"}`}, {"jobs_create", `{"idempotency_key":"k","Title":"x"}`}, {"jobs_create", `{"idempotency_key":"k","title":null}`}, {"jobs_create", `{"idempotency_key":"k","title":"bad\ud800"}`},
		{"job_update", `{"idempotency_key":"k","job_id":"job","expected_revision":1}`}, {"job_update", `{"idempotency_key":"k","job_id":"job","title":"x"}`}, {"job_update", `{"idempotency_key":"k","job_id":"job","expected_revision":0,"title":"x"}`}, {"job_update", `{"idempotency_key":"k","job_id":"job","expected_revision":1.5,"title":"x"}`}, {"job_update", `{"idempotency_key":"k","job_id":"job","expected_revision":9223372036854775807,"title":"x"}`}, {"job_update", `{"idempotency_key":"k","job_id":"job","expected_revision":9223372036854775808,"title":"x"}`}, {"job_update", `{"idempotency_key":"k","job_id":"job","expected_revision":1,"brief":null}`}, {"job_update", `{"idempotency_key":"k","job_id":"job","expected_revision":1,"status":""}`}, {"job_update", `{"idempotency_key":"k","job_id":"..","expected_revision":1,"title":"x"}`},
		{"inbox_send", `{"idempotency_key":"k","recipient_worker_id":"worker","body":" "}`}, {"inbox_send", `{"idempotency_key":"k","recipient_worker_id":"worker","body":"` + strings.Repeat("x", (16<<10)+1) + `"}`}, {"inbox_send", `{"idempotency_key":"k","recipient_worker_id":"worker","body":"x","reply_to_message_id":"a/b"}`}, {"inbox_send", `{"idempotency_key":"k","recipient_worker_id":"worker","body":"x","sender_worker_id":"other"}`}, {"inbox_send", `{"idempotency_key":"k","recipient_worker_id":"worker","body":"x","reply_to_message_id":null}`},
		{"inbox_seen", `{"message_id":"message"}`}, {"inbox_seen", `{"idempotency_key":"k","message_id":".."}`}, {"inbox_ack", `{"idempotency_key":"k","message_id":"message","worker_id":"other"}`}, {"inbox_ack", `{"idempotency_key":"k","message_id":null}`},
	}
	for i, tc := range cases {
		t.Run(tc.name+string(rune('A'+i)), func(t *testing.T) {
			diagnosticResult(t, call(t, session, tc.name, json.RawMessage(tc.raw)), "invalid_arguments", false)
		})
	}
	if requests.Load() != 0 {
		t.Fatal("invalid mutation read credential or contacted service")
	}
}

func TestUnknownMutationOutcomeSurvivesAdapterAndExplicitRetry(t *testing.T) {
	for _, mode := range []string{"connection-drop", "malformed-receipt", "deadline-after-commit"} {
		t.Run(mode, func(t *testing.T) {
			f := mutationFixture(t)
			var writes atomic.Int64
			var first atomic.Bool
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/jobs") {
					writes.Add(1)
					if first.CompareAndSwap(false, true) {
						rec := httptest.NewRecorder()
						f.handler.ServeHTTP(rec, r)
						if rec.Code != 201 {
							t.Error("setup mutation did not commit")
						}
						switch mode {
						case "connection-drop":
							conn, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							conn.Close()
							return
						case "malformed-receipt":
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(201)
							w.Write([]byte(`{"id":"wrong","revision":1}`))
							return
						case "deadline-after-commit":
							select {
							case <-r.Context().Done():
							case <-time.After(2 * time.Second):
							}
							return
						}
					}
				}
				f.handler.ServeHTTP(w, r)
			}))
			defer service.Close()
			adapter := f.adapter(t, service.URL, f.sender)
			if mode == "deadline-after-commit" {
				var bounded atomic.Bool
				adapter.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
					return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
						if method == "tools/call" && bounded.CompareAndSwap(false, true) {
							ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
							defer cancel()
							return next(ctx, method, req)
						}
						return next(ctx, method, req)
					}
				})
			}
			session := connect(t, adapter, "")
			request := map[string]any{"idempotency_key": "supplied-recovery-key", "title": "commit-once"}
			code := "connection_failed"
			if mode == "malformed-receipt" {
				code = "invalid_response"
			} else if mode == "deadline-after-commit" {
				code = "deadline_exceeded"
			}
			failure := diagnosticResult(t, call(t, session, "jobs_create", request), code, true)
			if !strings.Contains(failure.Action, "same idempotency key") || writes.Load() != 1 {
				t.Fatal("unknown outcome lost explicit recovery or retried automatically")
			}
			var receipt client.JobReceipt
			decodeResult(t, call(t, session, "jobs_create", request), &receipt)
			var count int
			if err := f.store.DB().QueryRow("SELECT count(*) FROM jobs WHERE id=?", receipt.ID).Scan(&count); err != nil || count != 1 {
				t.Fatal("same-key recovery lost committed job")
			}
			if err := f.store.DB().QueryRow("SELECT count(*) FROM job_history").Scan(&count); err != nil || count != 1 {
				t.Fatal("recovery duplicated history")
			}
			if err := f.store.DB().QueryRow("SELECT count(*) FROM receipts WHERE operation='jobs.create'").Scan(&count); err != nil || count != 1 {
				t.Fatal("recovery duplicated receipt")
			}
			if writes.Load() != 2 || f.providers.Load() != 1 {
				t.Fatal("retry/provider count changed")
			}
		})
	}
}
