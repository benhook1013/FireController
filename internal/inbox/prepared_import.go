package inbox

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/coordimportmodel"
	"github.com/benhook1013/FireController/internal/core"
)

type PreparedMessage = coordimportmodel.PreparedMessage

type PreparedMessageSnapshotReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var preparedMessageID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func preparedMessageInvalid() error {
	return core.Fail("validation_failed", "Prepared message snapshot is invalid.", 400)
}
func preparedMessageKey(value string) bool {
	return len(value) <= 200 && utf8.ValidString(value) && strings.TrimSpace(value) != ""
}
func preparedMessageTime(value string) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, value)
	return at, len(value) <= 64 && err == nil && !at.IsZero()
}

// NormalizePreparedMessage validates without changing original text or times.
// Missing authors must be explicitly mapped by the caller, never inferred here.
func NormalizePreparedMessage(item PreparedMessage) (PreparedMessage, error) {
	if !preparedMessageKey(item.SourceKey) || item.ReplyToSourceKey != "" && (!preparedMessageKey(item.ReplyToSourceKey) || item.ReplyToSourceKey == item.SourceKey) || !preparedMessageID.MatchString(item.SenderWorkerID) || !preparedMessageID.MatchString(item.RecipientWorkerID) || !utf8.ValidString(item.Body) || strings.TrimSpace(item.Body) == "" || len(item.Body) > MaxBodyBytes {
		return PreparedMessage{}, preparedMessageInvalid()
	}
	created, ok := preparedMessageTime(item.CreatedAt)
	if !ok {
		return PreparedMessage{}, preparedMessageInvalid()
	}
	var seen time.Time
	if item.SeenAt != "" {
		seen, ok = preparedMessageTime(item.SeenAt)
		if !ok || seen.Before(created) {
			return PreparedMessage{}, preparedMessageInvalid()
		}
	}
	if item.AcknowledgedAt != "" {
		ack, valid := preparedMessageTime(item.AcknowledgedAt)
		if item.SeenAt == "" || !valid || ack.Before(seen) {
			return PreparedMessage{}, preparedMessageInvalid()
		}
	}
	return item, nil
}
func preparedMessageSelection(project, message, conversation, reply string, item PreparedMessage) error {
	if !preparedMessageID.MatchString(project) || !preparedMessageID.MatchString(message) || !preparedMessageID.MatchString(conversation) || reply != "" && !preparedMessageID.MatchString(reply) || reply == message || (item.ReplyToSourceKey == "") != (reply == "") {
		return preparedMessageInvalid()
	}
	return nil
}

// preparedMessageGraph follows strictly decreasing insertion sequences. A
// corrupt cycle terminates as invalid; an absent/foreign ancestor cannot reach
// the one root of this same-project, same-participant conversation.
func preparedMessageGraph(ctx context.Context, q PreparedMessageSnapshotReader, project, conversation, reference string, item PreparedMessage) error {
	var valid bool
	err := q.QueryRowContext(ctx, `WITH RECURSIVE ancestry(id,sequence,parent,conversation,sender,recipient,valid) AS (
 SELECT id,sequence,reply_to_message_id,conversation_id,sender_worker_id,recipient_worker_id,
  typeof(id)='text' AND typeof(sequence)='integer' AND sequence>0 AND typeof(conversation_id)='text' AND typeof(sender_worker_id)='text' AND typeof(recipient_worker_id)='text' AND (reply_to_message_id IS NULL OR typeof(reply_to_message_id)='text')
 FROM inbox_messages WHERE project_id=? AND id=?
 UNION ALL
 SELECT p.id,p.sequence,p.reply_to_message_id,p.conversation_id,p.sender_worker_id,p.recipient_worker_id,
  typeof(p.id)='text' AND typeof(p.sequence)='integer' AND p.sequence>0 AND p.sequence<a.sequence AND typeof(p.conversation_id)='text' AND typeof(p.sender_worker_id)='text' AND typeof(p.recipient_worker_id)='text' AND (p.reply_to_message_id IS NULL OR typeof(p.reply_to_message_id)='text')
 FROM ancestry a JOIN inbox_messages p ON p.project_id=? AND p.id=a.parent WHERE a.parent IS NOT NULL AND a.valid=1
 ) SELECT
 (SELECT count(*) FROM inbox_conversations WHERE project_id=? AND id=?)=1
 AND (SELECT count(*) FROM inbox_messages WHERE project_id=? AND conversation_id=? AND reply_to_message_id IS NULL)=1
 AND (SELECT count(*) FROM ancestry a JOIN inbox_conversations c ON c.project_id=? AND c.id=? AND c.root_message_id=a.id WHERE a.parent IS NULL)=1
 AND NOT EXISTS(SELECT 1 FROM ancestry WHERE valid<>1 OR conversation<>? OR NOT ((sender=? AND recipient=?) OR (sender=? AND recipient=?)))
 AND (SELECT count(*) FROM workers WHERE project_id=? AND id=?)=1
 AND (SELECT count(*) FROM workers WHERE project_id=? AND id=?)=1`,
		project, reference, project, project, conversation, project, conversation, project, conversation, conversation, item.SenderWorkerID, item.RecipientWorkerID, item.RecipientWorkerID, item.SenderWorkerID, project, item.SenderWorkerID, project, item.RecipientWorkerID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return preparedMessageInvalid()
	}
	return nil
}

