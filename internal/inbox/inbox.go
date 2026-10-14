// Package inbox implements project-scoped durable messages and explicit receipt state.
package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

const (
	MaxBodyBytes = 16 << 10
	PreviewRunes = 160
)

// EnsureSchema is called during exclusive startup, before accepting requests.
func EnsureSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, `
CREATE TABLE IF NOT EXISTS inbox_conversations (
 project_id TEXT NOT NULL REFERENCES projects(id),
 id TEXT NOT NULL,
 root_message_id TEXT NOT NULL,
 PRIMARY KEY(project_id,id),
 FOREIGN KEY(project_id,root_message_id) REFERENCES inbox_messages(project_id,id) DEFERRABLE INITIALLY DEFERRED
);`, `
CREATE TABLE IF NOT EXISTS inbox_messages (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 project_id TEXT NOT NULL REFERENCES projects(id),
 id TEXT NOT NULL,
 conversation_id TEXT NOT NULL,
 sender_worker_id TEXT NOT NULL,
 recipient_worker_id TEXT NOT NULL,
 reply_to_message_id TEXT,
 body TEXT NOT NULL CHECK(length(CAST(body AS BLOB)) BETWEEN 1 AND 16384),
 created_at TEXT NOT NULL,
 UNIQUE(project_id,id),
 FOREIGN KEY(project_id,conversation_id) REFERENCES inbox_conversations(project_id,id),
 FOREIGN KEY(project_id,sender_worker_id) REFERENCES workers(project_id,id),
 FOREIGN KEY(project_id,recipient_worker_id) REFERENCES workers(project_id,id),
 FOREIGN KEY(project_id,reply_to_message_id) REFERENCES inbox_messages(project_id,id)
);`, `
CREATE TABLE IF NOT EXISTS inbox_recipient_state (
 project_id TEXT NOT NULL,
 message_id TEXT NOT NULL,
 worker_id TEXT NOT NULL,
 seen INTEGER NOT NULL DEFAULT 0 CHECK(seen IN (0,1)),
 acknowledged INTEGER NOT NULL DEFAULT 0 CHECK(acknowledged IN (0,1) AND acknowledged <= seen),
 revision INTEGER NOT NULL DEFAULT 1 CHECK(revision >= 1),
 PRIMARY KEY(project_id,message_id,worker_id),
 FOREIGN KEY(project_id,message_id) REFERENCES inbox_messages(project_id,id),
 FOREIGN KEY(project_id,worker_id) REFERENCES workers(project_id,id)
);`,
		`CREATE INDEX IF NOT EXISTS inbox_recipient_page ON inbox_messages(project_id,recipient_worker_id,sequence)`,
		`CREATE INDEX IF NOT EXISTS inbox_conversation_page ON inbox_messages(project_id,conversation_id,sequence)`)
}

// Register adds the messages API to the shared authenticated application mux.
func Register(mux *http.ServeMux, store *core.Store) {
	h := &handler{store: store}
	mux.HandleFunc("POST /api/v1/projects/{project_id}/messages", h.send)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/inbox/{worker_id}", h.list)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/conversations/{conversation_id}", h.conversation)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/messages/{message_id}/seen", h.seen)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/messages/{message_id}/ack", h.ack)
}

type handler struct{ store *core.Store }

type sendRequest struct {
	RecipientWorkerID string `json:"recipient_worker_id"`
	Body              string `json:"body"`
	ReplyToMessageID  string `json:"reply_to_message_id,omitempty"`
}

type receipt struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	Outcome        string `json:"outcome"`
}

