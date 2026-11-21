package core

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

const browserProjectLimit = 8

type BrowserSessionProject struct {
	ID     string `json:"id"`
	Handle string `json:"handle"`
	Name   string `json:"name"`
}

// Only identities and role ceilings belong in the encrypted cookie. Names and
// handles are looked up under current authorization, never retained as grants.
// The compact signed field is a comma-separated list of opaque-id:role entries;
// role 0 is read, 1 adds write, and 3 adds presentation management.
type browserProjectGrant struct {
	id           string
	writes       bool
	presentation bool
}

func browserProjectGrants(claims browserSessionClaims) ([]browserProjectGrant, bool) {
	if claims.ProjectGrants == "" {
		return nil, true
	}
	parts := strings.Split(claims.ProjectGrants, ",")
	if len(parts) > browserProjectLimit {
		return nil, false
	}
	grants := make([]browserProjectGrant, 0, len(parts))
	seen, current := map[string]bool{}, false
	for _, part := range parts {
		id, role, ok := strings.Cut(part, ":")
		if !ok || !browserSessionIdentity.MatchString(id) || seen[id] || role != "0" && role != "1" && role != "3" {
			return nil, false
		}
		seen[id] = true
		grant := browserProjectGrant{id: id, writes: role != "0", presentation: role == "3"}
		if id == claims.ProjectID {
			current = true
			if grant.writes != claims.AllowWrites || grant.presentation != claims.CanEditPresentation {
				return nil, false
			}
		}
		grants = append(grants, grant)
	}
	return grants, current
}

func browserProjectGrantsValid(claims browserSessionClaims) bool {
	_, valid := browserProjectGrants(claims)
	return valid
}

func (s *Store) captureBrowserProjectGrants(ctx context.Context, tx *sql.Tx, scope Scope) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.id FROM projects p JOIN credentials c ON c.token_sha256=?
		WHERE c.revoked=0 AND NOT EXISTS(SELECT 1 FROM connector_credentials b WHERE b.credential_id=c.id)
		AND (c.is_admin=1 OR EXISTS(SELECT 1 FROM grants g WHERE g.credential_id=c.id AND g.project_id=p.id AND g.can_read=1))
		ORDER BY p.id<>?,p.id LIMIT ?`, scope.tokenHash, scope.ProjectID, browserProjectLimit)
	if err != nil {
		return "", err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return "", err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return "", err
	}
	if len(ids) < 2 {
		return "", nil
	}
	var entries []string
	for _, id := range ids {
		projectScope := scope
		projectScope.ProjectID, projectScope.WorkerID = id, ""
		if !browserSessionIdentity.MatchString(id) {
			return "", Fail("internal_error", "The browser project selection could not be created.", 500)
		}
		if err := s.authorize(ctx, tx, projectScope, "read"); err != nil {
			return "", err
		}
		writes, presentation, err := s.browserCapabilities(ctx, tx, projectScope, true, true)
		if err != nil {
			return "", err
		}
		role := "0"
		if writes {
			role = "1"
		}
		if presentation {
			role = "3"
		}
		entries = append(entries, id+":"+role)
	}
	return strings.Join(entries, ","), nil
}

// This explicit path is used only with verified encrypted grants. It creates a
// fresh project-bound read scope, not a reusable cross-project native scope, and
// intentionally never copies the source project's worker or management ceiling.
func browserTargetScope(source Scope, claims browserSessionClaims, grant browserProjectGrant) Scope {
	return Scope{PrincipalID: source.PrincipalID, ProjectID: grant.id, tokenHash: claims.CredentialHash,
		instanceID: claims.InstanceID, readOnly: true, expiresAt: time.Unix(claims.ExpiresAt, 0),
		sessionProject: grant.id, sessionManage: grant.presentation}
}

func (s *Store) browserSelectableProjects(ctx context.Context, tx *sql.Tx, source Scope, claims browserSessionClaims) (*[]BrowserSessionProject, error) {
	if claims.ProjectGrants == "" {
		return nil, nil
	}
	grants, valid := browserProjectGrants(claims)
	if !valid {
		return nil, Fail("unauthorized", "A valid browser session is required; connect again.", 401)
	}
	projects := make([]BrowserSessionProject, 0, len(grants))
	for _, grant := range grants {
		scope := browserTargetScope(source, claims, grant)
		if err := s.authorize(ctx, tx, scope, "read"); err != nil {
			var e *Error
			if errors.As(err, &e) && (e.Code == "forbidden" || e.Code == "not_found") {
				continue
			}
			return nil, err
		}
		project := BrowserSessionProject{ID: grant.id}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(h.handle,p.id),p.name FROM projects p
			LEFT JOIN project_handles h ON h.project_id=p.id WHERE p.id=?`, grant.id).Scan(&project.Handle, &project.Name); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return &projects, nil
}

