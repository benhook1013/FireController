package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/coordimport"
	"github.com/benhook1013/FireController/internal/coordimportmodel"
)

func TestMessageImportCLIHistoricalPendingStateAndReplay(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "historical-messages")
	sender := f.worker(t, project, "Sender")
	recipient := f.worker(t, project, "Recipient")
	if e := coordimport.EnsureSchema(t.Context(), f.store); e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	coordimport.Register(mux, f.store)
	f.handler = mux
	srv := httptest.NewServer(f.handler)
	defer srv.Close()
	profiles := mutationProfiles(t, f, srv.URL, map[string][2]string{"sender": {project, sender}, "recipient": {project, recipient}})
	zero := int64(0)
	batch := coordimportmodel.Batch{Version: 1, RepositoryID: 1, ExpectedRevision: &zero, Source: "selected message archive", SourceSHA256: strings.Repeat("a", 64), Jobs: []coordimportmodel.PreparedJob{}, Messages: []coordimportmodel.PreparedMessage{
		{SourceKey: "reply", ReplyToSourceKey: "root", SenderWorkerID: recipient, RecipientWorkerID: sender, Body: "  preserved historical reply 雪  ", CreatedAt: "2024-01-02T03:05:00+13:00", SeenAt: "2024-01-02T03:06:00+13:00", AcknowledgedAt: "2024-01-02T03:07:00+13:00"},
		{SourceKey: "root", SenderWorkerID: sender, RecipientWorkerID: recipient, Body: "  original pending message  ", CreatedAt: "2024-01-02T03:04:00+13:00"},
	}}
	raw, _ := json.Marshal(batch)
	file := importCLIFile(t, raw)
	provider := f.resolverHit.Load()
	var eventsBefore int
	if e := f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&eventsBefore); e != nil {
		t.Fatal(e)
	}
	code, out, stderr := mutationInvoke(t, profiles, "sender", "import", "preview", "--file", file)
	var preview client.CoordinationImportPreview
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &preview) != nil || preview.Counts.Messages != 2 || preview.Counts.NewMessages != 2 || strings.Contains(out, batch.Messages[0].Body) {
		t.Fatal("message-only preview failed", code, out, stderr)
	}
	args := []string{"import", "apply", "--file", file, "--preview", preview.PreviewSHA256, "--key", "historical-messages"}
	code, out, stderr = mutationInvoke(t, profiles, "sender", args...)
	var receipt client.CoordinationImportReceipt
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &receipt) != nil || receipt.Counts.NewMessages != 2 || len(receipt.MessageMappings) != 2 || receipt.MessageMappings[0].SourceKey != "reply" || receipt.MessageMappings[0].ConversationID != receipt.MessageMappings[1].ConversationID {
		t.Fatal("message import receipt lost graph", code, out, stderr)
	}
	mapping := receipt.MessageMappings[1]
	var seen, ack bool
	if e := f.store.DB().QueryRow(`SELECT seen,acknowledged FROM inbox_recipient_state WHERE project_id=? AND message_id=?`, project, mapping.MessageID).Scan(&seen, &ack); e != nil || seen || ack {
		t.Fatal("import silently handled pending message", e)
	}
	code, out, stderr = mutationInvoke(t, profiles, "recipient", "inbox", "list", "--unread", "--unacknowledged")
	if code != 0 || stderr != "" || !strings.Contains(out, mapping.MessageID) || !strings.Contains(out, "original pending message") {
		t.Fatal("imported pending message not discoverable", code, out, stderr)
	}
	// Only import metadata may be emitted, rather than replaying either historical send.
	var newEvents, sends int
	if e := f.store.DB().QueryRow(`SELECT count(*),coalesce(sum(CASE WHEN event_type='message.sent' THEN 1 ELSE 0 END),0) FROM events`).Scan(&newEvents, &sends); e != nil {
		t.Fatal(e)
	}
	if newEvents != eventsBefore+1 || sends != 0 {
		t.Fatal("historical delivery was replayed", newEvents, sends)
	}
	mutationSuccess(t, profiles, "recipient", "inbox", "ack", mapping.MessageID, "--key", "handle-imported-pending")
	code, out, stderr = mutationInvoke(t, profiles, "sender", args...)
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &receipt) != nil || receipt.Revision != 1 {
		t.Fatal("exact replay failed", code, stderr)
	}
	if e := f.store.DB().QueryRow(`SELECT seen,acknowledged FROM inbox_recipient_state WHERE project_id=? AND message_id=?`, project, mapping.MessageID).Scan(&seen, &ack); e != nil || !seen || !ack {
		t.Fatal("replay erased later handling", e)
	}
	if f.resolverHit.Load() != provider {
		t.Fatal("historical message import touched provider")
	}
}