func (h *handler) send(w http.ResponseWriter, r *http.Request) {
	scope, err := h.store.Scope(r, "write")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if scope.WorkerID == "" {
		core.WriteError(w, core.Fail("validation_failed", "Select a sender with X-FC-Worker.", http.StatusBadRequest))
		return
	}
	if err := queryKeys(r); err != nil {
		core.WriteError(w, err)
		return
	}
	var request sendRequest
	if err := core.ReadJSON(w, r, &request); err != nil {
		core.WriteError(w, err)
		return
	}
	if !cleanID(request.RecipientWorkerID) || (request.ReplyToMessageID != "" && !cleanID(request.ReplyToMessageID)) || strings.TrimSpace(request.Body) == "" || len(request.Body) > MaxBodyBytes || !utf8.ValidString(request.Body) {
		core.WriteError(w, core.Fail("validation_failed", "Provide a recipient, a nonempty body of at most 16 KiB, and an optional message ID to reply to.", http.StatusBadRequest))
		return
	}
	key, err := mutationKey(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	result, err := h.store.Mutate(r.Context(), scope, core.Mutation{Operation: "messages.send", Target: "collection", Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		if err := core.CheckWorker(ctx, tx, scope.ProjectID, scope.WorkerID); err != nil {
			return nil, err
		}
		if err := core.CheckWorker(ctx, tx, scope.ProjectID, request.RecipientWorkerID); err != nil {
			return nil, err
		}
		id := core.NewID("msg")
		conversationID := core.NewID("conv")
		if request.ReplyToMessageID != "" {
			if err := tx.QueryRowContext(ctx, "SELECT conversation_id FROM inbox_messages WHERE project_id=? AND id=?", scope.ProjectID, request.ReplyToMessageID).Scan(&conversationID); err != nil {
				return nil, missing(err)
			}
		} else {
			if _, err := tx.ExecContext(ctx, "INSERT INTO inbox_conversations(project_id,id,root_message_id) VALUES(?,?,?)", scope.ProjectID, conversationID, id); err != nil {
				return nil, err
			}
		}
		var reply any
		if request.ReplyToMessageID != "" {
			reply = request.ReplyToMessageID
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO inbox_messages(project_id,id,conversation_id,sender_worker_id,recipient_worker_id,reply_to_message_id,body,created_at) VALUES(?,?,?,?,?,?,?,?)", scope.ProjectID, id, conversationID, scope.WorkerID, request.RecipientWorkerID, reply, request.Body, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO inbox_recipient_state(project_id,message_id,worker_id) VALUES(?,?,?)", scope.ProjectID, id, request.RecipientWorkerID); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "message.sent", id, 1, map[string]string{"conversation_id": conversationID, "sender_worker_id": scope.WorkerID, "recipient_worker_id": request.RecipientWorkerID}); err != nil {
			return nil, err
		}
		return receipt{ID: id, ConversationID: conversationID, Outcome: "sent"}, nil
	})
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusCreated, json.RawMessage(result))
}

type inboxItem struct {
	ID                string `json:"id"`
	ConversationID    string `json:"conversation_id"`
	SenderWorkerID    string `json:"sender_worker_id"`
	RecipientWorkerID string `json:"recipient_worker_id"`
	Preview           string `json:"preview"`
	PreviewTruncated  bool   `json:"preview_truncated"`
	Seen              bool   `json:"seen"`
	Acknowledged      bool   `json:"acknowledged"`
	CreatedAt         string `json:"created_at"`
	sequence          int64
}

type inboxPage struct {
	Items   []inboxItem `json:"items"`
	Cursor  string      `json:"cursor,omitempty"`
	HasMore bool        `json:"has_more"`
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	scope, err := h.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if err := queryKeys(r, "limit", "cursor", "unread", "unacknowledged"); err != nil {
		core.WriteError(w, err)
		return
	}
	unread, err := boolFilter(r, "unread")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	unacknowledged, err := boolFilter(r, "unacknowledged")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	workerID := r.PathValue("worker_id")
	var exists int
	if err := h.store.DB().QueryRowContext(r.Context(), "SELECT 1 FROM workers WHERE project_id=? AND id=?", scope.ProjectID, workerID).Scan(&exists); err != nil {
		core.WriteError(w, missing(err))
		return
	}
	filter := "inbox:" + workerID + ":unread=" + strconv.FormatBool(unread) + ":unacknowledged=" + strconv.FormatBool(unacknowledged)
	limit, after, err := h.page(r, scope, filter)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	query := `SELECT m.sequence,m.id,m.conversation_id,m.sender_worker_id,m.recipient_worker_id,m.body,m.created_at,s.seen,s.acknowledged FROM inbox_messages m JOIN inbox_recipient_state s ON s.project_id=m.project_id AND s.message_id=m.id AND s.worker_id=m.recipient_worker_id WHERE m.project_id=? AND m.recipient_worker_id=? AND m.sequence>?`
	if unread {
		query += " AND s.seen=0"
	}
	if unacknowledged {
		query += " AND s.acknowledged=0"
	}
	query += " ORDER BY m.sequence ASC LIMIT ?"
	rows, err := h.store.DB().QueryContext(r.Context(), query, scope.ProjectID, workerID, after, limit+1)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	defer rows.Close()
	page := inboxPage{Items: make([]inboxItem, 0, limit)}
	for rows.Next() {
		var item inboxItem
		var body string
		if err := rows.Scan(&item.sequence, &item.ID, &item.ConversationID, &item.SenderWorkerID, &item.RecipientWorkerID, &body, &item.CreatedAt, &item.Seen, &item.Acknowledged); err != nil {
			core.WriteError(w, err)
			return
		}
		item.Preview, item.PreviewTruncated = preview(body)
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		core.WriteError(w, err)
		return
	}
	if len(page.Items) > limit {
		page.HasMore = true
		page.Items = page.Items[:limit]
		page.Cursor = h.store.Cursor(scope, filter, strconv.FormatInt(page.Items[len(page.Items)-1].sequence, 10))
	}
	core.WriteJSON(w, http.StatusOK, page)
}