// SwitchBrowserSession mints a new independently project-bound cookie from one
// of the original frozen grants. It does not renew expiry or revoke the source;
// a lost response must leave that source available for client recovery.
func (s *Store) SwitchBrowserSession(r *http.Request, project, id string) (BrowserSession, *http.Cookie, error) {
	if len(r.Header.Values("Authorization")) != 0 || len(r.Header.Values("X-FC-Browser-Session")) != 1 {
		return BrowserSession{}, nil, Fail("unauthorized", "Select one browser session authentication method.", 401)
	}
	if err := BrowserSessionOrigin(r, true); err != nil {
		return BrowserSession{}, nil, err
	}
	if !browserSessionID.MatchString(id) || !browserSessionIdentity.MatchString(project) {
		return BrowserSession{}, nil, Fail("validation_failed", "Provide an exact project identity and a new 32-character lowercase hexadecimal session identity.", 400)
	}
	readRequest := r.Clone(r.Context())
	readRequest.Method = http.MethodGet
	source, claims, err := s.browserScope(readRequest)
	if err != nil {
		return BrowserSession{}, nil, err
	}
	if id == claims.SessionID || project == claims.ProjectID {
		return BrowserSession{}, nil, Fail("validation_failed", "Select a different project and a new session identity.", 400)
	}
	cookie := browserSessionCookie(r, id)
	for _, existing := range r.Cookies() {
		if existing.Name == cookie.Name {
			return BrowserSession{}, nil, Fail("conflict", "Select a fresh browser session identity.", 409)
		}
	}
	grants, _ := browserProjectGrants(claims)
	var target *browserProjectGrant
	for i := range grants {
		if grants[i].id == project {
			target = &grants[i]
			break
		}
	}
	if target == nil {
		return BrowserSession{}, nil, Fail("forbidden", "The browser session does not permit this project selection.", 403)
	}
	var out BrowserSession
	err = s.Read(r.Context(), source, func(ctx context.Context, tx *sql.Tx) error {
		scope := browserTargetScope(source, claims, *target)
		if err := s.authorize(ctx, tx, scope, "read"); err != nil {
			return err
		}
		var err error
		out.AllowWrites, out.CanEditPresentation, err = s.browserCapabilities(ctx, tx, scope, target.writes, target.presentation)
		if err == nil {
			out.Projects, err = s.browserSelectableProjects(ctx, tx, source, claims)
		}
		return err
	})
	if err != nil {
		return BrowserSession{}, nil, err
	}
	claims.SessionID, claims.ProjectID = id, project
	claims.AllowWrites, claims.CanEditPresentation = target.writes, target.presentation
	cookie.Value, err = s.browserSessionCodec().Encode(cookie.Name, claims)
	if err != nil {
		return BrowserSession{}, nil, Fail("internal_error", "The browser session could not be switched.", 500)
	}
	cookie.Expires = time.Unix(claims.ExpiresAt, 0).UTC()
	cookie.MaxAge = int(time.Until(cookie.Expires).Seconds())
	if cookie.MaxAge < 1 {
		return BrowserSession{}, nil, Fail("unauthorized", "The browser session has expired; connect again.", 401)
	}
	// Admission must be able to retain both cookies without exceeding the cap.
	if _, err := s.PruneBrowserSwitchSessions(r, cookie); err != nil {
		return BrowserSession{}, nil, err
	}
	out.SessionID, out.InstanceID, out.ProjectID = id, claims.InstanceID, project
	out.ExpiresAt = cookie.Expires.Format(time.RFC3339)
	return out, cookie, nil
}

// PruneBrowserSwitchSessions uses the ordinary count/byte caps but reserves both
// the new cookie and the selected source until the client confirms adoption.
func (s *Store) PruneBrowserSwitchSessions(r *http.Request, current *http.Cookie) ([]*http.Cookie, error) {
	if current == nil {
		return nil, Fail("validation_failed", "A target browser session cookie is required.", 400)
	}
	sourceName := browserSessionCookie(r, r.Header.Get("X-FC-Browser-Session")).Name
	type retained struct {
		cookie  *http.Cookie
		expires int64
	}
	var candidates []retained
	count, bytes := 1, len(current.Name)+len(current.Value)+1
	seen := map[string]bool{current.Name: true}
	now := time.Now()
	codec := s.browserSessionCodec()
	for _, cookie := range r.Cookies() {
		if seen[cookie.Name] {
			continue
		}
		seen[cookie.Name] = true
		if cookie.Name == sourceName {
			count++
			bytes += len(cookie.Name) + len(cookie.Value) + 3
			continue
		}
		prefix := "fc-local-browser-"
		if !BrowserSessionLocalHTTP(r) && strings.HasPrefix(cookie.Name, "__Host-fc-browser-") {
			prefix = "__Host-fc-browser-"
		}
		id := strings.TrimPrefix(cookie.Name, prefix)
		var claims browserSessionClaims
		if !strings.HasPrefix(cookie.Name, prefix) || !browserSessionID.MatchString(id) ||
			codec.Decode(cookie.Name, cookie.Value, &claims) != nil || claims.SessionID != id || claims.InstanceID != s.instanceID ||
			!browserSessionIdentity.MatchString(claims.ProjectID) || !browserCredentialHash.MatchString(claims.CredentialHash) || !browserSessionExpiryValid(claims.ExpiresAt, now) {
			continue
		}
		candidates = append(candidates, retained{cookie, claims.ExpiresAt})
	}
	if bytes > browserSessionCookieBudget || count != 2 {
		return nil, Fail("conflict", "The browser session cookie budget cannot preserve this switch; connect again.", 409)
	}
	slices.SortFunc(candidates, func(a, b retained) int {
		if a.expires > b.expires {
			return -1
		}
		if a.expires < b.expires {
			return 1
		}
		return strings.Compare(a.cookie.Name, b.cookie.Name)
	})
	var expired []*http.Cookie
	for _, candidate := range candidates {
		size := len(candidate.cookie.Name) + len(candidate.cookie.Value) + 3
		if count < browserSessionCookieLimit && bytes+size <= browserSessionCookieBudget {
			count++
			bytes += size
			continue
		}
		cookie := browserSessionCookie(r, "")
		cookie.Name, cookie.MaxAge, cookie.Expires = candidate.cookie.Name, -1, time.Unix(1, 0).UTC()
		expired = append(expired, cookie)
	}
	return expired, nil
}
