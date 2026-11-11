package inbox_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/server"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerRetirementRealParentEmptyACKAndRetiredSendReplay(t *testing.T) {
	root := os.Getenv("FC_ADMIN_PARENT_FIXTURE")
	if root == "" {
		t.Skip("requires actual parent-produced receipts")
	}
	body, err := os.ReadFile(filepath.Join(root, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]string
	if json.Unmarshal(body, &f) != nil {
		t.Fatal("fixture metadata")
	}
	dir := filepath.Join(t.TempDir(), "copy")
	err = filepath.WalkDir(filepath.Join(root, "source"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(filepath.Join(root, "source"), path)
		if err != nil {
			return err
		}
		dest := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, b, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mux := server.New(s)
	inbox.Register(mux, s)
	call := func(path, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+f["token"])
		r.Header.Set("X-FC-Worker", f["worker"])
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	base := "/api/v1/projects/" + f["project"]
	var before int
	if err := s.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	w := call(base+"/messages/"+f["message"]+"/ack", "parent-empty-ack", "{}")
	if w.Code != 200 || w.Body.String() != f["ack_response"] {
		t.Fatal("legacy ACK changed", w.Code, w.Body.String())
	}
	explicit, _ := json.Marshal(map[string]string{"worker_id": f["worker"]})
	w = call(base+"/messages/"+f["message"]+"/ack", "parent-empty-ack", string(explicit))
	if w.Code != 409 {
		t.Fatal("different body reused parent receipt", w.Code)
	}
	var after int
	s.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&after)
	if before != after {
		t.Fatal("replay mutated receipts")
	}
	var sendKey, original string
	if err := s.DB().QueryRow(`SELECT key,response_json FROM receipts WHERE project_id=? AND operation='messages.send'`, f["project"]).Scan(&sendKey, &original); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO worker_retirements VALUES(?,?, '2026-11-01T00:00:00Z','synthetic-actor')`, f["project"], f["worker"]); err != nil {
		t.Fatal(err)
	}
	message, _ := json.Marshal(map[string]string{"recipient_worker_id": f["worker"], "body": "Retained message with literal <text>"})
	w = call(base+"/messages", sendKey, string(message))
	if w.Code != 201 || strings.TrimSpace(w.Body.String()) != original {
		t.Fatal("retired sender lost exact history", w.Code, w.Body.String(), original)
	}
	w = call(base+"/messages", "new-after-retirement", string(message))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "worker_retired") {
		t.Fatal("new retired send admitted", w.Code, w.Body.String())
	}
}

func TestWorkerRetirementACKRequiresActingWorker(t *testing.T) {
	for _, tc := range []struct {
		name              string
		onBehalf          bool
		explicitRecipient bool
	}{
		{name: "manager", onBehalf: true, explicitRecipient: true},
		{name: "administrator", onBehalf: true, explicitRecipient: true},
		{name: "writer-default"},
		{name: "writer-explicit", explicitRecipient: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			token := f.clientA
			switch tc.name {
			case "manager":
				token = "synthetic-ack-manager-token"
				f.grant(f.a, token, []string{"manage"})
			case "administrator":
				token = f.admin
			}
			actor := f.recipient
			if tc.onBehalf {
				actor = f.sender
			}
			body := map[string]string{}
			if tc.explicitRecipient {
				body["worker_id"] = f.recipient
			}
			sent := f.send(f.a, f.clientA, f.sender, f.recipient, "Synthetic pending acknowledgement", "", "send")
			path := f.path(f.a, "messages/"+sent.ID+"/ack")
			before := f.receiptStateSnapshot(f.a, sent.ID, f.recipient)
			if before.seen || before.acknowledged || before.revision != 1 {
				t.Fatalf("unexpected initial recipient state: %+v", before)
			}

			errorCode(t, f.request(http.MethodPost, path, token, "", "ack", body), http.StatusBadRequest, "validation_failed")
			if got := f.receiptStateSnapshot(f.a, sent.ID, f.recipient); got != before {
				t.Fatalf("missing acting worker changed durable state: before=%+v after=%+v", before, got)
			}
			if tc.onBehalf {
				errorCode(t, f.request(http.MethodPost, path, f.clientA, actor, "writer-ack", body), http.StatusForbidden, "forbidden")
				if got := f.receiptStateSnapshot(f.a, sent.ID, f.recipient); got != before {
					t.Fatalf("ordinary writer changed another recipient's state: %+v", got)
				}
			}

			// A rejected request must not reserve its key or poison a corrected retry.
			first := f.request(http.MethodPost, path, token, actor, "ack", body)
			var result receipt
			decode(t, first, http.StatusOK, &result)
			if result != (receipt{ID: sent.ID, ConversationID: sent.ConversationID, Outcome: "acknowledged"}) {
				t.Fatalf("unexpected acknowledgement receipt: %+v", result)
			}
			want := before
			want.receipts++
			want.events++
			want.seen, want.acknowledged = true, true
			want.revision++
			if got := f.receiptStateSnapshot(f.a, sent.ID, f.recipient); got != want {
				t.Fatalf("acknowledgement did not commit exactly one transition: want=%+v got=%+v", want, got)
			}

			var eventActor, payloadJSON, principal string
			var revision int64
			if err := f.store.DB().QueryRow(`SELECT actor_id,payload_json,revision FROM events WHERE project_id=? AND resource_id=? AND event_type='message.acknowledged'`, f.a, sent.ID).Scan(&eventActor, &payloadJSON, &revision); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(token))
			if err := f.store.DB().QueryRow(`SELECT principal_id FROM credentials WHERE token_sha256=?`, hex.EncodeToString(sum[:])).Scan(&principal); err != nil {
				t.Fatal(err)
			}
			var payload map[string]string
			if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
				t.Fatal(err)
			}
			if eventActor != principal || revision != want.revision || payload["recipient_worker_id"] != f.recipient || payload["conversation_id"] != sent.ConversationID {
				t.Fatalf("acknowledgement lost principal or recipient attribution: actor=%q revision=%d payload=%v", eventActor, revision, payload)
			}
			actingWorker, hasActingWorker := payload["actor_worker_id"]
			if tc.onBehalf && (!hasActingWorker || actingWorker != actor) || !tc.onBehalf && hasActingWorker {
				t.Fatalf("acting-worker audit attribution changed: %v", payload)
			}

			replay := f.request(http.MethodPost, path, token, actor, "ack", body)
			decode(t, replay, http.StatusOK, nil)
			if replay.Body.String() != first.Body.String() {
				t.Fatal("exact-key replay changed the original acknowledgement receipt")
			}
			if got := f.receiptStateSnapshot(f.a, sent.ID, f.recipient); got != want {
				t.Fatalf("exact-key replay changed durable state: %+v", got)
			}
			errorCode(t, f.request(http.MethodPost, path, token, "", "ack", body), http.StatusBadRequest, "validation_failed")
			if got := f.receiptStateSnapshot(f.a, sent.ID, f.recipient); got != want {
				t.Fatalf("replay without acting worker changed durable state: %+v", got)
			}
		})
	}
}

type receiptStateSnapshot struct {
	receipts, events   int
	seen, acknowledged bool
	revision           int64
}

func (f *fixture) receiptStateSnapshot(project, message, worker string) receiptStateSnapshot {
	f.t.Helper()
	var snapshot receiptStateSnapshot
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&snapshot.receipts); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&snapshot.events); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT seen,acknowledged,revision FROM inbox_recipient_state WHERE project_id=? AND message_id=? AND worker_id=?`, project, message, worker).Scan(&snapshot.seen, &snapshot.acknowledged, &snapshot.revision); err != nil {
		f.t.Fatal(err)
	}
	return snapshot
}
