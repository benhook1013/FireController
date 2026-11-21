package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func browserSwitchFixture(t *testing.T) *Store {
	t.Helper()
	s, _, _ := streamFixture(t)
	for i, id := range []string{"read", "manage", "hidden", "late"} {
		multiConnectorExec(t, s, `INSERT INTO projects VALUES(?,?,?,?)`, id, "Name "+id, i+2, "synthetic/"+id)
		multiConnectorExec(t, s, `INSERT INTO project_handles VALUES(?,?)`, id, "handle-"+id)
	}
	multiConnectorExec(t, s, `INSERT INTO project_handles VALUES('project','handle-project');
		INSERT INTO grants VALUES('credential','read',1,0,0),('credential','manage',1,1,1);
		INSERT INTO credentials VALUES('other','other-principal',?,0,0);
		INSERT INTO grants VALUES('other','hidden',1,1,1)`, tokenHash("synthetic-unrelated-credential-token"))
	return s
}

func browserSwitchLogin(t *testing.T, s *Store, project, id string, enabled bool) (BrowserSession, *http.Cookie) {
	t.Helper()
	r := httptest.NewRequest("POST", "http://localhost/api/v1/projects/"+project+"/browser-session", nil)
	r.SetPathValue("project_id", project)
	r.Header.Set("Authorization", "Bearer synthetic-core-stream-token-1234567")
	r.Header.Set("X-FC-Instance", s.InstanceID())
	out, cookie, err := s.CreateBrowserSessionWithProjectSwitching(r, id, enabled)
	if err != nil {
		t.Fatal(err)
	}
	return out, cookie
}

func browserSwitchRequest(s *Store, project, id string, cookie *http.Cookie) *http.Request {
	r := browserCoreRequest(s, id, cookie)
	r.SetPathValue("project_id", project)
	r.Method = "POST"
	r.URL.Path = "/api/v1/projects/" + project + "/browser-session/switch"
	r.Header.Set("Origin", "http://localhost")
	r.Header.Del("X-FC-Worker")
	return r
}

func browserSwitchProjects(t *testing.T, out BrowserSession, ids ...string) {
	t.Helper()
	if out.Projects == nil {
		t.Fatal("missing opt-in project list")
	}
	var actual []string
	for _, project := range *out.Projects {
		actual = append(actual, project.ID)
		if project.Name == "" || project.Handle == "" {
			t.Fatal("incomplete selectable project metadata")
		}
	}
	if !reflect.DeepEqual(actual, ids) {
		t.Fatalf("selectable projects = %v, want %v", actual, ids)
	}
}

