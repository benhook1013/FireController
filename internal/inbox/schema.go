package inbox

import (
	"context"
	"database/sql"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

var schemaOne = []string{`
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
	`CREATE INDEX IF NOT EXISTS inbox_conversation_page ON inbox_messages(project_id,conversation_id,sequence)`}

// SchemaSQL retains the exact supported bundle layouts; reads never upgrade them.
func SchemaSQL(version int) []string {
	out := append([]string(nil), schemaOne...)
	if version == 2 {
		out[1] = strings.Replace(out[1], "sender_worker_id TEXT NOT NULL,", "sender_worker_id TEXT,\n origin_kind TEXT NOT NULL DEFAULT 'worker' CHECK((origin_kind='worker' AND sender_worker_id IS NOT NULL) OR (origin_kind='controller' AND sender_worker_id IS NULL AND reply_to_message_id IS NULL)),", 1)
	}
	return out
}

func EnsureSchema(ctx context.Context, store *core.Store) error {
	ddl := strings.Replace(SchemaSQL(2)[1], "CREATE TABLE IF NOT EXISTS inbox_messages", "CREATE TABLE inbox_messages_next", 1)
	migration := []string{
		"PRAGMA defer_foreign_keys=ON",
		"CREATE TEMP TABLE inbox_sequence_before AS SELECT seq FROM sqlite_sequence WHERE name='inbox_messages'",
		ddl,
		"INSERT INTO inbox_messages_next(sequence,project_id,id,conversation_id,sender_worker_id,recipient_worker_id,reply_to_message_id,body,created_at) SELECT sequence,project_id,id,conversation_id,sender_worker_id,recipient_worker_id,reply_to_message_id,body,created_at FROM inbox_messages",
		"DROP TABLE inbox_messages",
		"ALTER TABLE inbox_messages_next RENAME TO inbox_messages",
		"INSERT INTO sqlite_sequence(name,seq) SELECT 'inbox_messages',max(seq) FROM inbox_sequence_before HAVING count(*)>0 AND NOT EXISTS(SELECT 1 FROM sqlite_sequence WHERE name='inbox_messages')",
		"UPDATE sqlite_sequence SET seq=max(seq,coalesce((SELECT max(seq) FROM inbox_sequence_before),0)) WHERE name='inbox_messages'",
		"DELETE FROM sqlite_sequence WHERE name='inbox_messages' AND NOT EXISTS(SELECT 1 FROM inbox_sequence_before)",
		"DROP TABLE inbox_sequence_before",
		SchemaSQL(2)[3], SchemaSQL(2)[4],
	}
	return store.EnsureSchemaWithBackfill(ctx, "inbox", [][]string{schemaOne, migration}, func(ctx context.Context, conn *sql.Conn) error {
		// DROP/rebuild can leave SQLite's deferred violation counter pending
		// even after all same-ID parents have been restored. Check the actual
		// complete graph before restoring ordinary FK enforcement and commit.
		rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
		if err != nil {
			return err
		}
		bad := rows.Next()
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		if bad {
			return core.Fail("schema_invalid", "Inbox migration has inconsistent foreign-key references.", 500)
		}
		_, err = conn.ExecContext(ctx, "PRAGMA defer_foreign_keys=OFF")
		return err
	})
}
