package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const receiptV1DDL = `CREATE TABLE receipts(principal_id TEXT NOT NULL,project_id TEXT NOT NULL,operation TEXT NOT NULL,target TEXT NOT NULL,key TEXT NOT NULL,fingerprint TEXT NOT NULL,response_json TEXT NOT NULL,PRIMARY KEY(principal_id,project_id,operation,target,key))`

func receiptRows(t *testing.T, db *sql.DB, query string) [][]string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	result := [][]string{}
	for rows.Next() {
		values := make([]string, len(columns))
		scan := make([]any, len(columns))
		for i := range values {
			scan[i] = &values[i]
		}
		if err := rows.Scan(scan...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func receiptCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

// Start from genuine core mutations, then retain precisely the old supported
// receipt DDL. This avoids depending on the new schema to invent v1 row bytes.
func receiptLegacyFixture(t *testing.T) (string, *Store, Scope, Mutation) {
	t.Helper()
	s, scope, _ := streamFixture(t)
	mutation := Mutation{Operation: "origin.probe", Target: "resource", Key: "origin-key", Request: map[string]string{"value": "original"}}
	for _, key := range []string{mutation.Key, "second-key"} {
		m := mutation
		m.Key = key
		_, err := s.Mutate(t.Context(), scope, m, func(ctx context.Context, tx *sql.Tx) (any, error) {
			if _, err := tx.ExecContext(ctx, `UPDATE projects SET name='retained history' WHERE id='project'`); err != nil {
				return nil, err
			}
			if err := AppendEvent(ctx, tx, scope, "origin.changed", "resource", 1, map[string]string{"key": key}); err != nil {
				return nil, err
			}
			return map[string]string{"id": "original", "key": key}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Deliberately preserve semantically valid JSON whose exact bytes differ
	// from canonical marshaling, including an escaped Unicode scalar.
	if _, err := s.DB().Exec(`UPDATE receipts SET response_json=? WHERE key=?`, " { \"id\": \"original\", \"literal\": \"\\u03a9\" }\n", mutation.Key); err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, q := range []string{`ALTER TABLE receipts RENAME TO receipts_new`, receiptV1DDL, `INSERT INTO receipts SELECT principal_id,project_id,operation,target,key,fingerprint,response_json FROM receipts_new`, `DROP TABLE receipts_new`, `PRAGMA user_version=1`} {
		if _, err = tx.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, path string
	if err = s.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(path), s, scope, mutation
}

func receiptInactiveMetadata(t *testing.T, s *Store) (string, string) {
	t.Helper()
	origin, current := s.InstanceID(), NewID("instance")
	start := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	finish := time.Now().UTC().Format(time.RFC3339Nano)
	for key, value := range map[string]string{"instance_id": current, "restore:state": "inactive", "restore:source_instance_id": origin, "restore:bundle_id": NewID("backup"), "restore:snapshot_started_at": start, "restore:snapshot_finished_at": finish, "restore:event_sequence": "2"} {
		if _, err := s.DB().Exec(`INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec(`UPDATE credentials SET revoked=1`); err != nil {
		t.Fatal(err)
	}
	return origin, current
}

func TestReceiptOriginMigratesV1BytesAndInactiveProvenance(t *testing.T) {
	for _, inactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "inactive restore"}[inactive], func(t *testing.T) {
			dir, old, scope, mutation := receiptLegacyFixture(t)
			origin, current := old.InstanceID(), old.InstanceID()
			if inactive {
				origin, current = receiptInactiveMetadata(t, old)
			}
			queries := []string{`SELECT key,value FROM metadata ORDER BY key`, `SELECT * FROM credentials ORDER BY id`, `SELECT * FROM grants ORDER BY credential_id,project_id`, `SELECT * FROM events ORDER BY sequence`, `SELECT * FROM projects ORDER BY id`, `SELECT * FROM workers ORDER BY project_id,id`, `SELECT * FROM sqlite_sequence ORDER BY name`}
			unchanged := make([][][]string, len(queries))
			for i, q := range queries {
				unchanged[i] = receiptRows(t, old.DB(), q)
			}
			rows := receiptRows(t, old.DB(), `SELECT principal_id,project_id,operation,target,key,fingerprint,response_json FROM receipts ORDER BY key`)
			for _, row := range rows {
				if !json.Valid([]byte(row[6])) {
					t.Fatal("legacy receipt fixture must retain valid JSON")
				}
			}
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if s.InstanceID() != current {
				t.Fatal("migration changed installation identity")
			}
			if got := receiptRows(t, s.DB(), `SELECT principal_id,project_id,operation,target,key,fingerprint,response_json FROM receipts ORDER BY key`); !reflect.DeepEqual(got, rows) {
				t.Fatal("receipt bytes/fingerprints changed")
			}
			if got := receiptRows(t, s.DB(), `SELECT DISTINCT instance_id FROM receipts`); !reflect.DeepEqual(got, [][]string{{origin}}) {
				t.Fatal("predecessor receipts relabeled", got)
			}
			if got := receiptRows(t, s.DB(), `PRAGMA user_version`); !reflect.DeepEqual(got, [][]string{{"2"}}) {
				t.Fatal("base version not advanced")
			}
			for i, q := range queries {
				if !reflect.DeepEqual(receiptRows(t, s.DB(), q), unchanged[i]) {
					t.Fatalf("non-receipt history changed: %s", q)
				}
			}
			if !inactive {
				result, found, err := s.Replay(t.Context(), scope, mutation)
				if err != nil || !found || string(result) != rows[0][6] {
					t.Fatal("ordinary migrated receipt did not replay exactly", err)
				}
			} else {
				_, _, err := s.Replay(t.Context(), scope, mutation)
				receiptCode(t, err, "unauthorized") // No credential is revived by migration.
			}
			// Reopening v2 never relabels rows, even with retained restore metadata.
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if got := receiptRows(t, s.DB(), `SELECT DISTINCT instance_id FROM receipts`); !reflect.DeepEqual(got, [][]string{{origin}}) {
				t.Fatal("restart relabeled receipt origin")
			}
		})
	}
}

func TestReceiptOriginCurrentAuthReplayConflictAndIndependentHistory(t *testing.T) {
	s, scope, _ := streamFixture(t)
	m := Mutation{Operation: "origin.probe", Target: "resource", Key: "same-key", Request: "original"}
	fingerprint, err := mutationFingerprint(scope, m)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := NewID("instance")
	if _, err := s.DB().Exec(`INSERT INTO receipts(instance_id,principal_id,project_id,operation,target,key,fingerprint,response_json) VALUES(?,?,?,?,?,?,?,?)`, predecessor, scope.PrincipalID, scope.ProjectID, m.Operation, m.Target, m.Key, fingerprint, `{"id":"predecessor"}`); err != nil {
		t.Fatal(err)
	}
	prior := receiptRows(t, s.DB(), `SELECT * FROM receipts`)
	if body, found, err := s.Replay(t.Context(), scope, m); err != nil || found || body != nil {
		t.Fatal("predecessor receipt replayed as current", found, err)
	}
	calls := 0
	fn := func(ctx context.Context, tx *sql.Tx) (any, error) {
		calls++
		return map[string]string{"id": "current"}, AppendEvent(ctx, tx, scope, "origin.current", "resource", 1, nil)
	}
	body, err := s.Mutate(t.Context(), scope, m, fn)
	if err != nil || calls != 1 {
		t.Fatal("current key could not coexist", err)
	}
	if replay, err := s.Mutate(t.Context(), scope, m, fn); err != nil || string(replay) != string(body) || calls != 1 {
		t.Fatal("current exact replay repeated mutation", err)
	}
	if replay, found, err := s.Replay(t.Context(), scope, m); err != nil || !found || string(replay) != string(body) {
		t.Fatal("current explicit replay failed", err)
	}
	if got := receiptRows(t, s.DB(), `SELECT * FROM receipts WHERE instance_id='`+predecessor+`'`); !reflect.DeepEqual(got, prior) {
		t.Fatal("current mutation rewrote predecessor receipt")
	}
	if got := receiptRows(t, s.DB(), `SELECT count(*) FROM receipts`); !reflect.DeepEqual(got, [][]string{{"2"}}) {
		t.Fatal("cross-instance key histories collided")
	}
	oldScope := scope
	oldScope.instanceID = predecessor
	_, _, err = s.Replay(t.Context(), oldScope, m)
	receiptCode(t, err, "unauthorized")
	changed := m
	changed.Request = "different"
	_, _, err = s.Replay(t.Context(), scope, changed)
	receiptCode(t, err, "idempotency_conflict")
	otherWorker := scope
	otherWorker.WorkerID = ""
	_, _, err = s.Replay(t.Context(), otherWorker, m)
	receiptCode(t, err, "idempotency_conflict")
	if _, err = s.DB().Exec(`UPDATE grants SET can_write=0 WHERE project_id='project'`); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Replay(t.Context(), scope, changed)
	receiptCode(t, err, "forbidden")
	if _, err = s.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='credential'`); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Replay(t.Context(), scope, m)
	receiptCode(t, err, "unauthorized")
	if calls != 1 {
		t.Fatal("refusals invoked mutation")
	}
}

func TestReceiptOriginInvalidRestoreAndSQLFailureRollBack(t *testing.T) {
	changes := map[string]string{
		"missing source":        `DELETE FROM metadata WHERE key='restore:source_instance_id'`,
		"empty source":          `UPDATE metadata SET value='' WHERE key='restore:source_instance_id'`,
		"malformed source":      `UPDATE metadata SET value='instance_bad' WHERE key='restore:source_instance_id'`,
		"same source":           `UPDATE metadata SET value=(SELECT value FROM metadata WHERE key='instance_id') WHERE key='restore:source_instance_id'`,
		"unknown field":         `INSERT INTO metadata VALUES('restore:unknown','unrecognized')`,
		"wrong state":           `UPDATE metadata SET value='active' WHERE key='restore:state'`,
		"wrong bundle":          `UPDATE metadata SET value='backup_bad' WHERE key='restore:bundle_id'`,
		"noncanonical time":     `UPDATE metadata SET value='2026-01-01T00:00:00+00:00' WHERE key='restore:snapshot_started_at'`,
		"reversed window":       `UPDATE metadata SET value='2020-01-01T00:00:00Z' WHERE key='restore:snapshot_finished_at'`,
		"negative sequence":     `UPDATE metadata SET value='-1' WHERE key='restore:event_sequence'`,
		"noncanonical sequence": `UPDATE metadata SET value='02' WHERE key='restore:event_sequence'`,
		"empty global instance": `UPDATE metadata SET value='' WHERE key='instance_id'`,
		"malformed global key":  `UPDATE metadata SET value='bad' WHERE key='signing_key'`,
		"missing global key":    `DELETE FROM metadata WHERE key='signing_key'`,
		"copy SQL failure":      `ALTER TABLE receipts RENAME COLUMN response_json TO broken_response`,
		"newer schema":          `PRAGMA user_version=3`,
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			dir, old, _, _ := receiptLegacyFixture(t)
			receiptInactiveMetadata(t, old)
			if _, err := old.DB().Exec(change); err != nil {
				t.Fatal(err)
			}
			before := receiptRows(t, old.DB(), `SELECT type,name,sql FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name`)
			rows := receiptRows(t, old.DB(), `SELECT * FROM receipts ORDER BY key`)
			metadata := receiptRows(t, old.DB(), `SELECT * FROM metadata ORDER BY key`)
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(t.Context(), dir)
			if s != nil {
				s.Close()
				t.Fatal("invalid migration returned usable store")
			}
			code := "schema_invalid"
			if name == "copy SQL failure" {
				code = "storage_unavailable"
			}
			if name == "newer schema" {
				code = "schema_unsupported"
			}
			receiptCode(t, err, code)
			u := databaseURL(dir + "/firecontroller.sqlite")
			u.RawQuery = "mode=ro"
			db, err := sql.Open("sqlite", u.String())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if !reflect.DeepEqual(before, receiptRows(t, db, `SELECT type,name,sql FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name`)) || !reflect.DeepEqual(rows, receiptRows(t, db, `SELECT * FROM receipts ORDER BY key`)) || !reflect.DeepEqual(metadata, receiptRows(t, db, `SELECT * FROM metadata ORDER BY key`)) {
				t.Fatal("failed migration leaked SQL changes")
			}
			version := "1"
			if name == "newer schema" {
				version = "3"
			}
			if got := receiptRows(t, db, `PRAGMA user_version`); !reflect.DeepEqual(got, [][]string{{version}}) {
				t.Fatal("failed migration advanced base version")
			}
		})
	}
}
