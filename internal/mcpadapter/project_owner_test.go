package mcpadapter_test

import (
	"bytes"
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

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestProjectOwnerSDKManagementAndSelectedWorker(t *testing.T) {
	f := mutationFixture(t)
	if err := reviewexec.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	engine, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "sdk-capability", Provider: "synthetic", Account: "fixture", Channels: []string{"cli", "hosted"}}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	engine.Register(mux)
	f.handler = mux
	setup := func(method, path string, body any, selected ...string) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+f.admin)
		r.Header.Set("Idempotency-Key", core.NewID("setup"))
		actor := f.sender
		if len(selected) > 0 {
			actor = selected[0]
		}
		r.Header.Set("X-FC-Worker", actor)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code < 200 || w.Code > 299 {
			t.Fatal(w.Code, w.Body.String())
		}
		var v map[string]any
		if json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatal("setup response")
		}
		return v
	}
	const managerToken = "synthetic-private-sdk-manager"
	digest := sha256.Sum256([]byte(managerToken))
	setup("POST", "/api/v1/projects/"+f.project+"/credentials", map[string]any{"token_sha256": hex.EncodeToString(digest[:]), "permissions": []string{"manage"}})
	setup("POST", "/api/v1/projects/"+f.project+"/reviews/execution/configuration", map[string]any{"expected_revision": 0, "capability_id": "sdk-capability", "mode": "independent", "queue": []int{2}, "channels": []string{"cli", "hosted"}, "paused": false})
	tokenFile := filepath.Join(t.TempDir(), "owner.token")
	if os.WriteFile(tokenFile, []byte(managerToken), 0600) != nil {
		t.Fatal("private token fixture")
	}
	var lose atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/workers") && lose.Swap(false) {
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, r)
			if rec.Code != 200 {
				t.Error(rec.Code, rec.Body.String())
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.Code)
			w.Write([]byte("{"))
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	identity := profile.Context{Profile: "owner", ServerURL: server.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"owner": identity}, tokenFile), "owner")
	var policy client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_policy_configure", map[string]any{"expected_revision": 0, "paused_channels": []string{}, "required_channels": []string{"cli", "hosted"}, "max_review_files": 100, "round_defaults": map[string]any{"hosted": map[string]any{"minimum": 2, "maximum": 5}, "cli": map[string]any{"minimum": 0, "maximum": 8}}, "idempotency_key": "policy"}), &policy)
	var current client.ReviewPolicyRecord
	decodeResult(t, call(t, session, "review_policy_read", nil), &current)
	if current.Revision != policy.Revision || current.Policy.MaxReviewFiles != 100 || current.Policy.RoundDefaults == nil || current.Policy.RoundDefaults.Hosted.Maximum != 5 || current.Policy.RoundDefaults.CLI.Maximum != 8 {
		t.Fatal(current)
	}
	var configured client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_execution_configure", map[string]any{"expected_revision": 1, "capability_id": "sdk-capability", "mode": "independent", "queue": []int{2, 4}, "channels": []string{"cli", "hosted"}, "paused": false, "idempotency_key": "execution"}), &configured)
	if configured.Revision != 2 {
		t.Fatal(configured)
	}
	for _, pair := range []struct {
		pull             int64
		channel          string
		maximum, minimum int64
	}{{2, "hosted", 3, 2}, {2, "cli", 6, 2}, {4, "hosted", 5, 2}, {4, "cli", 8, 0}} {
		var control client.ReviewRoundControl
		decodeResult(t, call(t, session, "review_rounds_read", map[string]any{"pull_number": pair.pull, "channel": pair.channel}), &control)
		if !control.Configured || control.Minimum != pair.minimum || control.Maximum != pair.maximum || control.Used != 0 || control.Closed {
			t.Fatal("SDK enrollment changed existing controls or missed defaults", control)
		}
	}
	create := map[string]any{"name": "SDK Inspector", "idempotency_key": "create-worker"}
	lose.Store(true)
	diagnosticResult(t, call(t, session, "worker_create", create), "invalid_response", true)
	var worker client.WorkerCreateReceipt
	decodeResult(t, call(t, session, "worker_create", create), &worker)
	if worker.Outcome != "created" {
		t.Fatal(worker)
	}
	for i := 0; i < 2; i++ {
		setup("POST", "/api/v1/projects/"+f.project+"/messages", map[string]any{"recipient_worker_id": f.recipient, "body": "Synthetic selected inbox"})
	}
	var inbox client.Page[client.InboxItem]
	decodeResult(t, call(t, session, "inbox_list", map[string]any{"worker_id": f.recipient, "limit": 1}), &inbox)
	if len(inbox.Items) != 1 || !inbox.HasMore || inbox.Items[0].RecipientWorkerID != f.recipient {
		t.Fatal(inbox)
	}
	diagnosticResult(t, call(t, session, "inbox_list", map[string]any{"worker_id": f.sender, "limit": 1, "cursor": inbox.Cursor}), "validation_failed", false)
	newDigest := sha256.Sum256([]byte("new-private-sdk-credential"))
	var grant client.ProjectCredentialReceipt
	decodeResult(t, call(t, session, "project_credential_grant", map[string]any{"token_sha256": hex.EncodeToString(newDigest[:]), "permissions": []string{"read"}, "idempotency_key": "grant"}), &grant)
	var update client.ProjectCredentialUpdateReceipt
	decodeResult(t, call(t, session, "project_credential_update", map[string]any{"credential_id": grant.ID, "expected_permissions": []string{"read"}, "permissions": []string{"manage"}, "idempotency_key": "update"}), &update)
	if len(update.Permissions) != 3 {
		t.Fatal(update)
	}
	stale := diagnosticResult(t, call(t, session, "project_credential_update", map[string]any{"credential_id": grant.ID, "expected_permissions": []string{"read"}, "permissions": []string{"manage"}, "idempotency_key": "stale-update"}), "credential_permissions_changed", false)
	if !strings.Contains(stale.Action, "returned current_permissions") {
		t.Fatal(stale)
	}
	var revoke client.ProjectCredentialReceipt
	decodeResult(t, call(t, session, "project_credential_revoke", map[string]any{"credential_id": grant.ID, "idempotency_key": "revoke"}), &revoke)
	if revoke.Outcome != "revoked" {
		t.Fatal(revoke)
	}
}

