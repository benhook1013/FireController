package inbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

func originRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", core.InboxOriginHeader)
		next(w, r.WithContext(core.WithControllerInbox(r.Context(), core.RequestsControllerInbox(r))))
	}
}
func originFilter(ctx context.Context, filter string) string {
	if core.IncludesControllerInbox(ctx) {
		return filter + ":origin=controller-v1"
	}
	return filter
}
func originSQL(ctx context.Context, prefix string) string {
	if core.IncludesControllerInbox(ctx) {
		return ""
	}
	return " AND " + prefix + "origin_kind='worker'"
}
func wireOrigin(stored string) string {
	if stored == "worker" {
		return ""
	}
	return stored
}
func validMessageOrigin(origin, sender string) bool {
	return origin == "controller" && sender == "" || origin == "" && cleanID(sender) && utf8.ValidString(sender)
}

// PublishControllerMessage is for trusted internal producers inside their owning
// transaction. Empty ID with nil error means a retired recipient was skipped.
// Scope retains the actual service actor; it does not impersonate a worker.
func PublishControllerMessage(ctx context.Context, tx *sql.Tx, scope core.Scope, key, recipientWorkerID, body string) (string, error) {
	if ctx == nil || tx == nil || !cleanID(scope.ProjectID) || !cleanID(scope.PrincipalID) || !cleanID(recipientWorkerID) || len(key) < 1 || len(key) > 512 || !utf8.ValidString(key) || strings.TrimSpace(key) != key || strings.TrimSpace(body) == "" || len(body) > MaxBodyBytes || !utf8.ValidString(body) {
		return "", core.Fail("validation_failed", "Controller notification input is invalid.", 400)
	}
	framed, _ := json.Marshal([]string{scope.ProjectID, recipientWorkerID, key})
	digest := sha256.Sum256(framed)
	suffix := hex.EncodeToString(digest[:16])
	id, conversation := "msg_"+suffix, "conv_"+suffix
	var storedBody, storedConversation, storedRecipient, origin string
	var sender, reply sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT body,conversation_id,recipient_worker_id,origin_kind,sender_worker_id,reply_to_message_id FROM inbox_messages WHERE project_id=? AND id=?`, scope.ProjectID, id).Scan(&storedBody, &storedConversation, &storedRecipient, &origin, &sender, &reply)
	if err == nil {
		if storedBody != body || storedConversation != conversation || storedRecipient != recipientWorkerID || origin != "controller" || sender.Valid || reply.Valid {
			return "", core.Fail("idempotency_conflict", "The Controller notification key already identifies different immutable content.", 409)
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err = core.CheckActiveWorker(ctx, tx, scope.ProjectID, recipientWorkerID); err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "worker_retired" {
			return "", nil
		}
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_conversations(project_id,id,root_message_id) VALUES(?,?,?)`, scope.ProjectID, conversation, id); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_messages(project_id,id,conversation_id,sender_worker_id,recipient_worker_id,reply_to_message_id,body,created_at,origin_kind) VALUES(?,?,?,NULL,?,NULL,?,?,'controller')`, scope.ProjectID, id, conversation, recipientWorkerID, body, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_recipient_state(project_id,message_id,worker_id) VALUES(?,?,?)`, scope.ProjectID, id, recipientWorkerID); err != nil {
		return "", err
	}
	if err = core.AppendEvent(ctx, tx, scope, "message.sent", id, 1, map[string]string{"conversation_id": conversation, "recipient_worker_id": recipientWorkerID, "sender_worker_id": "", "origin_kind": "controller"}); err != nil {
		return "", err
	}
	return id, nil
}
