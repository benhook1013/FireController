package inbox

import (
	"database/sql"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

// readMessage reads one immutable body without loading a conversation or changing
// recipient receipt state. Project read authority matches conversation reads.
func (h *handler) readMessage(w http.ResponseWriter, r *http.Request) {
	scope, err := h.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var value message
	err = h.store.DB().QueryRowContext(r.Context(), `SELECT id,conversation_id,COALESCE(sender_worker_id,''),recipient_worker_id,COALESCE(reply_to_message_id,''),body,created_at,origin_kind FROM inbox_messages WHERE project_id=? AND id=?`+originSQL(r.Context(), ""), scope.ProjectID, r.PathValue("message_id")).Scan(&value.ID, &value.ConversationID, &value.SenderWorkerID, &value.RecipientWorkerID, &value.ReplyToMessageID, &value.Body, &value.CreatedAt, &value.OriginKind)
	if err != nil {
		core.WriteError(w, missing(err))
		return
	}
	value.OriginKind = wireOrigin(value.OriginKind)
	if !validMessageOrigin(value.OriginKind, value.SenderWorkerID) {
		core.WriteError(w, core.Fail("storage_unavailable", "Stored message origin is unavailable.", 500))
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}

type messageDelivery struct {
	OriginKind        string `json:"origin_kind,omitempty"`
	ID                string `json:"id"`
	SenderWorkerID    string `json:"sender_worker_id"`
	RecipientWorkerID string `json:"recipient_worker_id"`
	Seen              bool   `json:"seen"`
	Acknowledged      bool   `json:"acknowledged"`
}

// Delivery is an explicit project read, independent of the reading worker. A
// missing receipt is inconsistent storage, never evidence of an unread message.
func (h *handler) readDelivery(w http.ResponseWriter, r *http.Request) {
	scope, err := h.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var value messageDelivery
	var seen, acknowledged sql.NullBool
	err = h.store.DB().QueryRowContext(r.Context(), `SELECT m.id,COALESCE(m.sender_worker_id,''),m.recipient_worker_id,s.seen,s.acknowledged,m.origin_kind FROM inbox_messages m LEFT JOIN inbox_recipient_state s ON s.project_id=m.project_id AND s.message_id=m.id AND s.worker_id=m.recipient_worker_id WHERE m.project_id=? AND m.id=?`+originSQL(r.Context(), "m."), scope.ProjectID, r.PathValue("message_id")).Scan(&value.ID, &value.SenderWorkerID, &value.RecipientWorkerID, &seen, &acknowledged, &value.OriginKind)
	if errors.Is(err, sql.ErrNoRows) {
		core.WriteError(w, missing(err))
		return
	}
	value.OriginKind = wireOrigin(value.OriginKind)
	if err != nil || !seen.Valid || !acknowledged.Valid || acknowledged.Bool && !seen.Bool || !cleanID(value.ID) || !utf8.ValidString(value.ID) || !validMessageOrigin(value.OriginKind, value.SenderWorkerID) || !cleanID(value.RecipientWorkerID) || !utf8.ValidString(value.RecipientWorkerID) {
		core.WriteError(w, core.Fail("storage_unavailable", "Stored message delivery is unavailable.", http.StatusInternalServerError))
		return
	}
	value.Seen, value.Acknowledged = seen.Bool, acknowledged.Bool
	core.WriteJSON(w, http.StatusOK, value)
}
