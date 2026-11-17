package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func automationFixture(t *testing.T) (*Store, Scope) {
	t.Helper()
	s, scope, _ := streamFixture(t)
	_, err := s.DB().Exec(`UPDATE grants SET can_manage=1 WHERE credential_id='credential';
INSERT INTO projects VALUES('foreign','Foreign',2,'synthetic/foreign');
INSERT INTO workers VALUES('foreign','foreign-worker','Foreign worker');
INSERT INTO credentials VALUES('alternate','alternate-principal',?,0,0);
INSERT INTO grants(credential_id,project_id,can_read,can_write,can_manage) VALUES('alternate','project',1,1,1)`, tokenHash("synthetic-alternate-automation-token"))
	if err != nil {
		t.Fatal(err)
	}
	return s, scope.RequireAdministration()
}

func automationTx(t *testing.T, s *Store) *sql.Tx {
	t.Helper()
	tx, err := s.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func TestAuthenticatedCredentialIDRetainsExactIdentityAndScopeCeilings(t *testing.T) {
	s, scope := automationFixture(t)
	if _, err := s.DB().Exec(`INSERT INTO credentials VALUES('duplicate','principal',?,0,0)`, tokenHash("synthetic-duplicate-principal-token")); err == nil {
		t.Fatal("canonical schema permitted alternate credentials for one principal")
	}
	for _, tc := range []struct{ token, id string }{{"synthetic-core-stream-token-1234567", "credential"}, {"synthetic-alternate-automation-token", "alternate"}} {
		r := httptest.NewRequest("GET", "/", nil)
		r.SetPathValue("project_id", "project")
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("X-FC-Worker", "worker")
		authenticated, err := s.AdminScope(r)
		if err != nil {
			t.Fatal(err)
		}
		tx := automationTx(t, s)
		id, err := s.AuthenticatedCredentialID(t.Context(), tx, authenticated)
		_ = tx.Rollback()
		if err != nil || id != tc.id || strings.Contains(id, tc.token) || id == tokenHash(tc.token) {
			t.Fatal("authenticated credential was substituted or exposed secret material", err)
		}
	}
	for _, tc := range []struct {
		name, code, query string
		change            func(*Scope)
	}{
		{"unauthenticated", "unauthorized", "", func(v *Scope) { *v = Scope{ProjectID: "project", PrincipalID: "principal", WorkerID: "worker"} }},
		{"principal", "unauthorized", "", func(v *Scope) { v.PrincipalID = "another-principal" }},
		{"instance", "unauthorized", "", func(v *Scope) { v.instanceID = "another-instance" }},
		{"project", "forbidden", "", func(v *Scope) { v.ProjectID, v.WorkerID = "foreign", "foreign-worker" }},
		{"revoked-original", "unauthorized", `UPDATE credentials SET revoked=1 WHERE id='credential'`, func(*Scope) {}},
		{"demoted-original", "forbidden", `UPDATE grants SET can_manage=0 WHERE credential_id='credential'`, func(*Scope) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forged := scope
			tc.change(&forged)
			tx := automationTx(t, s)
			if tc.query != "" {
				if _, err := tx.ExecContext(t.Context(), tc.query); err != nil {
					t.Fatal(err)
				}
			}
			id, err := s.AuthenticatedCredentialID(t.Context(), tx, forged)
			browserCoreCode(t, err, tc.code)
			if id != "" {
				t.Fatal("refused scope received a credential identity")
			}
		})
	}
	// A browser read may identify its own credential, but identifying it must
	// not remove its original read-only ceiling after a later role upgrade.
	browserStore, _, _ := streamFixture(t)
	id, cookie := browserCoreSession(t, browserStore)
	browser, err := browserStore.Scope(browserCoreRequest(browserStore, id, cookie), "read")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := browserStore.DB().Exec(`UPDATE grants SET can_write=1,can_manage=1 WHERE credential_id='credential'`); err != nil {
		t.Fatal(err)
	}
	tx := automationTx(t, browserStore)
	credential, err := browserStore.AuthenticatedCredentialID(t.Context(), tx, browser)
	if err != nil || credential != "credential" || !browser.readOnly || browser.Administrative() {
		t.Fatal("identity lookup changed the attenuated browser scope", err)
	}
	_, err = browserStore.AuthenticatedCredentialID(t.Context(), tx, browser.RequireAdministration())
	browserCoreCode(t, err, "forbidden")
}

