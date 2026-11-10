package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func projectManagerGrant(t *testing.T, f *fixture, project, token string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	v := require(t, request(f.handler, "POST", "/api/v1/projects/"+project+"/credentials", f.admin, core.NewID("key"), "", `{"token_sha256":"`+hex.EncodeToString(sum[:])+`","permissions":["manage"]}`), http.StatusOK)
	return v["id"].(string)
}

func TestProjectManagerDelegatesOnlyItsProjectAndCannotCreateInstanceAuthority(t *testing.T) {
	f := setup(t)
	one, two := f.project(t, "owner/one"), f.project(t, "owner/two")
	manager := "synthetic-project-manager-token"
	id := projectManagerGrant(t, f, one, manager)
	var read, write, manage, admin int
	if err := f.store.DB().QueryRow(`SELECT g.can_read,g.can_write,g.can_manage,c.is_admin FROM grants g JOIN credentials c ON c.id=g.credential_id WHERE g.credential_id=? AND g.project_id=?`, id, one).Scan(&read, &write, &manage, &admin); err != nil || read != 1 || write != 1 || manage != 1 || admin != 0 {
		t.Fatal("management did not imply project read/write only", err)
	}
	sum := sha256.Sum256([]byte("synthetic-delegated-manager"))
	body := `{"token_sha256":"` + hex.EncodeToString(sum[:]) + `","permissions":["read","write","manage"]}`
	v := require(t, request(f.handler, "POST", "/api/v1/projects/"+one+"/credentials", manager, "delegate-same-project", "", body), 200)
	child := v["id"].(string)
	if err := f.store.DB().QueryRow(`SELECT is_admin FROM credentials WHERE id=?`, child).Scan(&admin); err != nil || admin != 0 {
		t.Fatal("delegation created global administration", err)
	}
	require(t, request(f.handler, "POST", "/api/v1/projects/"+two+"/credentials", manager, "not-other-project", "", body), 403)
	require(t, request(f.handler, "POST", "/api/v1/projects", manager, "not-global-create", "", `{"name":"Other","repository":"owner/three"}`), 403)
	require(t, request(f.handler, "GET", "/api/v1/projects", manager, "", "", ""), 403)
	writer := "synthetic-ordinary-project-writer"
	f.grant(t, one, writer, true)
	require(t, request(f.handler, "POST", "/api/v1/projects/"+one+"/credentials", writer, "writer-cannot-delegate", "", body), 403)
	// Same-key replay must be reauthorized after management is withdrawn.
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_manage=0 WHERE credential_id=?`, id); err != nil {
		t.Fatal(err)
	}
	require(t, request(f.handler, "POST", "/api/v1/projects/"+one+"/credentials", manager, "delegate-same-project", "", body), 403)
}

func TestProjectManagerRevocationPreservesAnotherProjectsSharedGrant(t *testing.T) {
	f := setup(t)
	one, two := f.project(t, "owner/one"), f.project(t, "owner/two")
	manager := "synthetic-project-manager-token"
	projectManagerGrant(t, f, one, manager)
	shared := "synthetic-shared-project-token"
	id := f.grant(t, one, shared, true)
	if _, err := f.store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES(?,?,1,1)`, id, two); err != nil {
		t.Fatal(err)
	}
	before := count(t, f.store, "events")
	path := "/api/v1/projects/" + one + "/credentials/" + id
	require(t, request(f.handler, "DELETE", path, manager, "revoke-own-project", "", ""), 200)
	require(t, request(f.handler, "GET", "/api/v1/projects/"+one, shared, "", "", ""), 403)
	require(t, request(f.handler, "GET", "/api/v1/projects/"+two, shared, "", "", ""), 200)
	var revoked int
	if err := f.store.DB().QueryRow(`SELECT revoked FROM credentials WHERE id=?`, id).Scan(&revoked); err != nil || revoked != 0 {
		t.Fatal("revocation affected other project", err)
	}
	if count(t, f.store, "events") != before+1 {
		t.Fatal("revocation audit missing")
	}
	require(t, request(f.handler, "DELETE", path, manager, "another-key-same-revocation", "", ""), 200)
	if count(t, f.store, "events") != before+1 {
		t.Fatal("already withdrawn grant produced another event")
	}
	foreign := "synthetic-foreign-project-token"
	foreignID := f.grant(t, two, foreign, true)
	require(t, request(f.handler, "DELETE", "/api/v1/projects/"+one+"/credentials/"+foreignID, manager, "not-foreign-credential", "", ""), 404)
	require(t, request(f.handler, "GET", "/api/v1/projects/"+two, foreign, "", "", ""), 200)
}