func TestBrowserProjectSwitchOptInFrozenCredentialDiscoveryAndLegacyWire(t *testing.T) {
	s := browserSwitchFixture(t)
	id := strings.Repeat("a", 32)
	legacy, legacyCookie := browserSwitchLogin(t, s, "project", id, false)
	if legacy.Projects != nil || renewalClaims(t, s, legacyCookie).ProjectGrants != "" {
		t.Fatal("legacy login acquired project switching")
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil || len(wire) != 6 {
		t.Fatal("legacy response changed")
	}
	var claimsWire map[string]any
	if err := s.browserSessionCodec().Decode(legacyCookie.Name, legacyCookie.Value, &claimsWire); err != nil || len(claimsWire) != 7 {
		t.Fatal("legacy encrypted wire changed", err)
	}
	// Reproduce a pre-capability cookie, with all new fields absent.
	delete(claimsWire, "allow_writes")
	delete(claimsWire, "can_edit_presentation")
	old := *legacyCookie
	old.Value, err = s.browserSessionCodec().Encode(old.Name, claimsWire)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.CurrentBrowserSession(browserCoreRequest(s, id, &old))
	if err != nil || current.Projects != nil || current.AllowWrites || current.CanEditPresentation {
		t.Fatal("old cookie changed authority", err)
	}
	oldRenewed, oldNext, err := s.RenewBrowserSession(renewalCoreRequest(s, id, &old))
	if err != nil || oldRenewed.Projects != nil || oldRenewed.AllowWrites || oldRenewed.CanEditPresentation || renewalClaims(t, s, oldNext).ProjectGrants != "" {
		t.Fatal("old cookie renewal acquired switching or write authority", err)
	}
	_, next, err := s.SwitchBrowserSession(browserSwitchRequest(s, "project", id, &old), "read", strings.Repeat("b", 32))
	browserCoreCode(t, err, "forbidden")
	if next != nil {
		t.Fatal("old cookie issued switching authority")
	}
	out, cookie := browserSwitchLogin(t, s, "project", id, true)
	browserSwitchProjects(t, out, "project", "manage", "read")
	if (*out.Projects)[1].Name != "Name manage" || (*out.Projects)[1].Handle != "handle-manage" {
		t.Fatal("project metadata mismatch")
	}
	initial := renewalClaims(t, s, cookie)
	if initial.ProjectGrants == "" || strings.Contains(initial.ProjectGrants, "hidden") {
		t.Fatal("discovery used another credential's authority")
	}
	multiConnectorExec(t, s, `INSERT INTO grants VALUES('credential','late',1,1,1);
		UPDATE projects SET name='Current name' WHERE id='read';
		UPDATE project_handles SET handle='current-read' WHERE project_id='read'`)
	current, err = s.CurrentBrowserSession(browserCoreRequest(s, id, cookie))
	if err != nil {
		t.Fatal(err)
	}
	browserSwitchProjects(t, current, "project", "manage", "read")
	if (*current.Projects)[2].Name != "Current name" || (*current.Projects)[2].Handle != "current-read" {
		t.Fatal("project metadata was frozen instead of current-authorized")
	}
	renewed, next, err := s.RenewBrowserSession(renewalCoreRequest(s, id, cookie))
	if err != nil {
		t.Fatal(err)
	}
	browserSwitchProjects(t, renewed, "project", "manage", "read")
	if renewalClaims(t, s, next).ProjectGrants != initial.ProjectGrants {
		t.Fatal("renewal widened frozen grants")
	}
	_, _, err = s.SwitchBrowserSession(browserSwitchRequest(s, "project", id, cookie), "late", strings.Repeat("c", 32))
	browserCoreCode(t, err, "forbidden")
}

func TestBrowserProjectSwitchIndependentTargetCeilingsAndProjectIsolation(t *testing.T) {
	s := browserSwitchFixture(t)
	id, newID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	out, cookie := browserSwitchLogin(t, s, "read", id, true)
	if out.AllowWrites || out.CanEditPresentation {
		t.Fatal("reader fixture widened")
	}
	claims := renewalClaims(t, s, cookie)
	claims.ExpiresAt = time.Now().Add(time.Hour).Unix()
	cookie = renewalCookie(t, s, cookie, claims)
	r := browserSwitchRequest(s, "read", id, cookie)
	switched, targetCookie, err := s.SwitchBrowserSession(r, "manage", newID)
	if err != nil || !switched.AllowWrites || !switched.CanEditPresentation || switched.ProjectID != "manage" || switched.ExpiresAt != time.Unix(claims.ExpiresAt, 0).UTC().Format(time.RFC3339) {
		t.Fatal("switch failed to use target's independent original ceiling", err)
	}
	browserSwitchProjects(t, switched, "read", "manage", "project")
	if targetCookie.Name == cookie.Name || targetCookie.MaxAge > 3600 || targetCookie.MaxAge < 3590 || !targetCookie.HttpOnly || targetCookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("switch replaced source, renewed lifetime or weakened cookie")
	}
	targetClaims := renewalClaims(t, s, targetCookie)
	if targetClaims.ProjectGrants != claims.ProjectGrants || targetClaims.CredentialHash != claims.CredentialHash || targetClaims.ExpiresAt != claims.ExpiresAt {
		t.Fatal("switch changed frozen credential, grants or expiry")
	}
	read := browserSwitchRequest(s, "manage", newID, targetCookie)
	read.Method, read.URL.Path = "GET", "/api/v1/projects/manage"
	scope, err := s.Scope(read, "read")
	if err != nil || scope.WorkerID != "" || !scope.readOnly || scope.sessionProject != "manage" {
		t.Fatal("target read inherited worker or native authority", err)
	}
	_, err = s.Scope(read, "write")
	browserCoreCode(t, err, "forbidden")
	read.Method, read.URL.Path = "POST", "/api/v1/projects/manage/messages"
	scope, err = s.Scope(read, "write")
	if err != nil || scope.WorkerID != "" || scope.readOnly {
		t.Fatal("target write authority unavailable", err)
	}
	forged := scope
	forged.ProjectID = "read"
	err = s.Read(context.Background(), forged, func(context.Context, *sql.Tx) error { t.Fatal("cross-project retained scope accepted"); return nil })
	browserCoreCode(t, err, "forbidden")
	// The new selector cannot make the old cookie valid for the target, and
	// neither cookie's native routes can cross its active project.
	for _, test := range []struct {
		project, selector string
		cookie            *http.Cookie
		code              string
	}{{"manage", id, cookie, "forbidden"}, {"read", newID, targetCookie, "forbidden"}, {"manage", newID, cookie, "unauthorized"}} {
		r := browserSwitchRequest(s, test.project, test.selector, test.cookie)
		r.Method = "GET"
		_, err := s.Scope(r, "read")
		browserCoreCode(t, err, test.code)
	}
	if _, err := s.CurrentBrowserSession(browserSwitchRequestAsRead(s, "read", id, cookie)); err != nil {
		t.Fatal("switch invalidated old cookie before adoption", err)
	}
	back, readCookie, err := s.SwitchBrowserSession(browserSwitchRequest(s, "manage", newID, targetCookie), "read", strings.Repeat("c", 32))
	if err != nil || back.AllowWrites || back.CanEditPresentation {
		t.Fatal("switch inherited previous project's management rights", err)
	}
	write := browserSwitchRequest(s, "read", back.SessionID, readCookie)
	write.URL.Path = "/api/v1/projects/read/messages"
	_, err = s.Scope(write, "write")
	browserCoreCode(t, err, "forbidden")
}