func TestProjectAutomationScopeRequiresExactCurrentManagementAndActiveWorker(t *testing.T) {
	s, _ := automationFixture(t)
	for _, tc := range []struct {
		name, project, principal, worker, credential, query, code string
	}{
		{name: "valid"},
		{name: "instance-administrator", query: `UPDATE grants SET can_manage=0 WHERE credential_id='credential'; UPDATE credentials SET is_admin=1 WHERE id='credential'`},
		{name: "missing-credential", credential: "missing", code: "forbidden"},
		{name: "wrong-principal", principal: "another-principal", code: "forbidden"},
		{name: "foreign-credential", credential: "alternate", code: "forbidden"},
		{name: "replaced-credential", query: `DELETE FROM grants WHERE credential_id='credential'; DELETE FROM credentials WHERE id='credential'; INSERT INTO credentials VALUES('replacement','principal','` + tokenHash("synthetic-replacement-token") + `',0,0); INSERT INTO grants(credential_id,project_id,can_read,can_write,can_manage) VALUES('replacement','project',1,1,1)`, code: "forbidden"},
		{name: "foreign-project", project: "foreign", worker: "foreign-worker", code: "forbidden"},
		{name: "missing-project", project: "missing", code: "not_found"},
		{name: "missing-worker", worker: "missing", code: "not_found"},
		{name: "foreign-worker", worker: "foreign-worker", code: "not_found"},
		{name: "revoked-original", query: `UPDATE credentials SET revoked=1 WHERE id='credential'`, code: "forbidden"},
		{name: "demoted-original", query: `UPDATE grants SET can_manage=0 WHERE credential_id='credential'`, code: "forbidden"},
		{name: "retired-worker", query: `INSERT INTO worker_retirements VALUES('project','worker','2026-11-01T00:00:00Z','synthetic-actor')`, code: "worker_retired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, principal, worker, credential := "project", "principal", "worker", "credential"
			if tc.project != "" {
				project = tc.project
			}
			if tc.principal != "" {
				principal = tc.principal
			}
			if tc.worker != "" {
				worker = tc.worker
			}
			if tc.credential != "" {
				credential = tc.credential
			}
			tx := automationTx(t, s)
			if tc.query != "" {
				if _, err := tx.ExecContext(t.Context(), tc.query); err != nil {
					t.Fatal(err)
				}
			}
			background, err := s.ProjectAutomationScope(t.Context(), tx, project, principal, worker, credential)
			if tc.code != "" {
				browserCoreCode(t, err, tc.code)
				if background != (Scope{}) {
					t.Fatal("refused automation returned authority")
				}
				return
			}
			if err != nil || background.ProjectID != project || background.PrincipalID != principal || background.WorkerID != worker || !background.Administrative() {
				t.Fatal("background scope lost exact management attribution", err)
			}
			id, err := s.AuthenticatedCredentialID(t.Context(), tx, background)
			if err != nil || id != credential {
				t.Fatal("background scope selected an alternate credential", err)
			}
			encoded, err := json.Marshal(background)
			if err != nil || strings.Contains(string(encoded), background.tokenHash) || strings.Contains(string(encoded), "synthetic-core-stream-token") {
				t.Fatal("background scope exposed authentication material", err)
			}
		})
	}
	for _, tuple := range [][4]string{{"", "principal", "worker", "credential"}, {"project", "", "worker", "credential"}, {"project", "principal", "", "credential"}, {"project", "principal", "worker", ""}} {
		tx := automationTx(t, s)
		_, err := s.ProjectAutomationScope(t.Context(), tx, tuple[0], tuple[1], tuple[2], tuple[3])
		_ = tx.Rollback()
		browserCoreCode(t, err, "forbidden")
	}
}

func TestProjectAutomationScopeReauthorizesAtNormalStoreBoundaries(t *testing.T) {
	s, _ := automationFixture(t)
	tx := automationTx(t, s)
	background, err := s.ProjectAutomationScope(t.Context(), tx, "project", "principal", "worker", "credential")
	_ = tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	mutation := Mutation{Operation: "test.automation", Target: "local-control", Key: "original-automation-key", Request: struct{}{}}
	if _, err := s.Mutate(t.Context(), background, mutation, func(context.Context, *sql.Tx) (any, error) { return map[string]string{"outcome": "stored"}, nil }); err != nil {
		t.Fatal("current background management scope was unusable", err)
	}
	for _, tc := range []struct{ query, reset, code string }{
		{`UPDATE grants SET can_manage=0 WHERE credential_id='credential'`, `UPDATE grants SET can_manage=1 WHERE credential_id='credential'`, "forbidden"},
		{`UPDATE credentials SET revoked=1 WHERE id='credential'`, `UPDATE credentials SET revoked=0 WHERE id='credential'`, "unauthorized"},
	} {
		if _, err := s.DB().Exec(tc.query); err != nil {
			t.Fatal(err)
		}
		called := false
		err := s.Read(t.Context(), background, func(context.Context, *sql.Tx) error { called = true; return nil })
		browserCoreCode(t, err, tc.code)
		_, err = s.Mutate(t.Context(), background, mutation, func(context.Context, *sql.Tx) (any, error) { called = true; return nil, nil })
		browserCoreCode(t, err, tc.code)
		_, _, err = s.Replay(t.Context(), background, mutation)
		browserCoreCode(t, err, tc.code)
		if called {
			t.Fatal("stale authorization entered a Store callback")
		}
		if _, err := s.DB().Exec(tc.reset); err != nil {
			t.Fatal(err)
		}
	}
}