func TestProjectOwnerSDKInvalidBeforeCredentials(t *testing.T) {
	identity := localContext()
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": identity}, "missing-owner-token"), "chosen")
	for _, v := range []struct{ name, raw string }{
		{"inbox_snapshot", `{"worker_id":"other"}`},
		{"inbox_ack", `{"message_id":"msg-one","worker_id":"","idempotency_key":"x"}`},
		{"review_policy_configure", `{"expected_revision":0,"paused_channels":[],"required_channels":["cli"],"idempotency_key":"x"}`},
		{"review_policy_configure", `{"expected_revision":0,"paused_channels":[],"required_channels":["cli"],"max_review_files":0,"round_defaults":null,"idempotency_key":"x"}`},
		{"review_policy_configure", `{"expected_revision":0,"paused_channels":[],"required_channels":["cli"],"max_review_files":0,"round_defaults":{"hosted":{"minimum":2,"maximum":1},"cli":{"minimum":0,"maximum":8}},"idempotency_key":"x"}`},
		{"review_policy_configure", `{"expected_revision":0,"paused_channels":[],"required_channels":["cli"],"max_review_files":0,"round_defaults":{"hosted":{"minimum":2,"maximum":5},"cli":{"maximum":8}},"idempotency_key":"x"}`},
		{"review_execution_configure", `{"expected_revision":0,"capability_id":"x","mode":"independent","queue":[],"channels":["cli"],"idempotency_key":"x"}`},
		{"worker_create", `{"name":" padded ","idempotency_key":"x"}`},
		{"project_credential_grant", `{"token":"raw secret","permissions":["manage"],"idempotency_key":"x"}`},
		{"project_credential_update", `{"credential_id":"credential-one","expected_permissions":["read"],"permissions":null,"idempotency_key":"x"}`},
		{"project_credential_revoke", `{"credential_id":"bad/id","idempotency_key":"x"}`},
	} {
		diagnosticResult(t, call(t, session, v.name, json.RawMessage(v.raw)), "invalid_arguments", false)
	}
}