func browserSwitchRequestAsRead(s *Store, project, id string, cookie *http.Cookie) *http.Request {
	r := browserSwitchRequest(s, project, id, cookie)
	r.Method = "GET"
	return r
}

func TestBrowserProjectSwitchDropsSourceWorkerAndRechecksTargetCapabilities(t *testing.T) {
	for _, kind := range []string{"reader-promoted", "writer-promoted", "manager-downgraded"} {
		t.Run(kind, func(t *testing.T) {
			s := browserSwitchFixture(t)
			target := "read"
			if kind == "writer-promoted" {
				target = "project"
			}
			if kind == "manager-downgraded" {
				target = "manage"
			}
			id, nextID := strings.Repeat("a", 32), strings.Repeat("b", 32)
			_, cookie := browserSwitchLogin(t, s, "project", id, true)
			if kind == "manager-downgraded" {
				multiConnectorExec(t, s, `UPDATE grants SET can_write=0,can_manage=0 WHERE project_id=?`, target)
			} else {
				multiConnectorExec(t, s, `UPDATE grants SET can_write=1,can_manage=1 WHERE project_id=?`, target)
			}
			source := "project"
			if target == source {
				// Move to a different frozen project first, without changing pins.
				_, intermediate, err := s.SwitchBrowserSession(browserSwitchRequest(s, source, id, cookie), "manage", nextID)
				if err != nil {
					t.Fatal(err)
				}
				cookie, source, id, nextID = intermediate, "manage", nextID, strings.Repeat("c", 32)
			}
			r := browserSwitchRequest(s, source, id, cookie)
			if source == "project" {
				r.Header.Set("X-FC-Worker", "worker") // absent from every target
			}
			out, next, err := s.SwitchBrowserSession(r, target, nextID)
			if err != nil || out.AllowWrites != (kind == "writer-promoted") || out.CanEditPresentation {
				t.Fatal("switch widened initial target ceiling or ignored downgrade", err)
			}
			write := browserSwitchRequest(s, target, nextID, next)
			write.URL.Path = "/api/v1/projects/" + target + "/messages"
			_, writeErr := s.Scope(write, "write")
			if kind == "writer-promoted" {
				if writeErr != nil {
					t.Fatal("target lost original write authority", writeErr)
				}
			} else {
				browserCoreCode(t, writeErr, "forbidden")
			}
			claims := renewalClaims(t, s, next)
			if claims.AllowWrites != (kind != "reader-promoted") || claims.CanEditPresentation != (kind == "manager-downgraded") {
				t.Fatal("transient capabilities replaced original target ceiling")
			}
			r = browserSwitchRequest(s, target, nextID, next)
			r.URL.Path = "/api/v1/projects/" + target + "/browser-session/renew"
			renewed, next, err := s.RenewBrowserSession(r)
			if err != nil || renewed.AllowWrites != out.AllowWrites || renewed.CanEditPresentation {
				t.Fatal("renewal changed target ceiling", err)
			}
			if kind == "manager-downgraded" {
				multiConnectorExec(t, s, `UPDATE grants SET can_write=1,can_manage=1 WHERE project_id=?`, target)
				current, err := s.CurrentBrowserSession(browserSwitchRequestAsRead(s, target, nextID, next))
				if err != nil || !current.AllowWrites || !current.CanEditPresentation {
					t.Fatal("downgrade destroyed original ceiling", err)
				}
			}
		})
	}
}

