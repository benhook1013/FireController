package core

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

func TestSchemaMigrationOptionRejectsBeforeFilesystemWork(t *testing.T) {
	for _, timeout := range []time.Duration{-time.Second, 0, 24*time.Hour + time.Nanosecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "absent")
			store, err := Open(t.Context(), dir, WithSchemaMigrationTimeout(timeout))
			var failure *Error
			if store != nil || !errors.As(err, &failure) || failure.Code != "configuration_invalid" {
				t.Fatalf("invalid migration budget accepted: %v", err)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid open option performed filesystem work", err)
			}
		})
	}
}

func TestSchemaMigrationBudgetDefaultAndPerDomain(t *testing.T) {
	for _, test := range []struct {
		name    string
		timeout time.Duration
		options []OpenOption
	}{
		{"default", 30 * time.Second, nil},
		{"explicit", time.Minute, []OpenOption{WithSchemaMigrationTimeout(time.Minute)}},
		{"maximum", 24 * time.Hour, []OpenOption{WithSchemaMigrationTimeout(24 * time.Hour)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, err := Open(t.Context(), filepath.Join(t.TempDir(), "private"), test.options...)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				for _, domain := range []string{"first", "second"} {
					err := s.EnsureSchemaWithBackfill(t.Context(), domain, [][]string{{"SELECT 1"}}, func(ctx context.Context, _ *sql.Conn) error {
						deadline, ok := ctx.Deadline()
						if !ok || deadline.Sub(time.Now()) != test.timeout {
							t.Fatalf("domain %s did not receive its full budget: %v", domain, deadline.Sub(time.Now()))
						}
						time.Sleep(test.timeout / 2)
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				if writeTimeout != 2*time.Second {
					t.Fatal("startup option changed the live write budget")
				}
			})
		})
	}
}

func TestSchemaMigrationBudgetAtomicTimeoutAndLongerRetry(t *testing.T) {
	// Fake time tests actual default/explicit deadlines without using machine
	// throughput or a sleep-versus-SQL-speed race as the correctness oracle.
	synctest.Test(t, func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "private")
		s, err := Open(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		if _, err := s.DB().Exec(`CREATE TABLE retained_source(id INTEGER PRIMARY KEY,body TEXT); INSERT INTO retained_source VALUES(1,'retained'),(2,'also retained')`); err != nil {
			t.Fatal(err)
		}
		migrations := [][]string{{`CREATE TABLE budget_probe(id INTEGER PRIMARY KEY,body TEXT)`}}
		calls := 0
		backfill := func(ctx context.Context, conn *sql.Conn) error {
			calls++
			if _, err := conn.ExecContext(ctx, `INSERT INTO budget_probe SELECT * FROM retained_source WHERE id=1`); err != nil {
				return err
			}
			// Verify partial work exists inside the exclusive transaction.
			var count int
			if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM budget_probe`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("partial callback did not execute: %d %v", count, err)
			}
			time.Sleep(31 * time.Second)
			_, err := conn.ExecContext(ctx, `INSERT INTO budget_probe SELECT * FROM retained_source WHERE id=2`)
			return err
		}
		err = s.EnsureSchemaWithBackfill(t.Context(), "budget_probe", migrations, backfill)
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "schema_migration_timeout" || calls != 1 {
			t.Fatalf("default deadline did not abort partial backfill: calls%d err%v", calls, err)
		}
		for _, query := range []string{`SELECT count(*) FROM sqlite_master WHERE name='budget_probe'`, `SELECT count(*) FROM metadata WHERE key='schema:budget_probe'`} {
			var count int
			if err := s.DB().QueryRow(query).Scan(&count); err != nil || count != 0 {
				t.Fatal("timeout retained partial schema or metadata", count, err)
			}
		}
		assertSource := func() {
			t.Helper()
			var count int
			if err := s.DB().QueryRow(`SELECT count(*) FROM retained_source WHERE (id=1 AND body='retained') OR (id=2 AND body='also retained')`).Scan(&count); err != nil || count != 2 {
				t.Fatal("migration changed retained source", count, err)
			}
		}
		assertSource()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(t.Context(), dir, WithSchemaMigrationTimeout(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.EnsureSchemaWithBackfill(t.Context(), "budget_probe", migrations, backfill); err != nil || calls != 2 {
			t.Fatal("explicit longer budget did not complete identical backfill", calls, err)
		}
		assertSource()
		var count int
		if err := s.DB().QueryRow(`SELECT count(*) FROM budget_probe b JOIN retained_source s ON b.id=s.id AND b.body=s.body`).Scan(&count); err != nil || count != 2 {
			t.Fatal("longer budget published incomplete backfill", count, err)
		}
		var version string
		if err := s.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:budget_probe'`).Scan(&version); err != nil || version != "1" {
			t.Fatal("longer budget did not publish version atomically", version, err)
		}
		if err := s.EnsureSchemaWithBackfill(t.Context(), "budget_probe", migrations, backfill); err != nil || calls != 2 {
			t.Fatal("repeat startup reran backfill", calls, err)
		}
	})
}
