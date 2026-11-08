package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func TestDaemonSchemaMigrationTimeoutRejectsBeforeState(t *testing.T) {
	for _, value := range []string{"0", "-1s", "24h1ns", "private-invalid-duration", "9223372036854775808ns"} {
		t.Run(value, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "absent")
			args := []string{"--data-dir", dir, "--listen", "127.0.0.1:0", "--schema-migration-timeout", value,
				"--review-catalog", "must-not-load-catalog", "--github-token-file", "must-not-load-token"}
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), args, &stdout, &stderr); code != 2 || stdout.Len() != 0 || !json.Valid(stderr.Bytes()) {
				t.Fatalf("invalid migration budget continued: code%d diagnostics%s", code, stderr.String())
			}
			if strings.Contains(stderr.String(), "private-invalid-duration") {
				t.Fatal("invalid duration leaked private argument")
			}
			if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid migration budget created state", err)
			}
		})
	}
}

func TestGuardedDaemonStoreUsesMigrationBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, err := openGuardedDaemonStore(t.Context(), filepath.Join(t.TempDir(), "private"), core.WithSchemaMigrationTimeout(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		err = store.EnsureSchemaWithBackfill(t.Context(), "daemon_budget_probe", [][]string{{"SELECT 1"}}, func(ctx context.Context, _ *sql.Conn) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) != time.Minute {
				t.Fatal("daemon guard lost configured per-domain budget", time.Until(deadline))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestDaemonSchemaMigrationFailurePrecedesListen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	// The additive comparison schema is reached after earlier domain schemas.
	// Refuse a newer retained version rather than modify or discard it.
	if _, err := store.DB().Exec(`INSERT INTO metadata(key,value) VALUES('schema:review_comparison','2')`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// If startup tries to listen before finishing schemas, the occupied socket
	// produces listen_failed instead of the required schema refusal.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"--data-dir", dir, "--listen", occupied.Addr().String(), "--schema-migration-timeout", "24h"}, &stdout, &stderr)
	var diagnostic struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &diagnostic); err != nil || code != 3 || stdout.Len() != 0 || diagnostic.Error.Code != "schema_unsupported" {
		t.Fatalf("migration failure did not precede listen: code%d diagnostics%s err%v", code, stderr.String(), err)
	}
	store, err = core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version string
	if err := store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_comparison'`).Scan(&version); err != nil || version != "2" {
		t.Fatal("startup changed refused schema authority", version, err)
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE 'review_comparison_%'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("startup installed refused comparison schema", count, err)
	}
}
