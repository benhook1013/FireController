package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func schemaBackfillStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func schemaBackfillMigrations() [][]string {
	return [][]string{
		{`CREATE TABLE backfill_probe(id INTEGER PRIMARY KEY,stamp TEXT NOT NULL)`, `INSERT INTO backfill_probe VALUES(1,'2026-10-01T03:04:05.123456789+13:00')`},
		{`ALTER TABLE backfill_probe ADD COLUMN seconds INTEGER`, `ALTER TABLE backfill_probe ADD COLUMN nanos INTEGER`, `INSERT INTO backfill_probe(id,stamp) VALUES(2,'2026-09-30T14:04:05.000000001Z')`},
	}
}

func TestSchemaBackfillFreshAndUpgradeCommitOnce(t *testing.T) {
	for _, old := range []bool{false, true} {
		t.Run(fmt.Sprint("old-", old), func(t *testing.T) {
			s := schemaBackfillStore(t)
			migrations := schemaBackfillMigrations()
			if old {
				if err := s.EnsureSchema(t.Context(), "backfill_probe", migrations[0]); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			backfill := func(ctx context.Context, conn *sql.Conn) error {
				calls++
				var version string
				if err := conn.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:backfill_probe'`).Scan(&version); err != nil || version != "2" {
					return fmt.Errorf("backfill did not see all migration metadata: %q %v", version, err)
				}
				for id := 1; id <= 2; id++ {
					var stamp string
					if err := conn.QueryRowContext(ctx, `SELECT stamp FROM backfill_probe WHERE id=?`, id).Scan(&stamp); err != nil {
						return err
					}
					parsed, err := time.Parse(time.RFC3339Nano, stamp)
					if err != nil {
						return err
					}
					if _, err := conn.ExecContext(ctx, `UPDATE backfill_probe SET seconds=?,nanos=? WHERE id=?`, parsed.Unix(), parsed.Nanosecond(), id); err != nil {
						return err
					}
				}
				return nil
			}
			if err := s.EnsureSchemaWithBackfill(t.Context(), "backfill_probe", migrations, backfill); err != nil {
				t.Fatal(err)
			}
			if err := s.EnsureSchemaWithBackfill(t.Context(), "backfill_probe", migrations, backfill); err != nil || calls != 1 {
				t.Fatalf("already-current startup invoked backfill: calls%d err%v", calls, err)
			}
			for id, nanos := range []int64{123456789, 1} {
				var stamp string
				var seconds, gotNanos int64
				if err := s.DB().QueryRow(`SELECT stamp,seconds,nanos FROM backfill_probe WHERE id=?`, id+1).Scan(&stamp, &seconds, &gotNanos); err != nil {
					t.Fatal(err)
				}
				parsed, err := time.Parse(time.RFC3339Nano, stamp)
				if err != nil || seconds != parsed.Unix() || gotNanos != nanos {
					t.Fatalf("exact timestamp backfill lost source precision: %q seconds%d nanos%d err%v", stamp, seconds, gotNanos, err)
				}
			}
		})
	}
}

func assertSchemaBackfillRolledBack(t *testing.T, s *Store, old bool) {
	t.Helper()
	var version string
	err := s.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:backfill_probe'`).Scan(&version)
	if old && (err != nil || version != "1") || !old && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed backfill changed version: old%t version%q err%v", old, version, err)
	}
	var columns, callbackTables int
	if err := s.DB().QueryRow(`SELECT count(*) FROM pragma_table_info('backfill_probe')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	wantColumns := 0
	if old {
		wantColumns = 2
		var stamp string
		if err := s.DB().QueryRow(`SELECT stamp FROM backfill_probe WHERE id=1`).Scan(&stamp); err != nil || stamp != "2026-10-01T03:04:05.123456789+13:00" {
			t.Fatalf("failed callback changed retained data: %q %v", stamp, err)
		}
	}
	if err := s.DB().QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='callback_probe'`).Scan(&callbackTables); err != nil || callbackTables != 0 || columns != wantColumns {
		t.Fatalf("failed callback leaked schema: callback%d columns%d err%v", callbackTables, columns, err)
	}
}

func TestSchemaBackfillErrorRollsBackAllGroupsAndRetry(t *testing.T) {
	for _, old := range []bool{false, true} {
		t.Run(fmt.Sprint("old-", old), func(t *testing.T) {
			s := schemaBackfillStore(t)
			migrations := schemaBackfillMigrations()
			if old {
				if err := s.EnsureSchema(t.Context(), "backfill_probe", migrations[0]); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			backfill := func(ctx context.Context, conn *sql.Conn) error {
				calls++
				if _, err := conn.ExecContext(ctx, `CREATE TABLE callback_probe(value TEXT)`); err != nil {
					return err
				}
				if _, err := conn.ExecContext(ctx, `UPDATE backfill_probe SET stamp='callback-changed'`); err != nil {
					return err
				}
				if calls == 1 {
					return errors.New("private backfill failure")
				}
				return nil
			}
			err := s.EnsureSchemaWithBackfill(t.Context(), "backfill_probe", migrations, backfill)
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "storage_unavailable" || strings.Contains(err.Error(), "private") {
				t.Fatalf("callback error was not mapped safely: %v", err)
			}
			assertSchemaBackfillRolledBack(t, s, old)
			if err := s.EnsureSchemaWithBackfill(t.Context(), "backfill_probe", migrations, backfill); err != nil || calls != 2 {
				t.Fatalf("atomic retry failed: calls%d err%v", calls, err)
			}
			var version, stamp string
			if err := s.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:backfill_probe'`).Scan(&version); err != nil || version != "2" {
				t.Fatalf("retry version: %q %v", version, err)
			}
			if err := s.DB().QueryRow(`SELECT stamp FROM backfill_probe WHERE id=1`).Scan(&stamp); err != nil || stamp != "callback-changed" {
				t.Fatalf("retry callback data: %q %v", stamp, err)
			}
		})
	}
}

func TestSchemaBackfillCancellationRollsBackBeforeCommit(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint("deadline-", deadline), func(t *testing.T) {
			s := schemaBackfillStore(t)
			ctx, cancel := context.WithCancel(t.Context())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
			}
			defer cancel()
			calls := 0
			err := s.EnsureSchemaWithBackfill(ctx, "backfill_probe", schemaBackfillMigrations(), func(ctx context.Context, conn *sql.Conn) error {
				calls++
				if _, err := conn.ExecContext(ctx, `CREATE TABLE callback_probe(value TEXT)`); err != nil {
					return err
				}
				if !deadline {
					cancel()
				}
				<-ctx.Done()
				// Even a callback that forgets to return cancellation cannot commit.
				return nil
			})
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "overloaded" || calls != 1 || ctx.Err() == nil {
				t.Fatalf("parent cancellation did not abort callback transaction: calls%d err%v", calls, err)
			}
			assertSchemaBackfillRolledBack(t, s, false)
			if err := s.EnsureSchemaWithBackfill(t.Context(), "backfill_probe", schemaBackfillMigrations(), nil); err != nil {
				t.Fatalf("canceled callback retained admission / transaction: %v", err)
			}
		})
	}
}
