package core

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadReauthorizesAdministrativeScopeInSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "private")
	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	token, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO projects VALUES('read-project','Read project',1,'owner/read')`); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/preview", nil)
	r.SetPathValue("project_id", "read-project")
	r.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	admin, err := store.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := store.Scope(r, "read")
	if err != nil {
		t.Fatal(err)
	}
	// Preserve ordinary project read access while withdrawing administrative
	// authority after the HTTP boundary obtained its scope.
	if _, err := store.DB().Exec(`INSERT INTO grants SELECT id,'read-project',1,0 FROM credentials WHERE principal_id=?`, admin.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE credentials SET is_admin=0 WHERE principal_id=?`, admin.PrincipalID); err != nil {
		t.Fatal(err)
	}
	called := false
	callback := func(context.Context, *sql.Tx) error { called = true; return nil }
	err = store.Read(ctx, admin, callback)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "forbidden" || called {
		t.Fatalf("administrative read survived withdrawn authority: err=%v called=%v", err, called)
	}
	if err := store.Read(ctx, ordinary, callback); err != nil || !called {
		t.Fatalf("ordinary authorized read changed: %v called=%v", err, called)
	}
	if _, err := store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE principal_id=?`, admin.PrincipalID); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []Scope{admin, ordinary} {
		called = false
		err = store.Read(ctx, scope, callback)
		if !errors.As(err, &failure) || failure.Code != "unauthorized" || called {
			t.Fatalf("revoked read reached callback: %v called=%v", err, called)
		}
	}
}
