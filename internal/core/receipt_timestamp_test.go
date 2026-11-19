package core

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestReceiptTimestampMigrationAndReplay(t *testing.T) {
	s, scope, _ := streamFixture(t)
	var seq int
	var name, path string
	if err := s.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	m := Mutation{Operation: "receipt.timestamp", Target: "target", Key: "same-key"}
	calls := 0
	apply := func(context.Context, *sql.Tx) (any, error) {
		calls++
		return map[string]string{"value": "exact bytes"}, nil
	}
	start := time.Now()
	original, err := s.Mutate(t.Context(), scope, m, apply)
	if err != nil {
		t.Fatal(err)
	}
	readTimestamp := func(store *Store) string {
		t.Helper()
		var stamp string
		if err := store.DB().QueryRow(`SELECT created_at FROM receipts WHERE key=?`, m.Key).Scan(&stamp); err != nil {
			t.Fatal(err)
		}
		return stamp
	}
	stamp := readTimestamp(s)
	if stamp < ReceiptTimestamp(start) || stamp > ReceiptTimestamp(time.Now()) {
		t.Fatalf("new receipt timestamp outside mutation window: %q", stamp)
	}
	got, err := s.Mutate(t.Context(), scope, m, apply)
	if err != nil || string(got) != string(original) || readTimestamp(s) != stamp || calls != 1 {
		t.Fatalf("exact replay changed the timestamp, response or callback: %d %s %v", calls, got, err)
	}
	if _, err := s.DB().Exec(`ALTER TABLE receipts DROP COLUMN created_at; PRAGMA user_version=2`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	migrationStart := time.Now()
	opened, err := Open(t.Context(), filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	var version int
	if err := opened.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 3 {
		t.Fatalf("base v2 was not upgraded: version=%d err=%v", version, err)
	}
	migratedStamp := readTimestamp(opened)
	if migratedStamp < ReceiptTimestamp(migrationStart) || migratedStamp > ReceiptTimestamp(time.Now()) {
		t.Fatalf("legacy receipt was not stamped at migration: %q", migratedStamp)
	}
	replayed, found, err := opened.Replay(t.Context(), scope, m)
	if err != nil || !found || string(replayed) != string(original) || readTimestamp(opened) != migratedStamp {
		t.Fatalf("migration changed the response or replay refreshed timestamp: %s %v", replayed, err)
	}
}
