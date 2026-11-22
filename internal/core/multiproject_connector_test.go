package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
)

const multiConnectorToken = "synthetic-multi-project-connector-token"

func multiConnectorProject(index int) string { return fmt.Sprintf("project-%02d", index) }

func multiConnectorFixture(t *testing.T, projects int) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.DB().Exec(`INSERT INTO credentials VALUES('connector','connector-principal',?,0,0)`, tokenHash(multiConnectorToken)); err != nil {
		t.Fatal(err)
	}
	for i := range projects {
		project := multiConnectorProject(i)
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`INSERT INTO projects VALUES(?,?,?,?)`, []any{project, project, i + 1, "synthetic/" + project}},
			{`INSERT INTO workers VALUES(?,'worker','Worker'),(?,'other-worker','Other worker')`, []any{project, project}},
			{`INSERT INTO grants(credential_id,project_id,can_read,can_write,can_manage) VALUES('connector',?,1,1,1)`, []any{project}},
		} {
			if _, err := s.DB().Exec(statement.query, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	return s, dir
}

func multiConnectorBind(t *testing.T, s *Store, projects ...int) {
	t.Helper()
	tx, err := s.writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, project := range projects {
		if err := BindConnectorCredential(t.Context(), tx, "connector", multiConnectorProject(project), "worker"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func multiConnectorRequest(s *Store, project, worker string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://synthetic.test/", nil)
	r.SetPathValue("project_id", project)
	r.Header.Set("Authorization", "Bearer "+multiConnectorToken)
	r.Header.Set("X-FC-Worker", worker)
	r.Header.Set("X-FC-Instance", s.InstanceID())
	return r
}

func multiConnectorAccess(t *testing.T, s *Store) ConnectorAccess {
	t.Helper()
	access, token, err := s.AuthenticateConnector(multiConnectorRequest(s, "", ""))
	if err != nil || token != multiConnectorToken {
		t.Fatalf("connector authentication: %v", err)
	}
	return access
}

func multiConnectorExec(t *testing.T, s *Store, statement string, args ...any) {
	t.Helper()
	if _, err := s.DB().Exec(statement, args...); err != nil {
		t.Fatal(err)
	}
}

func TestMultiProjectConnectorBindingsBoundedImmutableAndActive(t *testing.T) {
	s, _ := multiConnectorFixture(t, 18)
	if MaxConnectorProjects != 16 {
		t.Fatal("connector project limit changed")
	}
	multiConnectorBind(t, s, 15)
	// SQLite REPLACE can bypass delete triggers: the insert guard must reject
	// rebinding the same project before the overall project cap is reached.
	if _, err := s.DB().Exec(`INSERT OR REPLACE INTO connector_credentials VALUES('connector','project-15','other-worker')`); err == nil {
		t.Fatal("replacement bypassed immutable project/worker binding")
	}
	// Reverse insertion order must not affect deterministic discovery order.
	for i := 14; i >= 0; i-- {
		multiConnectorBind(t, s, i)
	}
	access := multiConnectorAccess(t, s)
	if !access.MultiProject || len(access.Bindings) != 16 {
		t.Fatalf("bounded multi-project access: %+v", access)
	}
	for i, binding := range access.Bindings {
		if binding != (ConnectorBinding{ProjectID: multiConnectorProject(i), WorkerID: "worker", write: true, manage: true}) {
			t.Fatalf("discovery order or permission snapshot changed: %+v", access)
		}
	}
	for _, statement := range []string{
		`UPDATE connector_credentials SET worker_id='other-worker' WHERE project_id='project-00'`,
		`UPDATE connector_credentials SET project_id='project-16' WHERE project_id='project-00'`,
		`DELETE FROM connector_credentials WHERE project_id='project-00'`,
		`INSERT INTO connector_credentials VALUES('connector','project-00','other-worker')`,
		`INSERT INTO connector_credentials VALUES('connector','project-16','worker')`,
	} {
		if _, err := s.DB().Exec(statement); err == nil {
			t.Fatalf("immutable binding or project cap bypassed: %s", statement)
		}
	}
	// Revoking a grant neither releases the cap nor removes connector identity.
	multiConnectorExec(t, s, `UPDATE grants SET can_read=0,can_write=0,can_manage=0 WHERE credential_id='connector' AND project_id='project-00'`)
	if _, err := s.DB().Exec(`INSERT INTO connector_credentials VALUES('connector','project-16','worker')`); err == nil {
		t.Fatal("grant revocation released an immutable binding slot")
	}
	access = multiConnectorAccess(t, s)
	if !access.MultiProject || len(access.Bindings) != 15 || access.Bindings[0].ProjectID != "project-01" {
		t.Fatalf("revocation affected other projects: %+v", access)
	}

	for _, kind := range []string{"admin", "revoked", "ordinary"} {
		admin, revoked := kind == "admin", kind == "revoked"
		multiConnectorExec(t, s, `INSERT INTO credentials VALUES(?,?,?,?,?)`, kind, kind+"-principal", tokenHash("synthetic-"+kind), admin, revoked)
		multiConnectorExec(t, s, `INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES(?,'project-17',1,1)`, kind)
		if kind != "ordinary" {
			if _, err := s.DB().Exec(`INSERT INTO connector_credentials VALUES(?,'project-17','worker')`, kind); err == nil {
				t.Fatalf("%s credential received a connector binding", kind)
			}
		}
	}
	for _, statement := range []string{
		`INSERT INTO connector_credentials VALUES('ordinary','project-00','worker')`, // No grant.
		`INSERT INTO connector_credentials VALUES('ordinary','project-17','missing-worker')`,
		`INSERT INTO connector_credentials VALUES('ordinary','','worker')`,
		`INSERT INTO connector_credentials VALUES('ordinary','project-17','')`,
	} {
		if _, err := s.DB().Exec(statement); err == nil {
			t.Fatalf("binding checks or foreign keys bypassed: %s", statement)
		}
	}
	// The public transaction helper still enforces worker activity for issuance.
	multiConnectorExec(t, s, `INSERT INTO worker_retirements VALUES('project-17','worker','2026-11-01T00:00:00Z','synthetic-actor')`)
	tx, err := s.writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = BindConnectorCredential(t.Context(), tx, "ordinary", "project-17", "worker")
	_ = tx.Rollback()
	receiptCode(t, err, "worker_retired")
	if got := receiptRows(t, s.DB(), `PRAGMA foreign_key_check`); len(got) != 0 {
		t.Fatal("connector schema has invalid foreign keys", got)
	}
}

func TestMultiProjectConnectorModeUsesHistoricalBindings(t *testing.T) {
	s, _ := multiConnectorFixture(t, 4)
	multiConnectorBind(t, s, 2)
	if access := multiConnectorAccess(t, s); access.MultiProject || len(access.Bindings) != 1 || access.Bindings[0].ProjectID != "project-02" {
		t.Fatalf("single binding lost legacy mode: %+v", access)
	}
	multiConnectorBind(t, s, 3, 1, 0)
	multiConnectorExec(t, s, `UPDATE grants SET can_read=0,can_write=0,can_manage=0 WHERE credential_id='connector' AND project_id='project-00'`)
	multiConnectorExec(t, s, `INSERT INTO worker_retirements VALUES('project-01','worker','2026-11-01T00:00:00Z','synthetic-actor')`)
	multiConnectorExec(t, s, `UPDATE grants SET can_write=0,can_manage=0 WHERE credential_id='connector' AND project_id='project-03'`)
	access := multiConnectorAccess(t, s)
	want := ConnectorAccess{MultiProject: true, Bindings: []ConnectorBinding{
		{ProjectID: "project-02", WorkerID: "worker", write: true, manage: true},
		{ProjectID: "project-03", WorkerID: "worker"},
	}}
	if !reflect.DeepEqual(access, want) {
		t.Fatalf("inactive bindings or permission changes not isolated: got %+v, want %+v", access, want)
	}
	multiConnectorExec(t, s, `UPDATE grants SET can_read=0 WHERE credential_id='connector' AND project_id='project-03'`)
	if access := multiConnectorAccess(t, s); !access.MultiProject || len(access.Bindings) != 1 || access.Bindings[0].ProjectID != "project-02" {
		t.Fatalf("one survivor incorrectly reverted to legacy mode: %+v", access)
	}
	multiConnectorExec(t, s, `UPDATE grants SET can_read=0,can_write=0,can_manage=0 WHERE credential_id='connector' AND project_id='project-02'`)
	access, token, err := s.AuthenticateConnector(multiConnectorRequest(s, "", ""))
	receiptCode(t, err, "unauthorized")
	if len(access.Bindings) != 0 || access.MultiProject || token != "" {
		t.Fatal("unusable connector returned access or a token")
	}
	// Authentication still recognizes a connector, even with no active grant.
	identity, err := s.Authenticate(multiConnectorRequest(s, "", ""))
	if err != nil || !identity.connector {
		t.Fatal("connector identity depended on current grants", err)
	}
}

func TestMultiProjectConnectorNativeScopeAndBrowserIsolation(t *testing.T) {
	s, _ := multiConnectorFixture(t, 4)
	multiConnectorBind(t, s, 2, 0)
	multiConnectorExec(t, s, `INSERT INTO connector_credentials VALUES('connector','project-01','other-worker')`)
	multiConnectorExec(t, s, `UPDATE grants SET can_manage=0 WHERE credential_id='connector' AND project_id='project-01'`)
	multiConnectorExec(t, s, `UPDATE grants SET can_write=0,can_manage=0 WHERE credential_id='connector' AND project_id='project-02'`)
	for _, tc := range []struct {
		project, worker, permission, code string
	}{
		{"project-00", "worker", "read", ""},
		{"project-00", "worker", "write", ""},
		{"project-00", "worker", "admin", ""},
		{"project-01", "other-worker", "write", ""},
		{"project-01", "other-worker", "admin", "forbidden"},
		{"project-01", "worker", "read", "forbidden"},
		{"project-02", "worker", "read", ""},
		{"project-02", "worker", "write", "forbidden"},
		{"project-02", "worker", "admin", "forbidden"},
		{"project-00", "other-worker", "read", "forbidden"},
		{"project-01", "", "read", "forbidden"},
		{"project-03", "worker", "read", "forbidden"}, // Grant exists, but no binding.
		{"project-03", "worker", "admin", "forbidden"},
		{"", "", "admin", "forbidden"},
	} {
		t.Run(tc.project+"/"+tc.worker+"/"+tc.permission, func(t *testing.T) {
			r := multiConnectorRequest(s, tc.project, tc.worker)
			var scope Scope
			var err error
			if tc.permission == "admin" {
				scope, err = s.AdminScope(r)
			} else {
				scope, err = s.Scope(r, tc.permission)
			}
			if tc.code != "" {
				receiptCode(t, err, tc.code)
				return
			}
			if err != nil || scope.ProjectID != tc.project || scope.WorkerID != tc.worker || !scope.connector {
				t.Fatal("selected connector scope changed", err)
			}
			if err := s.Read(t.Context(), scope, func(context.Context, *sql.Tx) error { return nil }); err != nil {
				t.Fatal("valid scope failed transaction reauthorization", err)
			}
			forged := scope
			forged.WorkerID = "other-worker"
			if forged.WorkerID == scope.WorkerID {
				forged.WorkerID = "worker"
			}
			forged.connector = false // Cached attribution must not grant authority.
			called := false
			err = s.Read(t.Context(), forged, func(context.Context, *sql.Tx) error { called = true; return nil })
			receiptCode(t, err, "forbidden")
			if called {
				t.Fatal("forged scope invoked read callback")
			}
			forged = scope
			forged.ProjectID = "project-03"
			err = s.Read(t.Context(), forged, func(context.Context, *sql.Tx) error { t.Fatal("unbound project invoked read callback"); return nil })
			receiptCode(t, err, "forbidden")
		})
	}
	for i := range 4 {
		worker := "worker"
		if i == 1 {
			worker = "other-worker"
		}
		r := multiConnectorRequest(s, multiConnectorProject(i), worker)
		_, cookie, err := s.CreateBrowserSession(r, "0123456789abcdef0123456789abcdef")
		receiptCode(t, err, "forbidden")
		if cookie != nil {
			t.Fatal("connector received a browser cookie")
		}
	}
	// Revoking one project cannot turn the same credential into an unbound
	// native principal on an unbound grant in a different project.
	multiConnectorExec(t, s, `UPDATE grants SET can_read=0,can_write=0,can_manage=0 WHERE credential_id='connector' AND project_id='project-00'`)
	_, err := s.Scope(multiConnectorRequest(s, "project-03", "other-worker"), "read")
	receiptCode(t, err, "forbidden")
	if _, err := s.Scope(multiConnectorRequest(s, "project-01", "other-worker"), "write"); err != nil {
		t.Fatal("another project's revocation removed valid native access", err)
	}
}

func TestMultiProjectConnectorReauthorizationIsSelectedAndPermissionPinned(t *testing.T) {
	for _, tc := range []struct{ name, change string }{
		{"read revoked", `UPDATE grants SET can_read=0,can_write=0,can_manage=0 WHERE credential_id='connector' AND project_id='project-00'`},
		{"write removed", `UPDATE grants SET can_write=0,can_manage=0 WHERE credential_id='connector' AND project_id='project-00'`},
		{"management removed", `UPDATE grants SET can_manage=0 WHERE credential_id='connector' AND project_id='project-00'`},
		{"worker retired", `INSERT INTO worker_retirements VALUES('project-00','worker','2026-11-01T00:00:00Z','synthetic-actor')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := multiConnectorFixture(t, 2)
			multiConnectorBind(t, s, 1, 0)
			original := multiConnectorAccess(t, s)
			multiConnectorExec(t, s, tc.change)
			receiptCode(t, s.ReauthorizeConnector(t.Context(), multiConnectorToken, original.Bindings[0]), "unauthorized")
			if err := s.ReauthorizeConnector(t.Context(), multiConnectorToken, original.Bindings[1]); err != nil {
				t.Fatal("unrelated project change invalidated selected response", err)
			}
			current := multiConnectorAccess(t, s)
			if !current.MultiProject || len(current.Bindings) == 0 {
				t.Fatal("remaining project lost its multi-project identity")
			}
			forged := original.Bindings[1]
			forged.WorkerID = "other-worker"
			receiptCode(t, s.ReauthorizeConnector(t.Context(), multiConnectorToken, forged), "unauthorized")
			multiConnectorExec(t, s, `UPDATE credentials SET revoked=1 WHERE id='connector'`)
			receiptCode(t, s.ReauthorizeConnector(t.Context(), multiConnectorToken, original.Bindings[1]), "unauthorized")
			_, _, err := s.AuthenticateConnector(multiConnectorRequest(s, "", ""))
			receiptCode(t, err, "unauthorized")
		})
	}
	// A read-only snapshot can remain usable when a later write upgrade is
	// removed again; reauthorization pins original permissions, not later ones.
	s, _ := multiConnectorFixture(t, 2)
	multiConnectorBind(t, s, 0, 1)
	multiConnectorExec(t, s, `UPDATE grants SET can_write=0,can_manage=0 WHERE credential_id='connector' AND project_id='project-00'`)
	readOnly := multiConnectorAccess(t, s).Bindings[0]
	multiConnectorExec(t, s, `UPDATE grants SET can_write=1 WHERE credential_id='connector' AND project_id='project-00'`)
	if err := s.ReauthorizeConnector(t.Context(), multiConnectorToken, readOnly); err != nil {
		t.Fatal("permission upgrade invalidated a read-only result", err)
	}
	multiConnectorExec(t, s, `UPDATE grants SET can_write=0 WHERE credential_id='connector' AND project_id='project-00'`)
	if err := s.ReauthorizeConnector(t.Context(), multiConnectorToken, readOnly); err != nil {
		t.Fatal("original read-only permission snapshot was not retained", err)
	}
}

func TestMultiProjectConnectorManagementReplayAndRetainedAdministrator(t *testing.T) {
	s, _ := multiConnectorFixture(t, 3)
	multiConnectorBind(t, s, 1, 0)
	var scopes []Scope
	mutation := Mutation{Operation: "test.connector.manage", Target: "settings", Key: "original-key", Request: "original"}
	for i := range 2 {
		scope, err := s.AdminScope(multiConnectorRequest(s, multiConnectorProject(i), "worker"))
		if err != nil {
			t.Fatal(err)
		}
		scopes = append(scopes, scope)
		if _, err := s.Mutate(t.Context(), scope, mutation, func(context.Context, *sql.Tx) (any, error) {
			return map[string]string{"project_id": scope.ProjectID}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	checkRetained := func(project, worker, code string) {
		t.Helper()
		tx, err := s.writer.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		err = CheckProjectAdministrator(t.Context(), tx, project, "connector-principal", worker)
		_ = tx.Rollback()
		if code == "" {
			if err != nil {
				t.Fatal("retained selected project manager was rejected", err)
			}
		} else {
			receiptCode(t, err, code)
		}
	}
	checkRetained("project-00", "worker", "")
	checkRetained("project-01", "worker", "")
	checkRetained("project-01", "other-worker", "forbidden")
	checkRetained("project-02", "worker", "forbidden")
	multiConnectorExec(t, s, `UPDATE grants SET can_manage=0 WHERE credential_id='connector' AND project_id='project-00'`)
	checkRetained("project-00", "worker", "forbidden")
	checkRetained("project-01", "worker", "")
	err := s.Read(t.Context(), scopes[0], func(context.Context, *sql.Tx) error {
		t.Fatal("downgraded management read invoked callback")
		return nil
	})
	receiptCode(t, err, "forbidden")
	_, _, err = s.Replay(t.Context(), scopes[0], mutation)
	receiptCode(t, err, "forbidden")
	_, err = s.Mutate(t.Context(), scopes[0], mutation, func(context.Context, *sql.Tx) (any, error) {
		t.Fatal("downgraded replay invoked mutation")
		return nil, nil
	})
	receiptCode(t, err, "forbidden")
	if _, found, err := s.Replay(t.Context(), scopes[1], mutation); err != nil || !found {
		t.Fatal("another project's downgrade invalidated management replay", err)
	}
	multiConnectorExec(t, s, `INSERT INTO worker_retirements VALUES('project-01','worker','2026-11-01T00:00:00Z','synthetic-actor')`)
	checkRetained("project-01", "worker", "forbidden")
	err = s.Read(t.Context(), scopes[1], func(context.Context, *sql.Tx) error { t.Fatal("retired manager invoked callback"); return nil })
	receiptCode(t, err, "unauthorized")
}

func TestMultiProjectConnectorRejectsNonConnectorAndMalformedBearers(t *testing.T) {
	s, _ := multiConnectorFixture(t, 1)
	multiConnectorBind(t, s, 0)
	multiConnectorExec(t, s, `INSERT INTO credentials VALUES('ordinary','ordinary-principal',?,0,0),('admin','admin-principal',?,1,0)`, tokenHash("synthetic-ordinary-token"), tokenHash("synthetic-admin-token"))
	multiConnectorExec(t, s, `INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES('ordinary','project-00',1,1)`)
	for _, headers := range [][]string{
		nil, {"Bearer missing-token"}, {"bearer " + multiConnectorToken},
		{"Bearer " + multiConnectorToken, "Bearer " + multiConnectorToken},
		{"Bearer " + multiConnectorToken + ","}, {"Bearer " + multiConnectorToken + " "},
		{"Bearer synthetic-ordinary-token"}, {"Bearer synthetic-admin-token"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		for _, header := range headers {
			r.Header.Add("Authorization", header)
		}
		access, token, err := s.AuthenticateConnector(r)
		receiptCode(t, err, "unauthorized")
		var failure *Error
		if !errors.As(err, &failure) || !failure.bearerChallenge || token != "" || len(access.Bindings) != 0 {
			t.Fatal("invalid connector did not fail closed with a bearer challenge")
		}
	}
	// Native ordinary and instance-administrator behavior remains unchanged.
	r := multiConnectorRequest(s, "project-00", "other-worker")
	r.Header.Set("Authorization", "Bearer synthetic-ordinary-token")
	if _, err := s.Scope(r, "write"); err != nil {
		t.Fatal("ordinary credential became worker-bound", err)
	}
	r.Header.Set("Authorization", "Bearer synthetic-admin-token")
	r.SetPathValue("project_id", "")
	r.Header.Del("X-FC-Worker")
	if _, err := s.AdminScope(r); err != nil {
		t.Fatal("instance administrator lost native access", err)
	}
}

// Reconstruct the exported canonical v4 objects, rather than relabel a v5 table.
func multiConnectorV4Fixture(t *testing.T) (*Store, string) {
	t.Helper()
	s, dir := multiConnectorFixture(t, 2)
	for _, statement := range []string{
		"DROP TRIGGER connector_binding_update", "DROP TRIGGER connector_binding_delete", "DROP TRIGGER connector_binding_insert", "DROP TABLE connector_credentials",
	} {
		multiConnectorExec(t, s, statement)
	}
	for _, statement := range ConnectorCredentialV4SchemaSQL() {
		multiConnectorExec(t, s, statement)
	}
	multiConnectorBind(t, s, 0)
	if _, err := s.DB().Exec(`INSERT INTO connector_credentials VALUES('connector','project-01','worker')`); err == nil {
		t.Fatal("v4 fixture did not enforce one project per credential")
	}
	multiConnectorExec(t, s, `INSERT INTO credentials VALUES('revoked','revoked-principal',?,0,0)`, tokenHash("synthetic-revoked-connector-token"))
	multiConnectorExec(t, s, `INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES('revoked','project-01',1,1)`)
	multiConnectorExec(t, s, `INSERT INTO connector_credentials VALUES('revoked','project-01','worker')`)
	multiConnectorExec(t, s, `UPDATE credentials SET revoked=1 WHERE id='revoked'`)
	multiConnectorExec(t, s, `PRAGMA user_version=4`)
	return s, dir
}

func TestMultiProjectConnectorV4MigrationPreservesHistoryAndCanonicalSchema(t *testing.T) {
	old, dir := multiConnectorV4Fixture(t)
	queries := []string{
		`SELECT * FROM connector_credentials ORDER BY credential_id,project_id`,
		`SELECT * FROM credentials ORDER BY id`, `SELECT * FROM grants ORDER BY credential_id,project_id`,
		`SELECT * FROM projects ORDER BY id`, `SELECT * FROM workers ORDER BY project_id,id`,
		`SELECT * FROM metadata ORDER BY key`, `SELECT * FROM events ORDER BY sequence`, `SELECT * FROM receipts`,
		`SELECT type,name,sql FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' AND name NOT LIKE 'connector_%' AND name<>'workers' ORDER BY type,name`,
	}
	before := make([][][]string, len(queries))
	for i, query := range queries {
		before[i] = receiptRows(t, old.DB(), query)
	}
	instance := old.InstanceID()
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.InstanceID() != instance {
		t.Fatal("migration changed instance identity")
	}
	for i, query := range queries {
		if !reflect.DeepEqual(receiptRows(t, s.DB(), query), before[i]) {
			t.Fatalf("migration changed retained history or unrelated schema: %s", query)
		}
	}
	if got := receiptRows(t, s.DB(), `PRAGMA user_version`); !reflect.DeepEqual(got, [][]string{{"6"}}) {
		t.Fatal("v4 migration did not advance to the current base version", got)
	}
	for i, name := range []string{"connector_credentials", "connector_binding_update", "connector_binding_delete", "connector_binding_insert"} {
		var actual string
		if err := s.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE name=?`, name).Scan(&actual); err != nil || actual != ConnectorCredentialSchemaSQL()[i] {
			t.Fatalf("migrated object is not canonical v5: %s: %v", name, err)
		}
	}
	if got := receiptRows(t, s.DB(), `SELECT name FROM sqlite_schema WHERE name='connector_credentials_v4'`); len(got) != 0 {
		t.Fatal("migration retained a predecessor table", got)
	}
	if access := multiConnectorAccess(t, s); access.MultiProject || len(access.Bindings) != 1 {
		t.Fatal("migrated single binding was not legacy mode")
	}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer synthetic-revoked-connector-token")
	_, _, err = s.AuthenticateConnector(r)
	receiptCode(t, err, "unauthorized")
	if _, err := s.DB().Exec(`INSERT INTO connector_credentials VALUES('revoked','project-00','worker')`); err == nil {
		t.Fatal("new insert guard permitted a revoked credential")
	}
	multiConnectorBind(t, s, 1)
	if access := multiConnectorAccess(t, s); !access.MultiProject || len(access.Bindings) != 2 {
		t.Fatal("migrated schema did not permit a second project")
	}
	if got := receiptRows(t, s.DB(), `PRAGMA foreign_key_check`); len(got) != 0 {
		t.Fatal("migration broke connector foreign keys", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dir)
	if err != nil {
		t.Fatal("v5 reopen failed", err)
	}
	defer s.Close()
	if access := multiConnectorAccess(t, s); !access.MultiProject || len(access.Bindings) != 2 {
		t.Fatal("v5 reopen changed multi-project mode")
	}
}

func TestMultiProjectConnectorV4MigrationFailureIsAtomic(t *testing.T) {
	for _, change := range []string{
		`CREATE TABLE connector_credentials_v4(conflict TEXT)`,
		`ALTER TABLE connector_credentials RENAME COLUMN worker_id TO broken_worker`,
	} {
		t.Run(change, func(t *testing.T) {
			old, dir := multiConnectorV4Fixture(t)
			multiConnectorExec(t, old, change)
			queries := []string{
				`SELECT type,name,sql FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name`,
				`SELECT * FROM connector_credentials ORDER BY credential_id`,
				`SELECT * FROM credentials ORDER BY id`, `SELECT * FROM grants ORDER BY credential_id,project_id`,
				`SELECT * FROM metadata ORDER BY key`, `PRAGMA user_version`,
			}
			before := make([][][]string, len(queries))
			for i, query := range queries {
				before[i] = receiptRows(t, old.DB(), query)
			}
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(t.Context(), dir)
			if s != nil {
				s.Close()
				t.Fatal("failed migration returned a usable store")
			}
			receiptCode(t, err, "storage_unavailable")
			u := databaseURL(filepath.Join(dir, "firecontroller.sqlite"))
			u.RawQuery = "mode=ro"
			db, err := sql.Open("sqlite", u.String())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for i, query := range queries {
				if got := receiptRows(t, db, query); !reflect.DeepEqual(got, before[i]) {
					t.Fatalf("failed migration leaked partial changes: %s", query)
				}
			}
		})
	}
}

func TestMultiProjectConnectorV3UpgradesDirectlyToCompositeBindings(t *testing.T) {
	old, dir := multiConnectorFixture(t, 2)
	multiConnectorExec(t, old, `DROP TABLE connector_credentials; PRAGMA user_version=3`)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	multiConnectorBind(t, s, 1, 0)
	if access := multiConnectorAccess(t, s); !access.MultiProject || len(access.Bindings) != 2 {
		t.Fatal("pre-v4 schema did not upgrade directly to v5")
	}
}
