package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestSelectedMessageCLIAndSDKReadOneBodyWithoutAcknowledgment(t *testing.T) {
	f := mutationFixture(t)
	var selected, conversations atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/messages/") {
			selected.Add(1)
		}
		if strings.Contains(r.URL.Path, "/conversations/") {
			conversations.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	binding := localContext()
	binding.ServerURL = service.URL
	binding.InstanceID = f.store.InstanceID()
	binding.ProjectID = f.project
	binding.WorkerID = f.sender
	path := profiles(t, map[string]profile.Context{"chosen": binding}, f.credential)
	session := connect(t, adapter(t, path, "chosen"), "")
	var sent client.MessageReceipt
	body := "  Full selected text\n" + strings.Repeat("untruncated context ", 600)
	decodeResult(t, call(t, session, "inbox_send", map[string]any{"idempotency_key": "selected-message", "recipient_worker_id": f.recipient, "body": body}), &sent)
	var got client.Message
	decodeResult(t, call(t, session, "inbox_message_read", map[string]any{"message_id": sent.ID}), &got)
	if got.ID != sent.ID || got.ConversationID != sent.ConversationID || got.Body != body || selected.Load() != 1 || conversations.Load() != 0 {
		t.Fatal("SDK read expanded or changed selected message")
	}
	var out, stderr bytes.Buffer
	code := cli.Run([]string{"--profiles-file", path, "--profile", "chosen", "inbox", "read", sent.ID}, nil, &out, &stderr)
	if code != cli.ExitSuccess || stderr.Len() != 0 || json.Unmarshal(out.Bytes(), &got) != nil || got.Body != body || selected.Load() != 2 || conversations.Load() != 0 {
		t.Fatalf("CLI selected read failed: code=%d error=%s", code, &stderr)
	}
	var seen, ack int
	if err := f.store.DB().QueryRow("SELECT seen,acknowledged FROM inbox_recipient_state WHERE message_id=?", sent.ID).Scan(&seen, &ack); err != nil || seen != 0 || ack != 0 {
		t.Fatal("selected reads changed receipt state", err)
	}
	before := selected.Load()
	for _, args := range []map[string]any{{}, {"message_id": "bad/id"}, {"message_id": sent.ID, "cursor": "unused"}, {"message_id": sent.ID, "project_id": f.project}} {
		result := call(t, session, "inbox_message_read", args)
		if !result.IsError {
			t.Fatal("malformed selected read accepted")
		}
	}
	if selected.Load() != before || f.providers.Load() != 1 {
		t.Fatal("invalid read contacted service or provider")
	}
}