func TestBrowserProjectSwitchRevocationAndStaleScope(t *testing.T) {
	for _, kind := range []string{"target-read", "target-grant-deleted", "target-deleted", "source-read", "credential", "principal-changed", "expired", "foreign-instance", "source-project", "source-worker"} {
		t.Run(kind, func(t *testing.T) {
			s := browserSwitchFixture(t)
			id := strings.Repeat("a", 32)
			_, cookie := browserSwitchLogin(t, s, "project", id, true)
			r := browserSwitchRequest(s, "project", id, cookie)
			code := "forbidden"
			switch kind {
			case "target-read":
				multiConnectorExec(t, s, `UPDATE grants SET can_read=0 WHERE credential_id='credential' AND project_id='read'`)
			case "target-grant-deleted":
				multiConnectorExec(t, s, `DELETE FROM grants WHERE credential_id='credential' AND project_id='read'`)
			case "target-deleted":
				multiConnectorExec(t, s, `DELETE FROM grants WHERE project_id='read';DELETE FROM project_handles WHERE project_id='read';DELETE FROM projects WHERE id='read'`)
				code = "not_found"
			case "source-read":
				multiConnectorExec(t, s, `UPDATE grants SET can_read=0 WHERE credential_id='credential' AND project_id='project'`)
			case "credential":
				multiConnectorExec(t, s, `UPDATE credentials SET revoked=1 WHERE id='credential'`)
				code = "unauthorized"
			case "principal-changed":
				// A retained source scope must also reauthenticate the exact principal.
				scope, claims, err := s.browserScope(browserSwitchRequestAsRead(s, "project", id, cookie))
				if err != nil {
					t.Fatal(err)
				}
				multiConnectorExec(t, s, `UPDATE credentials SET principal_id='replacement' WHERE id='credential'`)
				err = s.Read(t.Context(), browserTargetScope(scope, claims, browserProjectGrant{id: "read"}), func(context.Context, *sql.Tx) error { t.Fatal("stale principal accepted"); return nil })
				browserCoreCode(t, err, "unauthorized")
				return
			case "expired":
				claims := renewalClaims(t, s, cookie)
				claims.ExpiresAt = time.Now().Unix()
				r = browserSwitchRequest(s, "project", id, renewalCookie(t, s, cookie, claims))
				code = "unauthorized"
			case "foreign-instance":
				r.Header.Set("X-FC-Instance", "foreign")
				code = "instance_mismatch"
			case "source-project":
				r.SetPathValue("project_id", "manage")
			case "source-worker":
				r.Header.Set("X-FC-Worker", "foreign-worker")
				code = "not_found"
			}
			_, next, err := s.SwitchBrowserSession(r, "read", strings.Repeat("b", 32))
			browserCoreCode(t, err, code)
			if next != nil {
				t.Fatal("denied switch minted cookie")
			}
			if strings.HasPrefix(kind, "target-") {
				current, err := s.CurrentBrowserSession(browserCoreRequest(s, id, cookie))
				if err != nil {
					t.Fatal(err)
				}
				browserSwitchProjects(t, current, "project", "manage")
			}
		})
	}
}

