package inbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
)

func controllerNotice(t *testing.T, f *fixture, key, recipient, body string) string {
	t.Helper()
	var id string
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		id, err = inbox.PublishControllerMessage(ctx, tx, core.Scope{ProjectID: f.a, PrincipalID: "service-lifecycle-sync"}, key, recipient, body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func controllerRead(t *testing.T, f *fixture, path string, optin bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer "+f.clientA)
	r.Header.Set("X-FC-Worker", f.recipient)
	if optin {
		r.Header.Set(core.InboxOriginHeader, "controller-v1")
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func TestControllerInboxDurableDedupeOriginProjectionAndOneWay(t *testing.T) {
	f := setup(t)
	id := controllerNotice(t, f, "terminal-pr-2", f.recipient, "PR #2 merged")
	if got := controllerNotice(t, f, "terminal-pr-2", f.recipient, "PR #2 merged"); got != id {
		t.Fatal("replay changed identity", got, id)
	}
	var messages, events int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM inbox_messages WHERE id=?`, id).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='message.sent' AND resource_id=? AND actor_id='service-lifecycle-sync'`, id).Scan(&events); err != nil || messages != 1 || events != 1 {
		t.Fatal("dedupe/provenance lost", messages, events, err)
	}
	path := f.path(f.a, "inbox/"+f.recipient) + "?limit=1"
	old := controllerRead(t, f, path, false)
	current := controllerRead(t, f, path, true)
	var oldPage, currentPage struct {
		Items []map[string]any `json:"items"`
	}
	if old.Code != 200 || json.Unmarshal(old.Body.Bytes(), &oldPage) != nil || len(oldPage.Items) != 0 {
		t.Fatal("old projection included Controller", old.Code, old.Body.String())
	}
	if current.Code != 200 || json.Unmarshal(current.Body.Bytes(), &currentPage) != nil || len(currentPage.Items) != 1 || currentPage.Items[0]["origin_kind"] != "controller" || currentPage.Items[0]["sender_worker_id"] != "" {
		t.Fatal(current.Code, current.Body.String())
	}
	messagePath := f.path(f.a, "messages/"+id)
	if r := controllerRead(t, f, messagePath, false); r.Code != 404 {
		t.Fatal("old selected read exposed Controller", r.Code)
	}
	full := controllerRead(t, f, messagePath, true)
	var message map[string]any
	if full.Code != 200 || json.Unmarshal(full.Body.Bytes(), &message) != nil || message["body"] != "PR #2 merged" {
		t.Fatal(full.Code, full.Body.String())
	}
	conversation := message["conversation_id"].(string)
	if r := controllerRead(t, f, f.path(f.a, "conversations/"+conversation), false); r.Code != 404 {
		t.Fatal("old conversation read exposed Controller", r.Code)
	}
	if r := controllerRead(t, f, f.path(f.a, "conversations/"+conversation), true); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	for _, body := range []map[string]any{{"recipient_worker_id": f.sender, "body": "reply", "reply_to_message_id": id}, {"recipient_worker_id": f.recipient, "body": "forged", "origin_kind": "controller"}} {
		w := f.request(http.MethodPost, f.path(f.a, "messages"), f.clientA, f.recipient, "no-controller-forgery", body)
		if w.Code != 409 && w.Code != 400 {
			t.Fatal("reply/origin forgery accepted", w.Code, w.Body.String())
		}
	}
	ack := f.request(http.MethodPost, messagePath+"/ack", f.clientA, f.recipient, "ack-controller", map[string]any{})
	if ack.Code != 200 {
		t.Fatal(ack.Code, ack.Body.String())
	}
	delivery := controllerRead(t, f, messagePath+"/delivery", true)
	if delivery.Code != 200 || !strings.Contains(delivery.Body.String(), `"acknowledged":true`) {
		t.Fatal(delivery.Code, delivery.Body.String())
	}
	if err := inbox.ValidateSnapshot(t.Context(), f.store.DB(), 2); err != nil {
		t.Fatal(err)
	}
}

func TestControllerInboxMigrationPreservesRowsReceiptsAndSequence(t *testing.T) {
	for _, mode := range []string{"none", "empty_high", "sparse", "bad_reference", "bad_root_reference", "bad_reply_reference"} {
		t.Run(mode, func(t *testing.T) {
			store, err := core.Open(t.Context(), filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err = store.EnsureSchema(t.Context(), "inbox", inbox.SchemaSQL(1)); err != nil {
				t.Fatal(err)
			}
			_, err = store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES('p','Project',1,'example/project');INSERT INTO workers(project_id,id,name) VALUES('p','s','Sender'),('p','r','Recipient')`)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "none" {
				err = store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.Exec(`INSERT INTO inbox_conversations VALUES('p','c','m');INSERT INTO inbox_messages(sequence,project_id,id,conversation_id,sender_worker_id,recipient_worker_id,body,created_at) VALUES(41,'p','m','c','s','r','retained','2026-11-02T00:00:00Z');INSERT INTO inbox_recipient_state VALUES('p','m','r',1,1,3);UPDATE sqlite_sequence SET seq=99 WHERE name='inbox_messages'`)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				if mode == "empty_high" {
					err = store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
						_, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON;DELETE FROM inbox_recipient_state;DELETE FROM inbox_conversations;DELETE FROM inbox_messages`)
						return err
					})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if strings.HasPrefix(mode, "bad_") {
				conn, err := store.DB().Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				corrupt := `UPDATE inbox_messages SET recipient_worker_id='missing'`
				if mode == "bad_root_reference" {
					corrupt = `UPDATE inbox_conversations SET root_message_id='missing'`
				} else if mode == "bad_reply_reference" {
					corrupt = `UPDATE inbox_messages SET reply_to_message_id='missing'`
				}
				_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF;`+corrupt+`;PRAGMA foreign_keys=ON`)
				_ = conn.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			before := controllerRows(t, store.DB(), `SELECT sequence,project_id,id,conversation_id,sender_worker_id,recipient_worker_id,reply_to_message_id,body,created_at FROM inbox_messages`)
			conversations := controllerRows(t, store.DB(), `SELECT * FROM inbox_conversations`)
			states := controllerRows(t, store.DB(), `SELECT * FROM inbox_recipient_state`)
			seq := controllerRows(t, store.DB(), `SELECT seq FROM sqlite_sequence WHERE name='inbox_messages'`)
			err = inbox.EnsureSchema(t.Context(), store)
			if strings.HasPrefix(mode, "bad_") {
				if err == nil {
					t.Fatal("inconsistent old references migrated")
				}
				if got := controllerRows(t, store.DB(), `SELECT value FROM metadata WHERE key='schema:inbox'`); len(got) != 1 || got[0][0] != "1" {
					t.Fatal("failed conversion advanced metadata", got)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, controllerRows(t, store.DB(), `SELECT sequence,project_id,id,conversation_id,sender_worker_id,recipient_worker_id,reply_to_message_id,body,created_at FROM inbox_messages`)) || !reflect.DeepEqual(conversations, controllerRows(t, store.DB(), `SELECT * FROM inbox_conversations`)) || !reflect.DeepEqual(states, controllerRows(t, store.DB(), `SELECT * FROM inbox_recipient_state`)) || !reflect.DeepEqual(seq, controllerRows(t, store.DB(), `SELECT seq FROM sqlite_sequence WHERE name='inbox_messages'`)) {
				t.Fatal("migration changed retained data/receipts/sequence")
			}
			if got := controllerRows(t, store.DB(), `SELECT name FROM sqlite_schema WHERE name LIKE 'inbox_migration_%'`); len(got) != 0 {
				t.Fatal("migration leaked intermediate schema objects", got)
			}
			if !strings.HasPrefix(mode, "bad_") {
				if err = inbox.EnsureSchema(t.Context(), store); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestControllerInboxMigrationRetainsPopulatedReplyGraph(t *testing.T) {
	store, err := core.Open(t.Context(), filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.EnsureSchema(t.Context(), "inbox", inbox.SchemaSQL(1)); err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES('p','Project',1,'example/project');
INSERT INTO workers(project_id,id,name) VALUES('p','s','Sender'),('p','r','Recipient');
CREATE INDEX fixture_root_fk ON inbox_conversations(project_id,root_message_id);
CREATE INDEX fixture_reply_fk ON inbox_messages(project_id,reply_to_message_id)`)
	if err != nil {
		t.Fatal(err)
	}
	// Keep several thousand conversations and replies in one project. Without
	// bounded child-key lookups, the DROP/rebuild scans that entire project for
	// each message and can exhaust the ordinary startup migration budget.
	// Synthetic population is not an ordinary background mutation: its 6000
	// inserts can exceed that API's two-second budget under the race detector.
	// Keep fixture construction separate from the unchanged migration deadline.
	tx, err := store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(t.Context(), `
WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<3000)
INSERT INTO inbox_conversations SELECT 'p',printf('c%05d',x),printf('m%05d',2*x-1) FROM n;
WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<3000)
INSERT INTO inbox_messages(sequence,project_id,id,conversation_id,sender_worker_id,recipient_worker_id,body,created_at)
SELECT 2*x-1,'p',printf('m%05d',2*x-1),printf('c%05d',x),'s','r','retained root','2026-11-02T00:00:00Z' FROM n;
WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<3000)
INSERT INTO inbox_messages(sequence,project_id,id,conversation_id,sender_worker_id,recipient_worker_id,reply_to_message_id,body,created_at)
SELECT 2*x,'p',printf('m%05d',2*x),printf('c%05d',x),'r','s',printf('m%05d',2*x-1),'retained reply','2026-11-02T00:00:01Z' FROM n;
INSERT INTO inbox_recipient_state SELECT project_id,id,recipient_worker_id,1,1,3 FROM inbox_messages;
UPDATE sqlite_sequence SET seq=10000 WHERE name='inbox_messages';
INSERT INTO receipts(instance_id,principal_id,project_id,operation,target,key,fingerprint,response_json)
SELECT value,'principal','p','message.send','r','retained-key','retained-fingerprint','{"id":"m00001"}' FROM metadata WHERE key='instance_id';
INSERT INTO events(project_id,event_type,resource_id,revision,actor_id,payload_json,created_at)
VALUES('p','message.sent','m00001',1,'principal','{"id":"m00001"}','2026-11-02T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().Exec(`DROP INDEX fixture_root_fk;DROP INDEX fixture_reply_fk`); err != nil {
		t.Fatal(err)
	}
	queries := []string{
		`SELECT sequence,project_id,id,conversation_id,sender_worker_id,recipient_worker_id,reply_to_message_id,body,created_at FROM inbox_messages ORDER BY sequence`,
		`SELECT * FROM inbox_conversations ORDER BY project_id,id`,
		`SELECT * FROM inbox_recipient_state ORDER BY project_id,message_id,worker_id`,
		`SELECT * FROM receipts`, `SELECT * FROM events`,
		`SELECT seq FROM sqlite_sequence WHERE name='inbox_messages'`,
	}
	before := make([][][]any, len(queries))
	for i, query := range queries {
		before[i] = controllerRows(t, store.DB(), query)
	}
	if len(before[0]) != 6000 {
		t.Fatal("populated fixture incomplete", len(before[0]))
	}
	if err = inbox.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	for i, query := range queries {
		if !reflect.DeepEqual(before[i], controllerRows(t, store.DB(), query)) {
			t.Fatal("migration changed retained graph, receipts, events or sequence", query)
		}
	}
	if got := controllerRows(t, store.DB(), `PRAGMA foreign_key_check`); len(got) != 0 {
		t.Fatal("migrated graph has invalid references", got)
	}
	if got := controllerRows(t, store.DB(), `SELECT name FROM sqlite_schema WHERE name LIKE 'inbox_migration_%'`); len(got) != 0 {
		t.Fatal("migration leaked intermediate schema objects", got)
	}
	if err = inbox.ValidateSnapshot(t.Context(), store.DB(), 2); err != nil {
		t.Fatal(err)
	}
	if err = inbox.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
}
func controllerRows(t *testing.T, db *sql.DB, q string) [][]any {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]any
	for rows.Next() {
		v := make([]any, len(cols))
		ptr := make([]any, len(cols))
		for i := range v {
			ptr[i] = &v[i]
		}
		if err = rows.Scan(ptr...); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestControllerInboxProjectionPagingEventsAndOldCursorRecovery(t *testing.T) {
	f := setup(t)
	initial := controllerRead(t, f, f.path(f.a, "inbox/"+f.recipient+"/snapshot"), false)
	var snapshot struct {
		EventCursor string `json:"event_cursor"`
	}
	if initial.Code != 200 || json.Unmarshal(initial.Body.Bytes(), &snapshot) != nil {
		t.Fatal(initial.Code, initial.Body.String())
	}
	id := controllerNotice(t, f, "projection-notice", f.recipient, "PR #3 closed")
	oldEvents := controllerRead(t, f, f.path(f.a, "message-events")+"?cursor="+url.QueryEscape(snapshot.EventCursor)+"&limit=1", false)
	var oldFeed struct {
		Items  []map[string]any `json:"items"`
		Cursor string           `json:"cursor"`
	}
	if oldEvents.Code != 200 || json.Unmarshal(oldEvents.Body.Bytes(), &oldFeed) != nil || len(oldFeed.Items) != 0 || oldFeed.Cursor == snapshot.EventCursor {
		t.Fatal("old feed did not safely advance", oldEvents.Code, oldEvents.Body.String())
	}
	fresh := controllerRead(t, f, f.path(f.a, "inbox/"+f.recipient+"/snapshot"), true)
	if fresh.Code != 200 || !strings.Contains(fresh.Body.String(), id) {
		t.Fatal("new snapshot lost unacknowledged old-feed notice", fresh.Code, fresh.Body.String())
	}
	first := f.send(f.a, f.clientA, f.sender, f.recipient, "worker-first", "", "")
	f.send(f.a, f.clientA, f.sender, f.recipient, "worker-second", "", "")
	path := f.path(f.a, "inbox/"+f.recipient) + "?limit=1"
	var oldPage, newPage struct {
		Items   []map[string]any `json:"items"`
		Cursor  string           `json:"cursor"`
		HasMore bool             `json:"has_more"`
	}
	old := controllerRead(t, f, path, false)
	new := controllerRead(t, f, path, true)
	if json.Unmarshal(old.Body.Bytes(), &oldPage) != nil || len(oldPage.Items) != 1 || oldPage.Items[0]["id"] != first.ID || !oldPage.HasMore {
		t.Fatal("old list filtered after LIMIT", old.Code, old.Body.String())
	}
	if json.Unmarshal(new.Body.Bytes(), &newPage) != nil || len(newPage.Items) != 1 || newPage.Items[0]["id"] != id || !newPage.HasMore {
		t.Fatal(new.Code, new.Body.String())
	}
	if r := controllerRead(t, f, path+"&cursor="+url.QueryEscape(oldPage.Cursor), true); r.Code != 400 {
		t.Fatal("old cursor crossed projection", r.Code)
	}
	if r := controllerRead(t, f, path+"&cursor="+url.QueryEscape(newPage.Cursor), false); r.Code != 400 {
		t.Fatal("new cursor crossed projection", r.Code)
	}
	oldEvents = controllerRead(t, f, f.path(f.a, "message-events")+"?cursor="+url.QueryEscape(snapshot.EventCursor)+"&limit=1", false)
	if json.Unmarshal(oldEvents.Body.Bytes(), &oldFeed) != nil || len(oldFeed.Items) != 1 || oldFeed.Items[0]["resource_id"] != first.ID {
		t.Fatal("old event feed filtered after LIMIT", oldEvents.Code, oldEvents.Body.String())
	}
}

func TestControllerInboxRollbackChangedKeyAndRetiredRecipient(t *testing.T) {
	f := setup(t)
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := inbox.PublishControllerMessage(ctx, tx, core.Scope{ProjectID: f.a, PrincipalID: "service-lifecycle-sync"}, "rolled-back", f.recipient, "notice"); err != nil {
			return err
		}
		return errors.New("controlled rollback")
	})
	if err == nil {
		t.Fatal("fixture rollback committed")
	}
	if got := controllerRows(t, f.store.DB(), `SELECT id FROM inbox_messages WHERE origin_kind='controller'`); len(got) != 0 {
		t.Fatal("rolled-back message survived", got)
	}
	id := controllerNotice(t, f, "rolled-back", f.recipient, "notice")
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := inbox.PublishControllerMessage(ctx, tx, core.Scope{ProjectID: f.a, PrincipalID: "service-lifecycle-sync"}, "rolled-back", f.recipient, "changed")
		return err
	})
	var conflict *core.Error
	if !errors.As(err, &conflict) || conflict.Code != "idempotency_conflict" {
		t.Fatal("changed key silently rewrote notice", err)
	}
	retired := f.create(f.path(f.a, "workers"), map[string]any{"name": "Retired recipient"}, "")
	if _, err = f.store.DB().Exec(`INSERT INTO worker_retirements VALUES(?,?,'2026-11-02T00:00:00Z','service-lifecycle-sync')`, f.a, retired); err != nil {
		t.Fatal(err)
	}
	if got := controllerNotice(t, f, "skip-retired", retired, "not delivered"); got != "" {
		t.Fatal("retired recipient received a notice", got)
	}
	if got := controllerRows(t, f.store.DB(), `SELECT id FROM inbox_messages WHERE origin_kind='controller'`); len(got) != 1 || got[0][0] != id {
		t.Fatal("replay/skip changed originals", got)
	}
}