func TestProjectManagerBrowserAppearanceCeilingAndDowngrade(t *testing.T) {
	f := browserActionsFixture(t)
	p := f.project(t, "owner/one")
	worker := f.worker(t, p, "Owner")
	manager := "synthetic-project-manager-token"
	credential := projectManagerGrant(t, f, p, manager)
	session := strings.Repeat("e", 32)
	cookie := browserActionsLogin(t, f, p, manager, session, true)
	body := `{"expected_revision":0,"presentation":{"name":"Owned project"}}`
	r := browserActionRequest(f, "PUT", "/api/v1/projects/"+p+"/presentation", session, cookie, worker, "owner-appearance", body)
	require(t, serveBrowserAction(f, r), 200)
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_manage=0 WHERE credential_id=?`, credential); err != nil {
		t.Fatal(err)
	}
	r = browserActionRequest(f, "PUT", "/api/v1/projects/"+p+"/presentation", session, cookie, worker, "owner-appearance", body)
	require(t, serveBrowserAction(f, r), 403)
	// An original writer cookie must not acquire appearance authority later.
	id := strings.Repeat("f", 32)
	plain := browserActionsLogin(t, f, p, manager, id, false)
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_manage=1 WHERE credential_id=?`, credential); err != nil {
		t.Fatal(err)
	}
	r = browserActionRequest(f, "PUT", "/api/v1/projects/"+p+"/presentation", id, plain, worker, "no-cookie-elevation", body)
	require(t, serveBrowserAction(f, r), 403)
}