// InsertPreparedMessage writes historical native rows only. The caller owns
// authorization, source mappings, transaction rollback, and any import event.
// Historical sends/handling do not emit message events, receipts, or hints.
func InsertPreparedMessage(ctx context.Context, tx *sql.Tx, projectID, messageID, conversationID, replyID string, item PreparedMessage) error {
	if ctx == nil || tx == nil {
		return preparedMessageInvalid()
	}
	item, err := NormalizePreparedMessage(item)
	if err != nil {
		return err
	}
	if err = preparedMessageSelection(projectID, messageID, conversationID, replyID, item); err != nil {
		return err
	}
	for _, worker := range []string{item.SenderWorkerID, item.RecipientWorkerID} {
		if err = core.CheckWorker(ctx, tx, projectID, worker); err != nil {
			return err
		}
	}
	if replyID == "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_conversations(project_id,id,root_message_id) VALUES(?,?,?)`, projectID, conversationID, messageID); err != nil {
			return err
		}
	} else {
		if err = preparedMessageGraph(ctx, tx, projectID, conversationID, replyID, item); err != nil {
			return err
		}
	}
	var parent any
	if replyID != "" {
		parent = replyID
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_messages(project_id,id,conversation_id,sender_worker_id,recipient_worker_id,reply_to_message_id,body,created_at) VALUES(?,?,?,?,?,?,?,?)`, projectID, messageID, conversationID, item.SenderWorkerID, item.RecipientWorkerID, parent, item.Body, item.CreatedAt); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO inbox_recipient_state(project_id,message_id,worker_id,seen,acknowledged,revision) VALUES(?,?,?,?,?,1)`, projectID, messageID, item.RecipientWorkerID, item.SeenAt != "", item.AcknowledgedAt != "")
	return err
}

// ValidatePreparedMessageSnapshot checks immutable originals and source graph,
// allowing only monotone native receipt evolution after the imported flags.
func ValidatePreparedMessageSnapshot(ctx context.Context, q PreparedMessageSnapshotReader, projectID, messageID, conversationID, replyID string, item PreparedMessage) error {
	if ctx == nil || q == nil {
		return preparedMessageInvalid()
	}
	item, err := NormalizePreparedMessage(item)
	if err != nil {
		return err
	}
	if err = preparedMessageSelection(projectID, messageID, conversationID, replyID, item); err != nil {
		return err
	}
	var sender, recipient, body, created, parent, conversation string
	var typed bool
	var count, sequence int64
	err = q.QueryRowContext(ctx, `SELECT sequence,conversation_id,sender_worker_id,recipient_worker_id,COALESCE(reply_to_message_id,''),body,created_at,
 typeof(sequence)='integer' AND typeof(project_id)='text' AND typeof(id)='text' AND typeof(conversation_id)='text' AND typeof(sender_worker_id)='text' AND typeof(recipient_worker_id)='text' AND (reply_to_message_id IS NULL OR typeof(reply_to_message_id)='text') AND typeof(body)='text' AND typeof(created_at)='text',
 (SELECT count(*) FROM inbox_messages WHERE project_id=? AND id=?)
 FROM inbox_messages WHERE project_id=? AND id=?`, projectID, messageID, projectID, messageID).Scan(&sequence, &conversation, &sender, &recipient, &parent, &body, &created, &typed, &count)
	if err != nil {
		return err
	}
	if !typed || count != 1 || sequence < 1 || conversation != conversationID || sender != item.SenderWorkerID || recipient != item.RecipientWorkerID || parent != replyID || body != item.Body || created != item.CreatedAt {
		return preparedMessageInvalid()
	}
	if err = preparedMessageGraph(ctx, q, projectID, conversationID, messageID, item); err != nil {
		return err
	}
	var seen, ack, revision, states int64
	var worker string
	err = q.QueryRowContext(ctx, `SELECT worker_id,seen,acknowledged,revision,(SELECT count(*) FROM inbox_recipient_state WHERE project_id=? AND message_id=?) FROM inbox_recipient_state WHERE project_id=? AND message_id=? AND typeof(project_id)='text' AND typeof(message_id)='text' AND typeof(worker_id)='text' AND typeof(seen)='integer' AND typeof(acknowledged)='integer' AND typeof(revision)='integer'`, projectID, messageID, projectID, messageID).Scan(&worker, &seen, &ack, &revision, &states)
	if err != nil {
		return err
	}
	if states != 1 || worker != item.RecipientWorkerID || seen < 0 || seen > 1 || ack < 0 || ack > seen || item.SeenAt != "" && seen != 1 || item.AcknowledgedAt != "" && ack != 1 {
		return preparedMessageInvalid()
	}
	// Native transitions never increment an already-satisfied receipt. Starting
	// unread can reach ack directly (revision2) or through seen (revision3).
	expected := int64(1)
	if item.SeenAt == "" && seen == 1 {
		expected++
	}
	if item.AcknowledgedAt == "" && ack == 1 && item.SeenAt != "" {
		expected++
	}
	if revision != expected && !(item.SeenAt == "" && ack == 1 && revision == 3) {
		return preparedMessageInvalid()
	}
	return nil
}
