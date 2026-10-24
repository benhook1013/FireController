package inbox

import (
	"net/http"

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
	err = h.store.DB().QueryRowContext(r.Context(), `SELECT id,conversation_id,sender_worker_id,recipient_worker_id,COALESCE(reply_to_message_id,''),body,created_at FROM inbox_messages WHERE project_id=? AND id=?`, scope.ProjectID, r.PathValue("message_id")).Scan(&value.ID, &value.ConversationID, &value.SenderWorkerID, &value.RecipientWorkerID, &value.ReplyToMessageID, &value.Body, &value.CreatedAt)
	if err != nil {
		core.WriteError(w, missing(err))
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}
