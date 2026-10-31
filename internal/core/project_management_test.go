package core

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestProjectManagementScopeCannotEscapeOrSurviveDowngrade(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []string{
		`INSERT INTO projects VALUES('one','One',1,'owner/one'),('two','Two',2,'owner/two')`,
		`INSERT INTO credentials VALUES('manager','principal-manager','` + tokenHash("synthetic-project-manager") + `',0,0)`,
		`INSERT INTO grants(credential_id,project_id,can_read,can_write,can_manage) VALUES('manager','one',1,1,1),('manager','two',1,1,0)`,
	} {
		if _, err := s.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest("POST", "/owned-operation", nil)
	r.SetPathValue("project_id", "one")
	r.Header.Set("Authorization", "Bearer synthetic-project-manager")
	managed, err := s.AdminScope(r)
	if err != nil || !managed.Administrative() {
		t.Fatalf("own management scope: %v", err)
	}
	ordinary, err := s.Scope(r, "write")
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"two", ""} {
		r.SetPathValue("project_id", project)
		if _, err := s.AdminScope(r); err == nil {
			t.Fatal("manager gained another project's or instance administration")
		}
	}
	mutation := Mutation{Operation: "test.project.manage", Target: "settings", Key: "original-key", Request: struct{}{}}
	_, err = s.Mutate(ctx, managed, mutation, func(context.Context, *sql.Tx) (any, error) { return map[string]string{"outcome": "saved"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE grants SET can_manage=0 WHERE credential_id='manager' AND project_id='one'`); err != nil {
		t.Fatal(err)
	}
	called := false
	err = s.Read(ctx, managed, func(context.Context, *sql.Tx) error { called = true; return nil })
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "forbidden" || called {
		t.Fatal("management read survived downgrade", err)
	}
	if _, _, err := s.Replay(ctx, managed, mutation); !errors.As(err, &failure) || failure.Code != "forbidden" {
		t.Fatal("management receipt leaked after downgrade", err)
	}
	if _, err := s.Mutate(ctx, managed, mutation, func(context.Context, *sql.Tx) (any, error) { called = true; return nil, nil }); !errors.As(err, &failure) || failure.Code != "forbidden" || called {
		t.Fatal("management mutation survived downgrade", err)
	}
	if err := s.Read(ctx, ordinary, func(context.Context, *sql.Tx) error { return nil }); err != nil {
		t.Fatal("ordinary read/write grant changed", err)
	}
	copy := ordinary
	copy.ProjectID = "two"
	copy.admin = true
	if err := s.Read(ctx, copy, func(context.Context, *sql.Tx) error { return nil }); !errors.As(err, &failure) || failure.Code != "forbidden" {
		t.Fatal("copied project scope escaped management boundary", err)
	}
}

func TestProjectManagementMigrationPreservesExistingGrantsWithoutElevation(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "private")
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO projects VALUES('one','One',1,'owner/one')`,
		`INSERT INTO credentials VALUES('writer','principal-writer','` + tokenHash("synthetic-writer") + `',0,0)`,
		`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES('writer','one',1,1)`,
		`DELETE FROM metadata WHERE key='schema:project_management'`,
		`ALTER TABLE grants DROP COLUMN can_manage`,
	} {
		if _, err = s.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var read, write, manage int
	if err = s.DB().QueryRow(`SELECT can_read,can_write,can_manage FROM grants WHERE credential_id='writer'`).Scan(&read, &write, &manage); err != nil || read != 1 || write != 1 || manage != 0 {
		t.Fatal("existing writer changed or elevated", err)
	}
	if _, err = s.DB().Exec(`UPDATE grants SET can_read=0,can_manage=1 WHERE credential_id='writer'`); err == nil {
		t.Fatal("management without read/write was stored")
	}
}