func TestProjectManagerChangesExistingGrantWithExactPermissionCAS(t *testing.T) {
	f := setup(t)
	one, two := f.project(t, "owner/one"), f.project(t, "owner/two")
	manager := "synthetic-project-manager-token"
	projectManagerGrant(t, f, one, manager)
	token := "synthetic-retained-worker-token"
	id := f.grant(t, one, token, true)
	var principal, hash string
	if err := f.store.DB().QueryRow(`SELECT principal_id,token_sha256 FROM credentials WHERE id=?`, id).Scan(&principal, &hash); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/projects/" + one + "/credentials/" + id
	body := `{"expected_permissions":["write","read"],"permissions":["manage"]}`
	before := count(t, f.store, "events")
	v := require(t, request(f.handler, "PUT", path, manager, "retain-identity-promote", "", body), 200)
	if v["outcome"] != "updated" {
		t.Fatal("grant was not updated")
	}
	var afterPrincipal, afterHash string
	var admin int
	if err := f.store.DB().QueryRow(`SELECT principal_id,token_sha256,is_admin FROM credentials WHERE id=?`, id).Scan(&afterPrincipal, &afterHash, &admin); err != nil || principal != afterPrincipal || hash != afterHash || admin != 0 {
		t.Fatal("role edit replaced or globally elevated identity", err)
	}
	if count(t, f.store, "events") != before+1 {
		t.Fatal("management edit was not audited")
	}
	// The retained credential now administers this project's grants itself.
	sum := sha256.Sum256([]byte("synthetic-delegation-after-promotion"))
	require(t, request(f.handler, "POST", "/api/v1/projects/"+one+"/credentials", token, "promoted-client-own-project", "", `{"token_sha256":"`+hex.EncodeToString(sum[:])+`","permissions":["read"]}`), 200)
	require(t, request(f.handler, "GET", "/api/v1/projects", token, "", "", ""), 403)
	require(t, request(f.handler, "PUT", path, manager, "stale-permission-guard", "", body), 409)
	stale := request(f.handler, "PUT", path, manager, "another-stale-permission-guard", "", body)
	if !strings.Contains(stale.Body.String(), "current_permissions") || strings.Contains(stale.Body.String(), hash) {
		t.Fatal("stale conflict omitted safe permission detail or leaked credential hash")
	}
	// Equivalent normalized requests replay without changing the grant or its audit.
	require(t, request(f.handler, "PUT", path, manager, "retain-identity-promote", "", `{"expected_permissions":["read","write"],"permissions":["read","write","manage"]}`), 200)
	audits := count(t, f.store, "events")
	unchanged := require(t, request(f.handler, "PUT", path, manager, "already-same-role", "", `{"expected_permissions":["manage"],"permissions":["read","write","manage"]}`), 200)
	if unchanged["outcome"] != "unchanged" || count(t, f.store, "events") != audits {
		t.Fatal("equivalent permission set manufactured a change")
	}
	require(t, request(f.handler, "PUT", path, manager, "retain-identity-promote", "", `{"expected_permissions":["manage"],"permissions":["read"]}`), 409)
	require(t, request(f.handler, "PUT", "/api/v1/projects/"+two+"/credentials/"+id, manager, "not-other-project-grant", "", body), 403)
	foreign := f.grant(t, two, "synthetic-unrelated-credential", true)
	w := request(f.handler, "PUT", "/api/v1/projects/"+one+"/credentials/"+foreign, manager, "not-foreign-target", "", body)
	require(t, w, 404)
	if strings.Contains(w.Body.String(), "current_permissions") {
		t.Fatal("foreign target leaked permissions")
	}
	var globalID string
	if err := f.store.DB().QueryRow(`SELECT id FROM credentials WHERE is_admin=1`).Scan(&globalID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES(?,?,1,1)`, globalID, one); err != nil {
		t.Fatal(err)
	}
	require(t, request(f.handler, "PUT", "/api/v1/projects/"+one+"/credentials/"+globalID, manager, "not-global-administrator-target", "", body), 404)
	for i, bad := range []string{`{}`, `{"expected_permissions":null,"permissions":["manage"]}`, `{"expected_permissions":["read","read"],"permissions":["manage"]}`, `{"expected_permissions":["read"],"permissions":["admin"]}`, `{"expected_permissions":["read"],"permissions":[]}`, `{"expected_permissions":["read"],"permissions":["write"]}`} {
		require(t, request(f.handler, "PUT", path, manager, "bad-role-"+string(rune('a'+i)), "", bad), 400)
	}
}

func TestProjectManagerPermissionEditRollbackAndReplayAuthorization(t *testing.T) {
	f := setup(t)
	p := f.project(t, "owner/one")
	manager := "synthetic-project-manager-token"
	ownerID := projectManagerGrant(t, f, p, manager)
	id := f.grant(t, p, "synthetic-managed-credential", false)
	path := "/api/v1/projects/" + p + "/credentials/" + id
	body := `{"expected_permissions":["read"],"permissions":["manage"]}`
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_management_event BEFORE INSERT ON events WHEN NEW.event_type='credentials.permissions_changed' BEGIN SELECT RAISE(ABORT,'synthetic event failure'); END`); err != nil {
		t.Fatal(err)
	}
	require(t, request(f.handler, "PUT", path, manager, "retry-role-after-rollback", "", body), 500)
	var write, manage int
	if err := f.store.DB().QueryRow(`SELECT can_write,can_manage FROM grants WHERE credential_id=?`, id).Scan(&write, &manage); err != nil || write != 0 || manage != 0 {
		t.Fatal("failed audit left authority changed", err)
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER fail_management_event`); err != nil {
		t.Fatal(err)
	}
	require(t, request(f.handler, "PUT", path, manager, "retry-role-after-rollback", "", body), 200)
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_manage=0 WHERE credential_id=?`, ownerID); err != nil {
		t.Fatal(err)
	}
	require(t, request(f.handler, "PUT", path, manager, "retry-role-after-rollback", "", body), 403)
}
