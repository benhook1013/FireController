package backup

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
)

func TestControllerInboxBackupRetainsOriginsPreferencesAndInactiveRestore(t *testing.T) {
	f := newRestoreFixture(t)
	var id string
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		id, err = inbox.PublishControllerMessage(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "service-lifecycle-sync"}, "backup-notice", f.worker, "PR #2 merged")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.DB().Exec(`INSERT INTO review_pr_followers(project_id,pull_number,worker_id,mode,revision,updated_at) VALUES(?,2,?,'muted',3,'2026-11-02T00:00:00Z')`, f.project, f.worker); err != nil {
		t.Fatal(err)
	}
	bundle := restoreBundle(t, f)
	if _, err = Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if _, err = Restore(t.Context(), bundle.Path, dest); err != nil {
		t.Fatal(err)
	}
	got := generationRead(t, dest, `SELECT origin_kind,sender_worker_id,body FROM inbox_messages WHERE id='`+id+`'`)
	if len(got) != 1 || got[0][0] != "controller" || got[0][1] != nil || got[0][2] != "PR #2 merged" {
		t.Fatal("restore changed Controller origin", got)
	}
	if got := generationRead(t, dest, `SELECT mode,revision FROM review_pr_followers`); len(got) != 1 || got[0][0] != "muted" || got[0][1] != int64(3) {
		t.Fatal("notification preference not retained", got)
	}
	for name, query := range map[string]string{
		"worker_impersonation":       `PRAGMA ignore_check_constraints=ON;UPDATE inbox_messages SET sender_worker_id=recipient_worker_id WHERE id='` + id + `'`,
		"unknown_origin":             `PRAGMA ignore_check_constraints=ON;UPDATE inbox_messages SET origin_kind='other' WHERE id='` + id + `'`,
		"missing_receipt":            `DELETE FROM inbox_recipient_state WHERE message_id='` + id + `'`,
		"missing_actor":              `UPDATE events SET actor_id='' WHERE event_type='message.sent' AND resource_id='` + id + `'`,
		"invalid_preference":         `PRAGMA ignore_check_constraints=ON;UPDATE review_pr_followers SET mode='other'`,
		"worker_reply_to_controller": fmt.Sprintf(`INSERT INTO inbox_messages(project_id,id,conversation_id,sender_worker_id,recipient_worker_id,reply_to_message_id,body,created_at) SELECT project_id,'msg_forbidden_reply',conversation_id,recipient_worker_id,recipient_worker_id,id,'reply','2026-11-02T00:00:00Z' FROM inbox_messages WHERE id='%s';INSERT INTO inbox_recipient_state SELECT project_id,id,recipient_worker_id,0,0,1 FROM inbox_messages WHERE id='msg_forbidden_reply'`, id),
	} {
		t.Run(name, func(t *testing.T) {
			path := restoreModifiedBundle(t, bundle, query)
			_, err := Inspect(t.Context(), path)
			backupCode(t, err, "backup_invalid")
		})
	}
}

func TestControllerInboxBackupStillReadsWorkerOnlyVersionOne(t *testing.T) {
	f := newRestoreFixture(t)
	bundle := restoreBundle(t, f)
	old := inbox.SchemaSQL(1)
	ddl := strings.Replace(old[1], "CREATE TABLE IF NOT EXISTS inbox_messages", "CREATE TABLE inbox_messages_old", 1)
	query := `PRAGMA defer_foreign_keys=ON;` + ddl + `;INSERT INTO inbox_messages_old(sequence,project_id,id,conversation_id,sender_worker_id,recipient_worker_id,reply_to_message_id,body,created_at) SELECT sequence,project_id,id,conversation_id,sender_worker_id,recipient_worker_id,reply_to_message_id,body,created_at FROM inbox_messages;DROP TABLE inbox_messages;ALTER TABLE inbox_messages_old RENAME TO inbox_messages;` + old[3] + `;` + old[4] + `;UPDATE metadata SET value='1' WHERE key='schema:inbox';PRAGMA defer_foreign_keys=OFF`
	path := restoreModifiedBundle(t, bundle, query)
	if _, err := Inspect(t.Context(), path); err != nil {
		t.Fatal("historical inbox1 rejected", err)
	}
	if got := restoreRows(t, f.store.DB(), `SELECT value FROM metadata WHERE key='schema:inbox'`); len(got) != 1 || got[0][0] != "2" {
		t.Fatal("historical read migrated live source", got)
	}
	dest := filepath.Join(t.TempDir(), "old-restored")
	if _, err := Restore(t.Context(), path, dest); err != nil {
		t.Fatal(err)
	}
	if got := generationRead(t, dest, `SELECT value FROM metadata WHERE key='schema:inbox'`); len(got) != 1 || got[0][0] != "1" {
		t.Fatal("historical restore upgraded bundle", got)
	}
}
