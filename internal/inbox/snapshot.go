package inbox

import (
	"context"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

func inboxDefinition(sql string) string {
	sql = strings.ReplaceAll(sql, "\"inbox_messages\"", "inbox_messages")
	sql = strings.ReplaceAll(sql, "IF NOT EXISTS ", "")
	return strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimSpace(sql), ";")), " ")
}

// ValidateSnapshot checks retained origin/receipt/conversation authority without
// migrating old bundles or manufacturing delivery events for imported workers.
func ValidateSnapshot(ctx context.Context, q queryer, version int) error {
	invalid := func() error {
		return core.Fail("storage_unavailable", "Stored inbox origins or conversation references are unavailable.", 500)
	}
	if version != 1 && version != 2 {
		return invalid()
	}
	if version == 2 {
		var ddl string
		if q.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql END FROM sqlite_schema WHERE type='table' AND name='inbox_messages'`).Scan(&ddl) != nil || inboxDefinition(ddl) != inboxDefinition(SchemaSQL(2)[1]) {
			return invalid()
		}
	}
	origin := "'worker'"
	if version == 2 {
		origin = "m.origin_kind"
	}
	query := `SELECT EXISTS(SELECT 1 FROM inbox_conversations c LEFT JOIN inbox_messages r ON r.project_id=c.project_id AND r.id=c.root_message_id WHERE r.id IS NULL OR r.conversation_id<>c.id OR r.reply_to_message_id IS NOT NULL)
 OR EXISTS(SELECT 1 FROM inbox_messages m LEFT JOIN inbox_conversations c ON c.project_id=m.project_id AND c.id=m.conversation_id WHERE c.id IS NULL OR NOT EXISTS(SELECT 1 FROM inbox_recipient_state s WHERE s.project_id=m.project_id AND s.message_id=m.id AND s.worker_id=m.recipient_worker_id)
 OR EXISTS(SELECT 1 FROM inbox_recipient_state s WHERE s.project_id=m.project_id AND s.message_id=m.id AND s.worker_id<>m.recipient_worker_id)
 OR (` + origin + `='worker' AND m.sender_worker_id IS NULL)
 OR (` + origin + `='controller' AND (m.sender_worker_id IS NOT NULL OR m.reply_to_message_id IS NOT NULL OR c.root_message_id<>m.id OR EXISTS(SELECT 1 FROM inbox_messages x WHERE x.project_id=m.project_id AND x.conversation_id=m.conversation_id AND x.id<>m.id)))
 OR (` + origin + ` NOT IN ('worker','controller'))
 OR (m.reply_to_message_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM inbox_messages p WHERE p.project_id=m.project_id AND p.id=m.reply_to_message_id AND p.conversation_id=m.conversation_id AND p.sequence<m.sequence)))`
	var bad bool
	if err := q.QueryRowContext(ctx, query).Scan(&bad); err != nil || bad {
		return invalid()
	}
	if version == 2 {
		err := q.QueryRowContext(ctx, `WITH sent AS MATERIALIZED (
 SELECT e.project_id,e.resource_id,count(*) AS total,
 sum(CASE WHEN e.actor_id<>'' AND json_extract(e.payload_json,'$.origin_kind')='controller' AND json_extract(e.payload_json,'$.sender_worker_id')='' AND json_extract(e.payload_json,'$.recipient_worker_id')=m.recipient_worker_id AND json_extract(e.payload_json,'$.conversation_id')=m.conversation_id THEN 1 ELSE 0 END) AS valid
 FROM events e CROSS JOIN inbox_messages m ON m.project_id=e.project_id AND m.id=e.resource_id
 WHERE e.event_type='message.sent' AND m.origin_kind='controller' GROUP BY e.project_id,e.resource_id)
 SELECT EXISTS(SELECT 1 FROM inbox_messages m LEFT JOIN sent s ON s.project_id=m.project_id AND s.resource_id=m.id WHERE m.origin_kind='controller' AND (coalesce(s.total,0)<>1 OR coalesce(s.valid,0)<>1))
 OR EXISTS(SELECT 1 FROM inbox_messages x JOIN inbox_messages p ON p.project_id=x.project_id AND p.id=x.reply_to_message_id WHERE p.origin_kind='controller')`).Scan(&bad)
		if err != nil || bad {
			return invalid()
		}
	}
	return nil
}
