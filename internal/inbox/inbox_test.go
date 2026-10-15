package inbox_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/server"
)

type fixture struct {
	t                                      *testing.T
	store                                  *core.Store
	mux                                    *http.ServeMux
	directory                              string
	admin                                  string
	a, b                                   string
	sender, recipient, bSender, bRecipient string
	clientA, clientB, readonly             string
	credentialA                            string
	providerCalls                          int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, directory: filepath.Join(t.TempDir(), "private-state"), clientA: "synthetic-project-alpha-client-token", clientB: "synthetic-project-beta-client-token", readonly: "synthetic-readonly-client-token"}
	f.open()
	t.Cleanup(func() {
		if f.store != nil {
			_ = f.store.Close()
		}
	})
	data, err := os.ReadFile(filepath.Join(f.directory, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f.admin = strings.TrimSpace(string(data))
	f.a = f.create("/api/v1/projects", map[string]any{"name": "Alpha", "repository": "example/alpha"}, "")
	f.b = f.create("/api/v1/projects", map[string]any{"name": "Beta", "repository": "example/beta"}, "")
	f.sender = f.create(f.path(f.a, "workers"), map[string]any{"name": "Builder"}, "")
	f.recipient = f.create(f.path(f.a, "workers"), map[string]any{"name": "Overseer"}, "")
	f.bSender = f.create(f.path(f.b, "workers"), map[string]any{"name": "Builder"}, "")
	f.bRecipient = f.create(f.path(f.b, "workers"), map[string]any{"name": "Overseer"}, "")
	f.credentialA = f.grant(f.a, f.clientA, []string{"read", "write"})
	f.grant(f.b, f.clientB, []string{"read", "write"})
	f.grant(f.a, f.readonly, []string{"read"})
	if f.providerCalls != 2 {
		t.Fatalf("bootstrap provider calls=%d", f.providerCalls)
	}
	return f
}

func (f *fixture) open() {
	f.t.Helper()
	var err error
	f.store, err = core.Open(context.Background(), f.directory)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = inbox.EnsureSchema(context.Background(), f.store); err != nil {
		f.t.Fatal(err)
	}
	f.mux = server.New(f.store, server.WithRepositoryResolver(func(_ context.Context, repository string) (server.RepositoryIdentity, error) {
		f.providerCalls++
		id := int64(101)
		if repository == "example/beta" {
			id = 202
		}
		return server.RepositoryIdentity{ID: id, FullName: repository}, nil
	}))
	inbox.Register(f.mux, f.store)
}

func (f *fixture) path(project, suffix string) string {
	return "/api/v1/projects/" + project + "/" + suffix
}

func (f *fixture) request(method, path, token, worker, key string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	return f.raw(method, path, token, worker, key, data)
}

func (f *fixture) raw(method, path, token, worker, key string, data []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if worker != "" {
		r.Header.Set("X-FC-Worker", worker)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if len(data) > 0 {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder, want int, out any) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("HTTP %d want %d: %s", w.Code, want, w.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder, want int, code string) {
	t.Helper()
	var data struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decode(t, w, want, &data)
	if data.Error.Code != code {
		t.Fatalf("error=%s want=%s", data.Error.Code, code)
	}
}

func (f *fixture) create(path string, body any, key string) string {
	f.t.Helper()
	if key == "" {
		key = core.NewID("test")
	}
	var result struct {
		ID string `json:"id"`
	}
	decode(f.t, f.request("POST", path, f.admin, "", key, body), http.StatusOK, &result)
	if result.ID == "" {
		f.t.Fatal("empty created ID")
	}
	return result.ID
}

func (f *fixture) grant(project, token string, permissions []string) string {
	sum := sha256.Sum256([]byte(token))
	return f.create(f.path(project, "credentials"), map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": permissions}, "")
}

type receipt struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	Outcome        string `json:"outcome"`
}
type item struct {
	ID                string `json:"id"`
	ConversationID    string `json:"conversation_id"`
	SenderWorkerID    string `json:"sender_worker_id"`
	RecipientWorkerID string `json:"recipient_worker_id"`
	Body              string `json:"body"`
	Preview           string `json:"preview"`
	PreviewTruncated  bool   `json:"preview_truncated"`
	Seen              bool   `json:"seen"`
	Acknowledged      bool   `json:"acknowledged"`
	ReplyToMessageID  string `json:"reply_to_message_id"`
}
type page struct {
	Items         []item `json:"items"`
	Cursor        string `json:"cursor"`
	HasMore       bool   `json:"has_more"`
	RootMessageID string `json:"root_message_id"`
}

func (f *fixture) send(project, token, sender, recipient, body, reply, key string) receipt {
	f.t.Helper()
	if key == "" {
		key = core.NewID("test")
	}
	var result receipt
	w := f.request("POST", f.path(project, "messages"), token, sender, key, map[string]any{"recipient_worker_id": recipient, "body": body, "reply_to_message_id": reply})
	decode(f.t, w, http.StatusCreated, &result)
	if result.ID == "" || result.ConversationID == "" || result.Outcome != "sent" {
		f.t.Fatalf("bad receipt: %+v", result)
	}
	if strings.Contains(w.Body.String(), body) {
		f.t.Fatal("send receipt echoed body")
	}
	return result
}

func (f *fixture) inbox(project, token, worker, query string) page {
	f.t.Helper()
	var result page
	decode(f.t, f.request("GET", f.path(project, "inbox/"+worker)+query, token, "", "", nil), http.StatusOK, &result)
	return result
}

func (f *fixture) transition(project, token, worker, id, action, key string) receipt {
	f.t.Helper()
	if key == "" {
		key = core.NewID("test")
	}
	var result receipt
	decode(f.t, f.request("POST", f.path(project, "messages/"+id+"/"+action), token, worker, key, struct{}{}), http.StatusOK, &result)
	return result
}

func (f *fixture) eventCount(project string) int {
	f.t.Helper()
	var count int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM events WHERE project_id=? AND event_type LIKE 'message.%'", project).Scan(&count); err != nil {
		f.t.Fatal(err)
	}
	return count
}

func TestMessagesScopeAndStrictAdmission(t *testing.T) {
	f := setup(t)
	body := map[string]any{"recipient_worker_id": f.recipient, "body": "private synthetic body"}
	path := f.path(f.a, "messages")
	errorCode(t, f.request("POST", path, "", f.sender, "key", body), http.StatusUnauthorized, "unauthorized")
	errorCode(t, f.request("POST", path, f.readonly, f.sender, "key", body), http.StatusForbidden, "forbidden")
	errorCode(t, f.request("POST", path, f.clientB, f.sender, "key", body), http.StatusForbidden, "forbidden")
	errorCode(t, f.request("POST", path, f.clientA, "", "key", body), http.StatusBadRequest, "validation_failed")
	errorCode(t, f.request("POST", path, f.clientA, f.sender, "", body), http.StatusBadRequest, "validation_failed")
	errorCode(t, f.request("POST", path+"?unsupported=true", f.clientA, f.sender, "key", body), http.StatusBadRequest, "validation_failed")
	for _, recipient := range []string{"missing", f.bRecipient} {
		w := f.request("POST", path, f.clientA, f.sender, core.NewID("test"), map[string]any{"recipient_worker_id": recipient, "body": "synthetic"})
		if w.Code < 400 || w.Code >= 500 {
			t.Fatalf("invalid recipient accepted: %d %s", w.Code, w.Body.String())
		}
	}
	for _, sender := range []string{"missing", f.bSender} {
		w := f.request("POST", path, f.clientA, sender, core.NewID("test"), body)
		if w.Code < 400 || w.Code >= 500 {
			t.Fatalf("invalid sender accepted: %d %s", w.Code, w.Body.String())
		}
	}
	for _, text := range []string{"", " \t\n", strings.Repeat("x", inbox.MaxBodyBytes+1), strings.Repeat("雪", inbox.MaxBodyBytes/3+1)} {
		errorCode(t, f.request("POST", path, f.clientA, f.sender, core.NewID("test"), map[string]any{"recipient_worker_id": f.recipient, "body": text}), http.StatusBadRequest, "validation_failed")
	}
	for _, raw := range []string{`{"recipient_worker_id":"` + f.recipient + `","body":"first","body":"second"}`, `{"recipient_worker_id":"` + f.recipient + `","Body":"synthetic"}`, `{"recipient_worker_id":"` + f.recipient + `","body":"synthetic","unknown":1}`, `{"recipient_worker_id":"` + f.recipient + `","body":"broken-\ud800"}`} {
		w := f.raw("POST", path, f.clientA, f.sender, core.NewID("test"), []byte(raw))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("strict JSON accepted: %d %s", w.Code, w.Body.String())
		}
	}
	if f.eventCount(f.a) != 0 {
		t.Fatal("admission failure emitted message event")
	}
	result := f.send(f.a, f.clientA, f.sender, f.recipient, strings.Repeat("x", inbox.MaxBodyBytes), "", "")
	var thread page
	decode(t, f.request("GET", f.path(f.a, "conversations/"+result.ConversationID), f.readonly, "", "", nil), http.StatusOK, &thread)
	if len(thread.Items) != 1 || len(thread.Items[0].Body) != inbox.MaxBodyBytes {
		t.Fatal("bounded full body not retained")
	}
	if f.providerCalls != 2 {
		t.Fatal("messaging called repository provider")
	}
}

func TestRetryReceiptsAndRecipientOnlyState(t *testing.T) {
	f := setup(t)
	key := "send-lost-response"
	sent := f.send(f.a, f.clientA, f.sender, f.recipient, "synthetic-private-message", "", key)
	if again := f.send(f.a, f.clientA, f.sender, f.recipient, "synthetic-private-message", "", key); again != sent {
		t.Fatal("send retry changed receipt")
	}
	errorCode(t, f.request("POST", f.path(f.a, "messages"), f.clientA, f.sender, key, map[string]any{"recipient_worker_id": f.recipient, "body": "different"}), http.StatusConflict, "idempotency_conflict")
	errorCode(t, f.request("POST", f.path(f.a, "messages"), f.clientA, f.recipient, key, map[string]any{"recipient_worker_id": f.recipient, "body": "synthetic-private-message"}), http.StatusConflict, "idempotency_conflict")
	if f.eventCount(f.a) != 1 {
		t.Fatal("replay/conflict emitted another event")
	}
	for _, action := range []string{"seen", "ack"} {
		errorCode(t, f.request("POST", f.path(f.a, "messages/"+sent.ID+"/"+action), f.clientA, f.sender, core.NewID("test"), struct{}{}), http.StatusForbidden, "forbidden")
	}
	before := f.inbox(f.a, f.clientA, f.recipient, "")
	var thread page
	decode(t, f.request("GET", f.path(f.a, "conversations/"+sent.ConversationID), f.clientA, "", "", nil), http.StatusOK, &thread)
	after := f.inbox(f.a, f.clientA, f.recipient, "")
	if len(before.Items) != 1 || len(after.Items) != 1 || before.Items[0].Seen || after.Items[0].Seen || after.Items[0].Acknowledged || f.eventCount(f.a) != 1 {
		t.Fatal("read changed recipient state")
	}
	ack := f.transition(f.a, f.clientA, f.recipient, sent.ID, "ack", "ack-key")
	if ack.Outcome != "acknowledged" {
		t.Fatal("ack did not change outcome")
	}
	if again := f.transition(f.a, f.clientA, f.recipient, sent.ID, "ack", "ack-key"); again != ack {
		t.Fatal("ack retry changed receipt")
	}
	if got := f.transition(f.a, f.clientA, f.recipient, sent.ID, "ack", ""); got.Outcome != "unchanged" {
		t.Fatal("repeated ack with new key not no-op")
	}
	if got := f.transition(f.a, f.clientA, f.recipient, sent.ID, "seen", ""); got.Outcome != "unchanged" {
		t.Fatal("seen after ack not no-op")
	}
	state := f.inbox(f.a, f.clientA, f.recipient, "")
	if !state.Items[0].Seen || !state.Items[0].Acknowledged || f.eventCount(f.a) != 2 {
		t.Fatal("ack failed atomic seen/no-op event semantics")
	}
	if got := f.send(f.a, f.clientA, f.sender, f.recipient, "synthetic-private-message", "", key); got != sent {
		t.Fatal("send replay after state edit changed receipt")
	}
	var count int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM inbox_messages WHERE project_id=?", f.a).Scan(&count); err != nil || count != 1 {
		t.Fatalf("message count=%d err=%v", count, err)
	}
	rows, err := f.store.DB().Query("SELECT payload_json FROM events WHERE project_id=? AND event_type LIKE 'message.%'", f.a)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(payload, "synthetic-private-message") {
			t.Fatal("event leaked message body")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	decode(t, f.request("DELETE", f.path(f.a, "credentials/"+f.credentialA), f.admin, "", "revoke", nil), http.StatusOK, nil)
	w := f.request("POST", f.path(f.a, "messages"), f.clientA, f.sender, key, map[string]any{"recipient_worker_id": f.recipient, "body": "synthetic-private-message"})
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
		t.Fatalf("revoked replay=%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), sent.ID) || f.eventCount(f.a) != 2 {
		t.Fatal("revoked replay exposed receipt or emitted event")
	}
	if f.providerCalls != 2 {
		t.Fatal("ordinary coordination called provider")
	}
}

func TestConversationAndInboxPagingFilterBeforeLimit(t *testing.T) {
	f := setup(t)
	root := f.send(f.a, f.clientA, f.sender, f.recipient, "root synthetic message", "", "")
	second := f.send(f.a, f.clientA, f.sender, f.recipient, "second synthetic message", root.ID, "")
	third := f.send(f.a, f.clientA, f.sender, f.recipient, strings.Repeat("雪", inbox.PreviewRunes+10), second.ID, "")
	fourth := f.send(f.a, f.clientA, f.recipient, f.sender, "reverse synthetic reply", root.ID, "")
	for _, sent := range []receipt{second, third, fourth} {
		if sent.ConversationID != root.ConversationID {
			t.Fatal("reply forked conversation")
		}
	}
	// Equal timestamps are a synthetic fixture for the public chronological page
	// contract; durable insertion sequence must still establish a stable order.
	if _, err := f.store.DB().Exec("UPDATE inbox_messages SET created_at='2026-01-01T00:00:00Z' WHERE project_id=?", f.a); err != nil {
		t.Fatal(err)
	}
	var thread page
	decode(t, f.request("GET", f.path(f.a, "conversations/"+root.ConversationID)+"?limit=2", f.clientA, "", "", nil), http.StatusOK, &thread)
	if len(thread.Items) != 2 || thread.Items[0].ID != root.ID || thread.Items[1].ID != second.ID || !thread.HasMore || thread.Cursor == "" || thread.RootMessageID != root.ID {
		t.Fatalf("first thread page: %+v", thread)
	}
	var rest page
	decode(t, f.request("GET", f.path(f.a, "conversations/"+root.ConversationID)+"?limit=2&cursor="+url.QueryEscape(thread.Cursor), f.clientA, "", "", nil), http.StatusOK, &rest)
	if len(rest.Items) != 2 || rest.Items[0].ID != third.ID || rest.Items[1].ID != fourth.ID || rest.HasMore || rest.Items[0].ReplyToMessageID != second.ID {
		t.Fatalf("remaining thread page: %+v", rest)
	}
	f.transition(f.a, f.clientA, f.recipient, root.ID, "ack", "")
	f.transition(f.a, f.clientA, f.recipient, second.ID, "seen", "")
	filtered := f.inbox(f.a, f.clientA, f.recipient, "?unread=true&limit=1")
	if len(filtered.Items) != 1 || filtered.Items[0].ID != third.ID || filtered.HasMore || !filtered.Items[0].PreviewTruncated || utf8.RuneCountInString(filtered.Items[0].Preview) != inbox.PreviewRunes || !utf8.ValidString(filtered.Items[0].Preview) {
		t.Fatalf("unread page=%+v", filtered)
	}
	unack := f.inbox(f.a, f.clientA, f.recipient, "?unacknowledged=true&limit=1")
	if len(unack.Items) != 1 || unack.Items[0].ID != second.ID || !unack.HasMore || unack.Cursor == "" {
		t.Fatalf("unack page=%+v", unack)
	}
	remaining := f.inbox(f.a, f.clientA, f.recipient, "?unacknowledged=true&limit=1&cursor="+url.QueryEscape(unack.Cursor))
	if len(remaining.Items) != 1 || remaining.Items[0].ID != third.ID || remaining.HasMore {
		t.Fatalf("unack rest=%+v", remaining)
	}
	if got := f.inbox(f.b, f.clientB, f.bRecipient, ""); len(got.Items) != 0 {
		t.Fatal("same named other-project worker received message")
	}
	if got := f.inbox(f.a, f.clientA, f.sender, ""); len(got.Items) != 1 || got.Items[0].ID != fourth.ID {
		t.Fatal("reverse reply recipient scope wrong")
	}
	for _, query := range []string{"?unread=1", "?unread=", "?unread=%zz", "?unread=true&unread=false", "?limit=0", "?limit=101", "?unsupported=true", "?cursor=" + url.QueryEscape(unack.Cursor)} {
		w := f.request("GET", f.path(f.a, "inbox/"+f.recipient)+query, f.clientA, "", "", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("bad query accepted: %s HTTP %d %s", query, w.Code, w.Body.String())
		}
	}
	if f.providerCalls != 2 {
		t.Fatal("paging called provider")
	}
}

func TestCrossProjectIDsAndCursorsNeverDiscloseOrMutate(t *testing.T) {
	f := setup(t)
	first := f.send(f.a, f.clientA, f.sender, f.recipient, "private alpha synthetic body", "", "")
	f.send(f.a, f.clientA, f.sender, f.recipient, "next alpha synthetic body", "", "")
	alpha := f.inbox(f.a, f.clientA, f.recipient, "?limit=1")
	if alpha.Cursor == "" {
		t.Fatal("missing cursor")
	}
	for _, path := range []string{f.path(f.b, "conversations/"+first.ConversationID), f.path(f.b, "inbox/"+f.recipient)} {
		errorCode(t, f.request("GET", path, f.clientB, "", "", nil), http.StatusNotFound, "not_found")
	}
	w := f.request("GET", f.path(f.b, "inbox/"+f.bRecipient)+"?limit=1&cursor="+url.QueryEscape(alpha.Cursor), f.clientB, "", "", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cross-project cursor accepted: %d %s", w.Code, w.Body.String())
	}
	w = f.request("GET", f.path(f.a, "inbox/"+f.recipient)+"?limit=1&cursor="+url.QueryEscape(alpha.Cursor), f.readonly, "", "", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatal("another principal reused cursor")
	}
	errorCode(t, f.request("POST", f.path(f.b, "messages"), f.clientB, f.bSender, "cross-reply", map[string]any{"recipient_worker_id": f.bRecipient, "body": "synthetic reply", "reply_to_message_id": first.ID}), http.StatusNotFound, "not_found")
	for _, action := range []string{"seen", "ack"} {
		errorCode(t, f.request("POST", f.path(f.b, "messages/"+first.ID+"/"+action), f.clientB, f.bRecipient, core.NewID("test"), struct{}{}), http.StatusNotFound, "not_found")
	}
	if f.eventCount(f.b) != 0 || f.eventCount(f.a) != 2 {
		t.Fatal("cross-scope reference changed events")
	}
	if got := f.inbox(f.a, f.clientA, f.recipient, ""); len(got.Items) != 2 || got.Items[0].Seen || got.Items[0].Acknowledged {
		t.Fatal("cross-scope mutation changed alpha state")
	}
}

func TestMessagesAndReceiptsSurviveRestart(t *testing.T) {
	f := setup(t)
	sent := f.send(f.a, f.clientA, f.sender, f.recipient, "durable synthetic message", "", "durable-send")
	f.transition(f.a, f.clientA, f.recipient, sent.ID, "ack", "durable-ack")
	instance := f.store.InstanceID()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store = nil
	f.open()
	if f.store.InstanceID() != instance {
		t.Fatal("ordinary restart changed identity")
	}
	if got := f.send(f.a, f.clientA, f.sender, f.recipient, "durable synthetic message", "", "durable-send"); got != sent {
		t.Fatal("restart lost receipt")
	}
	if got := f.inbox(f.a, f.clientA, f.recipient, ""); len(got.Items) != 1 || !got.Items[0].Seen || !got.Items[0].Acknowledged {
		t.Fatal("restart lost recipient state")
	}
	if f.eventCount(f.a) != 2 || f.providerCalls != 2 {
		t.Fatal("restart retry repeated effect/provider call")
	}
}

func TestEventFailureRollsBackMessageAndRetryCanCommit(t *testing.T) {
	f := setup(t)
	if _, err := f.store.DB().Exec("CREATE TRIGGER inbox_test_fail_event BEFORE INSERT ON events WHEN NEW.event_type='message.sent' BEGIN SELECT RAISE(ABORT,'synthetic event failure'); END"); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"recipient_worker_id": f.recipient, "body": "synthetic atomic body"}
	w := f.request("POST", f.path(f.a, "messages"), f.clientA, f.sender, "atomic-send", body)
	if w.Code < 500 {
		t.Fatalf("event failure accepted: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "synthetic event failure") || strings.Contains(w.Body.String(), "synthetic atomic body") {
		t.Fatal("failure leaked SQL/body")
	}
	if got := f.inbox(f.a, f.clientA, f.recipient, ""); len(got.Items) != 0 || f.eventCount(f.a) != 0 {
		t.Fatal("failed event left message/event")
	}
	var count int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM inbox_conversations WHERE project_id=?", f.a).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed event left conversation: %d %v", count, err)
	}
	if _, err := f.store.DB().Exec("DROP TRIGGER inbox_test_fail_event"); err != nil {
		t.Fatal(err)
	}
	sent := f.send(f.a, f.clientA, f.sender, f.recipient, "synthetic atomic body", "", "atomic-send")
	if got := f.inbox(f.a, f.clientA, f.recipient, ""); len(got.Items) != 1 || f.eventCount(f.a) != 1 {
		t.Fatal("failed receipt prevented same-key successful retry")
	}
	if _, err := f.store.DB().Exec("CREATE TRIGGER inbox_test_fail_ack BEFORE INSERT ON events WHEN NEW.event_type='message.acknowledged' BEGIN SELECT RAISE(ABORT,'synthetic ack event failure'); END"); err != nil {
		t.Fatal(err)
	}
	w = f.request("POST", f.path(f.a, "messages/"+sent.ID+"/ack"), f.clientA, f.recipient, "atomic-ack", struct{}{})
	if w.Code < 500 {
		t.Fatalf("ack event failure accepted: %d %s", w.Code, w.Body.String())
	}
	state := f.inbox(f.a, f.clientA, f.recipient, "")
	if state.Items[0].Seen || state.Items[0].Acknowledged || f.eventCount(f.a) != 1 {
		t.Fatal("ack event failure left seen or acknowledged state")
	}
	if _, err := f.store.DB().Exec("DROP TRIGGER inbox_test_fail_ack"); err != nil {
		t.Fatal(err)
	}
	f.transition(f.a, f.clientA, f.recipient, sent.ID, "ack", "atomic-ack")
	state = f.inbox(f.a, f.clientA, f.recipient, "")
	if !state.Items[0].Seen || !state.Items[0].Acknowledged || f.eventCount(f.a) != 2 {
		t.Fatal("atomic ack retry did not commit exactly one transition")
	}
}

