package coordimport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const messageSchemaSQL = `CREATE TABLE coordination_import_messages(project_id TEXT NOT NULL,source TEXT NOT NULL,source_key TEXT NOT NULL,message_id TEXT NOT NULL,conversation_id TEXT NOT NULL,reply_message_id TEXT NOT NULL,import_revision INTEGER NOT NULL,item_sha256 TEXT NOT NULL,source_sha256 TEXT NOT NULL,item_json TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(project_id,source,source_key),UNIQUE(project_id,message_id),FOREIGN KEY(project_id,message_id) REFERENCES inbox_messages(project_id,id),FOREIGN KEY(project_id,conversation_id) REFERENCES inbox_conversations(project_id,id),FOREIGN KEY(project_id,import_revision) REFERENCES coordination_import_batches(project_id,revision) DEFERRABLE INITIALLY DEFERRED);`

// MessageSchemaSQL returns the additive v2 table; the v1 schema is unchanged.
func MessageSchemaSQL() string { return messageSchemaSQL }

type messagePlan struct {
	item                    inbox.PreparedMessage
	id, conversation, reply string
	fresh                   bool
}

type retainedMessage struct {
	item                    inbox.PreparedMessage
	id, conversation, reply string
}

func readMessage(ctx context.Context, q SnapshotReader, project, source, key string) (retainedMessage, error) {
	var v retainedMessage
	var body, hash string
	err := q.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(message_id AS BLOB))<=128 THEN message_id END,CASE WHEN length(CAST(conversation_id AS BLOB))<=128 THEN conversation_id END,CASE WHEN length(CAST(reply_message_id AS BLOB))<=128 THEN reply_message_id END,CASE WHEN length(CAST(item_json AS BLOB))<=262144 THEN item_json END,item_sha256 FROM coordination_import_messages WHERE project_id=? AND source=? AND source_key=?`, project, source, key).Scan(&v.id, &v.conversation, &v.reply, &body, &hash)
	if err != nil {
		return v, err
	}
	if strictjson.Decode([]byte(body), &v.item) != nil || v.item.SourceKey != key || digest([]byte(body)) != hash || v.id != resourceID(project, source, "message", "", key) {
		return v, conflict()
	}
	item, err := inbox.NormalizePreparedMessage(v.item)
	if err != nil {
		return v, err
	}
	normalized, _ := json.Marshal(item)
	if string(normalized) != body {
		return v, conflict()
	}
	if err = inbox.ValidatePreparedMessageSnapshot(ctx, q, project, v.id, v.conversation, v.reply, v.item); err != nil {
		// Only the provenance lookup's ErrNoRows means a new selection. A mapped
		// message with missing native rows is corruption, never a fresh import.
		if errors.Is(err, sql.ErrNoRows) {
			return v, conflict()
		}
		return v, err
	}
	if item.ReplyToSourceKey == "" {
		if v.reply != "" || v.conversation != resourceID(project, source, "conversation", "", key) {
			return v, conflict()
		}
	} else if v.reply != resourceID(project, source, "message", "", item.ReplyToSourceKey) {
		return v, conflict()
	}
	return v, nil
}

// Admission resolves only explicit selected or retained source mappings. DFS is
// bounded by the selected batch, and insertion order is independent of its wire order.
func admitMessages(ctx context.Context, tx *sql.Tx, project string, batch Batch) ([]messagePlan, error) {
	plans := make([]messagePlan, len(batch.Messages))
	selected := map[string]int{}
	for i, item := range batch.Messages {
		selected[item.SourceKey] = i
	}
	state := make([]uint8, len(plans))
	var resolve func(int) error
	resolve = func(i int) error {
		if state[i] == 2 {
			return nil
		}
		if state[i] == 1 {
			return conflict()
		}
		state[i] = 1
		item := batch.Messages[i]
		if err := core.CheckWorker(ctx, tx, project, item.SenderWorkerID); err != nil {
			return err
		}
		if err := core.CheckWorker(ctx, tx, project, item.RecipientWorkerID); err != nil {
			return err
		}
		p := messagePlan{item: item, id: resourceID(project, batch.Source, "message", "", item.SourceKey), fresh: true}
		if item.ReplyToSourceKey == "" {
			p.conversation = resourceID(project, batch.Source, "conversation", "", item.SourceKey)
		} else {
			var parent inbox.PreparedMessage
			if j, ok := selected[item.ReplyToSourceKey]; ok {
				if err := resolve(j); err != nil {
					return err
				}
				p.reply = plans[j].id
				p.conversation = plans[j].conversation
				parent = plans[j].item
			} else {
				retained, err := readMessage(ctx, tx, project, batch.Source, item.ReplyToSourceKey)
				if errors.Is(err, sql.ErrNoRows) {
					return conflict()
				}
				if err != nil {
					return err
				}
				p.reply = retained.id
				p.conversation = retained.conversation
				parent = retained.item
			}
			if !((parent.SenderWorkerID == item.SenderWorkerID && parent.RecipientWorkerID == item.RecipientWorkerID) || (parent.SenderWorkerID == item.RecipientWorkerID && parent.RecipientWorkerID == item.SenderWorkerID)) {
				return conflict()
			}
		}
		old, err := readMessage(ctx, tx, project, batch.Source, item.SourceKey)
		if err == nil {
			a, _ := json.Marshal(old.item)
			b, _ := json.Marshal(item)
			if string(a) != string(b) || old.id != p.id || old.conversation != p.conversation || old.reply != p.reply {
				return conflict()
			}
			p.fresh = false
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		} else {
			var exists bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM inbox_messages WHERE project_id=? AND id=?)`, project, p.id).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return conflict()
			}
			if p.reply == "" {
				if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM inbox_conversations WHERE project_id=? AND id=?)`, project, p.conversation).Scan(&exists); err != nil {
					return err
				}
				if exists {
					return conflict()
				}
			}
		}
		plans[i] = p
		state[i] = 2
		return nil
	}
	for i := range plans {
		if err := resolve(i); err != nil {
			return nil, err
		}
	}
	return plans, nil
}
func countMessages(counts *Counts, plans []messagePlan) {
	counts.Messages = len(plans)
	for _, p := range plans {
		if p.fresh {
			counts.NewMessages++
		} else {
			counts.UnchangedMessages++
		}
	}
}
func insertMessages(ctx context.Context, tx *sql.Tx, project string, batch Batch, plans []messagePlan, now string, receipt *Receipt) error {
	if len(plans) == 0 {
		return nil
	}
	receipt.MessageMappings = make([]MessageMapping, 0, len(plans))
	byID := map[string]int{}
	for i, p := range plans {
		byID[p.id] = i
		receipt.MessageMappings = append(receipt.MessageMappings, MessageMapping{SourceKey: p.item.SourceKey, MessageID: p.id, ConversationID: p.conversation})
	}
	done := make([]bool, len(plans))
	var insert func(int) error
	insert = func(i int) error {
		if done[i] {
			return nil
		}
		p := plans[i]
		if j, ok := byID[p.reply]; ok {
			if err := insert(j); err != nil {
				return err
			}
		}
		if p.fresh {
			if err := inbox.InsertPreparedMessage(ctx, tx, project, p.id, p.conversation, p.reply, p.item); err != nil {
				return err
			}
			body, _ := json.Marshal(p.item)
			if _, err := tx.ExecContext(ctx, `INSERT INTO coordination_import_messages(project_id,source,source_key,message_id,conversation_id,reply_message_id,import_revision,item_sha256,source_sha256,item_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, project, batch.Source, p.item.SourceKey, p.id, p.conversation, p.reply, *batch.ExpectedRevision+1, digest(body), batch.SourceSHA256, string(body), now); err != nil {
				return err
			}
		}
		done[i] = true
		return nil
	}
	for i := range plans {
		if err := insert(i); err != nil {
			return err
		}
	}
	return nil
}
