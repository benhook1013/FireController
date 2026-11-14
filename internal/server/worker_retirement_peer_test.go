package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestWorkerRetirementPeerAckFeedKeepsOriginalWireAndAudit(t *testing.T) {
	f, project, actor, target := retirementFixture(t)
	base := "/api/v1/projects/" + project
	live := httptest.NewServer(f.handler)
	defer live.Close()
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte(f.admin), 0600); err != nil {
		t.Fatal(err)
	}
	recipient, err := client.New(t.Context(), profile.Binding{Context: profile.Context{Profile: "fixture", ServerURL: live.URL, InstanceID: f.store.InstanceID(), ProjectID: project, WorkerID: target}, CredentialFile: credential})
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Close()
	snapshot, err := recipient.InboxSnapshot(t.Context(), client.PageOptions{}, client.InboxFilter{})
	if err != nil {
		t.Fatal(err)
	}
	message := retirementJSON(t, f, "POST", base+"/messages", f.admin, actor, map[string]any{"recipient_worker_id": target, "body": "Synthetic coordination"}, 201)["id"].(string)
	retirementJSON(t, f, "POST", base+"/messages/"+message+"/ack", f.admin, actor, map[string]any{"worker_id": target}, 200)
	events, err := recipient.MessageEvents(t.Context(), snapshot.EventCursor, 100, 0)
	if err != nil {
		t.Fatal("actual typed feed rejected on-behalf ACK", err)
	}
	if len(events.Items) != 2 || events.Items[1].Type != "message.acknowledged" || events.Items[1].Payload.RecipientWorkerID != target || events.Items[1].Payload.ConversationID == "" {
		t.Fatal("feed facts changed", events)
	}
	raw, err := json.Marshal(events)
	if err != nil || strings.Contains(string(raw), "actor_worker_id") {
		t.Fatal("public feed changed", err)
	}
	var stored string
	if err = f.store.DB().QueryRow(`SELECT payload_json FROM events WHERE project_id=? AND event_type='message.acknowledged' AND resource_id=?`, project, message).Scan(&stored); err != nil || !strings.Contains(stored, `"actor_worker_id":"`+actor+`"`) {
		t.Fatal("durable actor attribution was lost", err)
	}
}

func TestWorkerRetirementPeerBrowserOriginalManagementCeiling(t *testing.T) {
	f, project, actor, target := retirementFixture(t)
	base := "/api/v1/projects/" + project
	token := "synthetic-promoted-writer-token-1234567"
	f.grant(t, project, token, true)
	oldID := strings.Repeat("a", 32)
	oldCookie := browserActionsLogin(t, f, project, token, oldID, false)
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_manage=1 WHERE project_id=?`, project); err != nil {
		t.Fatal(err)
	}
	managerID := strings.Repeat("b", 32)
	managerCookie := browserActionsLogin(t, f, project, token, managerID, true)
	message := retirementJSON(t, f, "POST", base+"/messages", f.admin, actor, map[string]any{"recipient_worker_id": target, "body": "Synthetic pending message"}, 201)["id"].(string)
	path := base + "/messages/" + message + "/ack"
	body := `{"worker_id":"` + target + `"}`
	before := count(t, f.store, "receipts")
	require(t, serveBrowserAction(f, browserActionRequest(f, http.MethodPost, path, oldID, oldCookie, actor, "original-key", body)), 403)
	if count(t, f.store, "receipts") != before {
		t.Fatal("old writer cookie performed management work")
	}
	first := serveBrowserAction(f, browserActionRequest(f, http.MethodPost, path, managerID, managerCookie, actor, "original-key", body))
	require(t, first, 200)
	require(t, serveBrowserAction(f, browserActionRequest(f, http.MethodPost, path, oldID, oldCookie, actor, "original-key", body)), 403)
	replay := serveBrowserAction(f, browserActionRequest(f, http.MethodPost, path, managerID, managerCookie, actor, "original-key", body))
	require(t, replay, 200)
	if replay.Body.String() != first.Body.String() {
		t.Fatal("original manager replay changed")
	}
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_manage=0 WHERE project_id=?`, project); err != nil {
		t.Fatal(err)
	}
	require(t, serveBrowserAction(f, browserActionRequest(f, http.MethodPost, path, managerID, managerCookie, actor, "original-key", body)), 403)
	require(t, request(f.handler, http.MethodPost, path, f.admin, "bearer-admin", actor, body), 200)
}