func TestConcurrentSameKeySendAndSeenAckKeepOneDurableOutcome(t *testing.T) {
	f := setup(t)
	body := map[string]any{"recipient_worker_id": f.recipient, "body": "concurrent synthetic message"}
	var responses [2]*httptest.ResponseRecorder
	start := make(chan struct{})
	var workers sync.WaitGroup
	for index := range responses {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			responses[index] = f.request("POST", f.path(f.a, "messages"), f.clientA, f.sender, "simultaneous-send", body)
		}()
	}
	close(start)
	workers.Wait()
	var first, second receipt
	decode(t, responses[0], http.StatusCreated, &first)
	decode(t, responses[1], http.StatusCreated, &second)
	if first != second || f.eventCount(f.a) != 1 {
		t.Fatal("concurrent retry created distinct receipt/event")
	}
	if got := f.inbox(f.a, f.clientA, f.recipient, ""); len(got.Items) != 1 {
		t.Fatal("concurrent retry created extra message")
	}
	start = make(chan struct{})
	for index, action := range []string{"seen", "ack"} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			responses[index] = f.request("POST", f.path(f.a, "messages/"+first.ID+"/"+action), f.clientA, f.recipient, "simultaneous-"+action, struct{}{})
		}()
	}
	close(start)
	workers.Wait()
	for _, response := range responses {
		decode(t, response, http.StatusOK, nil)
	}
	state := f.inbox(f.a, f.clientA, f.recipient, "")
	if !state.Items[0].Seen || !state.Items[0].Acknowledged {
		t.Fatal("concurrent seen/ack lost acknowledgment or its implied seen state")
	}
	if count := f.eventCount(f.a); count < 2 || count > 3 {
		t.Fatalf("concurrent transitions emitted extra/missing events: %d", count)
	}
	if f.providerCalls != 2 {
		t.Fatal("concurrent coordination called provider")
	}
}