func TestBrowserProjectSwitchBoundedAdminDiscoveryAndCookieAdmission(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprint(admin), func(t *testing.T) {
			s := browserSwitchFixture(t)
			multiConnectorExec(t, s, `UPDATE credentials SET is_admin=? WHERE id='credential'`, admin)
			var source string
			for i := range 12 {
				id := fmt.Sprintf("a%010d%s", i, strings.Repeat("x", 117)) // maximum 128-byte identity
				multiConnectorExec(t, s, `INSERT INTO projects VALUES(?,?,?,?)`, id, "Long project", i+10, "synthetic/"+id)
				if !admin {
					multiConnectorExec(t, s, `INSERT INTO grants VALUES('credential',?,1,1,1)`, id)
				}
				source = id
			}
			id := strings.Repeat("f", 32) // source loses ordinary expiry/name tie-breaking
			out, cookie := browserSwitchLogin(t, s, source, id, true)
			if out.Projects == nil || len(*out.Projects) != 8 || (*out.Projects)[0].ID != source || len(cookie.Value) > 4096 {
				t.Fatal("discovery omitted current project, exceeded limit or cookie cap")
			}
			multiConnectorExec(t, s, `INSERT INTO projects VALUES('0-new-project','Post-login project',99,'synthetic/new')`)
			multiConnectorExec(t, s, `INSERT INTO grants VALUES('credential','0-new-project',1,1,1)`)
			current, err := s.CurrentBrowserSession(browserSwitchRequestAsRead(s, source, id, cookie))
			if err != nil || !reflect.DeepEqual(current.Projects, out.Projects) {
				t.Fatal("new project widened frozen administrator/native discovery", err)
			}
			_, _, err = s.SwitchBrowserSession(browserSwitchRequest(s, source, id, cookie), "0-new-project", strings.Repeat("c", 32))
			browserCoreCode(t, err, "forbidden")
			target := (*out.Projects)[7].ID
			r := browserSwitchRequest(s, source, id, cookie)
			for i := range 7 {
				_, extra := browserSwitchLogin(t, s, source, fmt.Sprintf("%032x", i+1), true)
				r.AddCookie(extra)
			}
			switched, next, err := s.SwitchBrowserSession(r, target, strings.Repeat("b", 32))
			if err != nil || len(*switched.Projects) != 8 {
				t.Fatal("bounded switch failed", err)
			}
			pruned, err := s.PruneBrowserSwitchSessions(r, next)
			if err != nil || len(pruned) == 0 {
				t.Fatal("switch did not enforce cookie budget", err)
			}
			removed := map[string]bool{}
			for _, expired := range pruned {
				if expired.Name == cookie.Name || expired.Name == next.Name || expired.MaxAge != -1 {
					t.Fatal("switch pruned its recovery source or target")
				}
				removed[expired.Name] = true
			}
			count, bytes := 1, len(next.Name)+len(next.Value)+1
			for _, old := range r.Cookies() {
				if !removed[old.Name] {
					count++
					bytes += len(old.Name) + len(old.Value) + 3
				}
			}
			if count > browserSessionCookieLimit || bytes > browserSessionCookieBudget {
				t.Fatal("retained switch cookies exceed caps", count, bytes)
			}
		})
	}
}

func TestBrowserProjectSwitchRejectsMalformedFrozenGrantsAndConnector(t *testing.T) {
	s := browserSwitchFixture(t)
	id := strings.Repeat("a", 32)
	_, cookie := browserSwitchLogin(t, s, "project", id, true)
	for _, field := range []string{"project:1,project:1", "read:0", "project:3", "project:2", "project:1,foreign/id:0", "project:1,read:0,manage:3,a:0,b:0,c:0,d:0,e:0,f:0"} {
		claims := renewalClaims(t, s, cookie)
		claims.ProjectGrants = field
		_, err := s.CurrentBrowserSession(browserCoreRequest(s, id, renewalCookie(t, s, cookie, claims)))
		browserCoreCode(t, err, "unauthorized")
	}
	// Opt-in must not become a route around immutable connector worker bindings.
	connector, _ := multiConnectorFixture(t, 2)
	multiConnectorBind(t, connector, 0, 1)
	r := multiConnectorRequest(connector, multiConnectorProject(0), "worker")
	_, next, err := connector.CreateBrowserSessionWithProjectSwitching(r, id, true)
	browserCoreCode(t, err, "forbidden")
	if next != nil {
		t.Fatal("connector minted switchable browser cookie")
	}
}