type message struct {
	ID                string `json:"id"`
	ConversationID    string `json:"conversation_id"`
	SenderWorkerID    string `json:"sender_worker_id"`
	RecipientWorkerID string `json:"recipient_worker_id"`
	ReplyToMessageID  string `json:"reply_to_message_id,omitempty"`
	Body              string `json:"body"`
	CreatedAt         string `json:"created_at"`
	sequence          int64
}

type conversationPage struct {
	ConversationID string    `json:"conversation_id"`
	RootMessageID  string    `json:"root_message_id"`
	Items          []message `json:"items"`
	Cursor         string    `json:"cursor,omitempty"`
	HasMore        bool      `json:"has_more"`
}

func (h *handler) conversation(w http.ResponseWriter, r *http.Request) {
	scope, err := h.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if err := queryKeys(r, "limit", "cursor"); err != nil {
		core.WriteError(w, err)
		return
	}
	id := r.PathValue("conversation_id")
	filter := "conversation:" + id
	limit, after, err := h.page(r, scope, filter)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	page := conversationPage{ConversationID: id, Items: make([]message, 0, limit)}
	if err := h.store.DB().QueryRowContext(r.Context(), "SELECT root_message_id FROM inbox_conversations WHERE project_id=? AND id=?", scope.ProjectID, id).Scan(&page.RootMessageID); err != nil {
		core.WriteError(w, missing(err))
		return
	}
	rows, err := h.store.DB().QueryContext(r.Context(), "SELECT sequence,id,conversation_id,sender_worker_id,recipient_worker_id,COALESCE(reply_to_message_id,''),body,created_at FROM inbox_messages WHERE project_id=? AND conversation_id=? AND sequence>? ORDER BY sequence ASC LIMIT ?", scope.ProjectID, id, after, limit+1)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var item message
		if err := rows.Scan(&item.sequence, &item.ID, &item.ConversationID, &item.SenderWorkerID, &item.RecipientWorkerID, &item.ReplyToMessageID, &item.Body, &item.CreatedAt); err != nil {
			core.WriteError(w, err)
			return
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		core.WriteError(w, err)
		return
	}
	if len(page.Items) > limit {
		page.HasMore = true
		page.Items = page.Items[:limit]
		page.Cursor = h.store.Cursor(scope, filter, strconv.FormatInt(page.Items[len(page.Items)-1].sequence, 10))
	}
	core.WriteJSON(w, http.StatusOK, page)
}

func (h *handler) seen(w http.ResponseWriter, r *http.Request) { h.transition(w, r, false) }
func (h *handler) ack(w http.ResponseWriter, r *http.Request)  { h.transition(w, r, true) }