func TestInvalidKeysCannotCreateMessageReceiptsOrTransitions(t *testing.T) {
	f := setup(t)
	sent := f.send(f.a, f.clientA, f.sender, f.recipient, "synthetic key validation", "", "valid-send")
	before := f.eventCount(f.a)
	for _, action := range []string{"send", "seen", "ack"} {
		for _, key := range []string{"", " padded", "padded ", strings.Repeat("x", 129)} {
			t.Run(action+"/"+key, func(t *testing.T) {
				path, worker, body := f.path(f.a, "messages/"+sent.ID+"/"+action), f.recipient, `{}`
				if action == "send" {
					path, worker, body = f.path(f.a, "messages"), f.sender, `{"recipient_worker_id":"`+f.recipient+`","body":"must not be stored"}`
				}
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+f.clientA)
				r.Header.Set("X-FC-Worker", worker)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Idempotency-Key", key) // Keep the explicitly empty header present.
				w := httptest.NewRecorder()
				f.mux.ServeHTTP(w, r)
				errorCode(t, w, http.StatusBadRequest, "validation_failed")
			})
		}
	}
	if f.eventCount(f.a) != before {
		t.Fatal("invalid key committed message state/events")
	}
	page := f.inbox(f.a, f.clientA, f.recipient, "")
	if len(page.Items) != 1 || page.Items[0].Seen || page.Items[0].Acknowledged {
		t.Fatal("invalid key changed recipient state")
	}
	for _, route := range []string{"inbox/" + f.recipient, "conversations/" + sent.ConversationID} {
		w := f.request("GET", f.path(f.a, route), f.clientA, "", "", nil)
		var result map[string]any
		decode(t, w, http.StatusOK, &result)
		if _, present := result["cursor"]; present || result["has_more"] != false {
			t.Fatal("final page must omit the continuation cursor")
		}
	}
}
