package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func copyManagementFixture(t *testing.T, from, to string) {
	t.Helper()
	if err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0600)
	}); err != nil {
		t.Fatal(err)
	}
}
func managementRows(t *testing.T, db *sql.DB, query string) string {
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
	out := [][]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		out = append(out, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func TestProjectManagementRealParentUpgradePreservesAllRowsAndSchemas(t *testing.T) {
	parent := os.Getenv("FC_ADMIN_PARENT_FIXTURE")
	if parent == "" {
		t.Skip("requires genuine pinned-parent binary fixture; see administration proof")
	}
	dir := filepath.Join(t.TempDir(), "source")
	copyManagementFixture(t, filepath.Join(parent, "source"), dir)
	db, err := sql.Open("sqlite", filepath.Join(dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	tables, err := db.Query(`SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	tables.Close()
	before := map[string]string{}
	queries := map[string]string{}
	for _, name := range names {
		q := `SELECT * FROM "` + name + `" ORDER BY rowid`
		if name == "metadata" {
			q = `SELECT * FROM metadata WHERE key NOT IN ('schema:project_management','schema:worker_retirements') ORDER BY rowid`
		}
		queries[name] = q
		before[name] = managementRows(t, db, q)
	}
	schema := managementRows(t, db, `SELECT type,name,tbl_name,sql FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' AND name<>'grants' ORDER BY type,name`)
	if got := managementRows(t, db, `SELECT key FROM metadata WHERE key IN ('schema:project_management','schema:worker_retirements')`); got != "[]" {
		t.Fatal("fixture already has new domains", got)
	}
	db.Close()
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	queries["grants"] = `SELECT credential_id,project_id,can_read,can_write FROM grants ORDER BY rowid`
	for _, name := range names {
		if got := managementRows(t, s.DB(), queries[name]); got != before[name] {
			t.Fatalf("old rows changed in %s", name)
		}
	}
	if got := managementRows(t, s.DB(), `SELECT type,name,tbl_name,sql FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' AND name NOT IN ('grants','worker_retirements') ORDER BY type,name`); got != schema {
		t.Fatal("unrelated physical schema changed")
	}
	if got := managementRows(t, s.DB(), `SELECT can_manage FROM grants WHERE can_manage<>0`); got != "[]" {
		t.Fatal("writer silently promoted", got)
	}
	if got := managementRows(t, s.DB(), `SELECT key,value FROM metadata WHERE key IN ('schema:project_management','schema:worker_retirements') ORDER BY key`); got != `[["schema:project_management","1"],["schema:worker_retirements","1"]]` {
		t.Fatal(got)
	}
}
func TestProjectManagementPrincipalUniquenessAndExactCredentialReauthorization(t *testing.T) {
	s, scope, _ := streamFixture(t)
	ctx := t.Context()
	var principal, hash string
	if err := s.DB().QueryRow(`SELECT principal_id,token_sha256 FROM credentials WHERE id='credential'`).Scan(&principal, &hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO credentials VALUES('sibling',?, ?,1,0)`, principal, strings.Repeat("b", 64)); err == nil {
		t.Fatal("principal uniqueness lost")
	}
	for _, mutate := range []func(Scope) Scope{func(v Scope) Scope { v.PrincipalID = "other"; return v }, func(v Scope) Scope { v.tokenHash = strings.Repeat("c", 64); return v }, func(v Scope) Scope { v.instanceID = "other"; return v }} {
		called := false
		err := s.Read(ctx, mutate(scope), func(_ context.Context, _ *sql.Tx) error { called = true; return nil })
		if err == nil || called {
			t.Fatal("forged credential accepted")
		}
	}
	if _, err := s.DB().Exec(`UPDATE credentials SET token_sha256=? WHERE principal_id=?`, strings.Repeat("d", 64), principal); err != nil {
		t.Fatal(err)
	}
	called := false
	err := s.Read(ctx, scope, func(_ context.Context, _ *sql.Tx) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("old exact credential survived replacement")
	}
}

func TestProjectManagementNewerRetirementBlocksBeforeManagementMigration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`DELETE FROM metadata WHERE key='schema:project_management'`, `ALTER TABLE grants DROP COLUMN can_manage`, `UPDATE metadata SET value='2' WHERE key='schema:worker_retirements'`} {
		if _, err = s.DB().Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	if next, err := Open(t.Context(), dir); err == nil {
		next.Close()
		t.Fatal("unknown retirement accepted")
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := managementRows(t, db, `SELECT key FROM metadata WHERE key='schema:project_management'`); got != "[]" {
		t.Fatal("management migrated before newer-domain refusal")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('grants') WHERE name='can_manage'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("column changed before version refusal", count, err)
	}
}

func TestProjectManagementRealParentMigrationFailureRollsBack(t *testing.T) {
	parent := os.Getenv("FC_ADMIN_PARENT_FIXTURE")
	if parent == "" {
		t.Skip("requires genuine pinned-parent binary fixture")
	}
	dir := filepath.Join(t.TempDir(), "source")
	copyManagementFixture(t, filepath.Join(parent, "source"), dir)
	db, err := sql.Open("sqlite", filepath.Join(dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	before := managementRows(t, db, `SELECT * FROM grants ORDER BY credential_id,project_id`)
	if _, err := db.Exec(`CREATE TRIGGER reject_management_version BEFORE INSERT ON metadata WHEN NEW.key='schema:project_management' BEGIN SELECT RAISE(ABORT,'synthetic migration failure'); END`); err != nil {
		t.Fatal(err)
	}
	if next, err := Open(t.Context(), dir); err == nil {
		next.Close()
		t.Fatal("injected migration failure ignored")
	}
	if got := managementRows(t, db, `SELECT * FROM grants ORDER BY credential_id,project_id`); got != before {
		t.Fatal("failed migration changed grants")
	}
	var columns, versions int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('grants') WHERE name='can_manage'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM metadata WHERE key IN ('schema:project_management','schema:worker_retirements')`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if columns != 0 || versions != 0 {
		t.Fatal("failed migration left schema or version", columns, versions)
	}
	if _, err := db.Exec(`DROP TRIGGER reject_management_version`); err != nil {
		t.Fatal(err)
	}
	next, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal("retry after injected failure", err)
	}
	next.Close()
}