func (h *handler) transition(w http.ResponseWriter, r *http.Request, ack bool) {
	scope, err := h.store.Scope(r, "write")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if scope.WorkerID == "" {
		core.WriteError(w, core.Fail("validation_failed", "Select the recipient with X-FC-Worker.", http.StatusBadRequest))
		return
	}
	if err := queryKeys(r); err != nil {
		core.WriteError(w, err)
		return
	}
	var request struct{}
	if err := core.ReadJSON(w, r, &request); err != nil {
		core.WriteError(w, err)
		return
	}
	id := r.PathValue("message_id")
	operation, outcome := "messages.seen", "seen"
	if ack {
		operation, outcome = "messages.ack", "acknowledged"
	}
	key, err := mutationKey(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	result, err := h.store.Mutate(r.Context(), scope, core.Mutation{Operation: operation, Target: id, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		var conversationID, recipient string
		if err := tx.QueryRowContext(ctx, "SELECT conversation_id,recipient_worker_id FROM inbox_messages WHERE project_id=? AND id=?", scope.ProjectID, id).Scan(&conversationID, &recipient); err != nil {
			return nil, missing(err)
		}
		if recipient != scope.WorkerID {
			return nil, core.Fail("forbidden", "Only the message recipient can change receipt state.", http.StatusForbidden)
		}
		var seen, acknowledged bool
		var revision int64
		if err := tx.QueryRowContext(ctx, "SELECT seen,acknowledged,revision FROM inbox_recipient_state WHERE project_id=? AND message_id=? AND worker_id=?", scope.ProjectID, id, scope.WorkerID).Scan(&seen, &acknowledged, &revision); err != nil {
			return nil, err
		}
		if (ack && acknowledged) || (!ack && seen) {
			return receipt{ID: id, ConversationID: conversationID, Outcome: "unchanged"}, nil
		}
		acknowledged = acknowledged || ack
		if _, err := tx.ExecContext(ctx, "UPDATE inbox_recipient_state SET seen=1,acknowledged=?,revision=revision+1 WHERE project_id=? AND message_id=? AND worker_id=?", acknowledged, scope.ProjectID, id, scope.WorkerID); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "message."+outcome, id, revision+1, map[string]string{"conversation_id": conversationID, "recipient_worker_id": scope.WorkerID}); err != nil {
			return nil, err
		}
		return receipt{ID: id, ConversationID: conversationID, Outcome: outcome}, nil
	})
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, json.RawMessage(result))
}

func (h *handler) page(r *http.Request, scope core.Scope, filter string) (int, int64, error) {
	limit, err := core.PageLimit(r)
	if err != nil {
		return 0, 0, err
	}
	after, err := h.store.ParseCursor(scope, filter, r.URL.Query().Get("cursor"))
	if err != nil {
		return 0, 0, err
	}
	var sequence int64
	if after != "" {
		sequence, err = strconv.ParseInt(after, 10, 64)
		if err != nil || sequence < 1 {
			return 0, 0, core.Fail("validation_failed", "Use a valid cursor from this list.", http.StatusBadRequest)
		}
	}
	return limit, sequence, nil
}

func cleanID(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 256
}

func mutationKey(r *http.Request) (string, error) {
	if len(r.Header.Values("Idempotency-Key")) != 1 {
		return "", core.Fail("validation_failed", "Select one Idempotency-Key for this mutation.", http.StatusBadRequest)
	}
	return r.Header.Get("Idempotency-Key"), nil
}

func missing(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return core.Fail("not_found", "The selected resource does not exist in this project.", http.StatusNotFound)
	}
	return err
}

func preview(body string) (string, bool) {
	runes := []rune(body)
	if len(runes) > PreviewRunes {
		return string(runes[:PreviewRunes]), true
	}
	return body, false
}

func queryKeys(r *http.Request, allowed ...string) error {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return core.Fail("validation_failed", "Use valid URL query encoding.", http.StatusBadRequest)
	}
	for key, values := range query {
		known := false
		for _, candidate := range allowed {
			if candidate == key {
				known = true
				break
			}
		}
		if !known || len(values) != 1 {
			return core.Fail("validation_failed", "Use supported query fields once each.", http.StatusBadRequest)
		}
	}
	return nil
}

func boolFilter(r *http.Request, key string) (bool, error) {
	values, present := r.URL.Query()[key]
	if !present {
		return false, nil
	}
	if len(values) == 1 {
		switch values[0] {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, core.Fail("validation_failed", "Inbox filters must be true or false.", http.StatusBadRequest)
}
