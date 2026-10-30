package core

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/securecookie"
)

const browserSessionLifetime = 24 * time.Hour

var browserSessionID = regexp.MustCompile(`^[0-9a-f]{32}$`)
var browserCredentialHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
var browserSessionIdentity = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type browserSessionClaims struct {
	SessionID           string `json:"session_id"`
	InstanceID          string `json:"instance_id"`
	ProjectID           string `json:"project_id"`
	CredentialHash      string `json:"credential_hash"`
	ExpiresAt           int64  `json:"expires_at"`
	AllowWrites         bool   `json:"allow_writes"`
	CanEditPresentation bool   `json:"can_edit_presentation"`
}

type BrowserSession struct {
	SessionID           string `json:"session_id"`
	InstanceID          string `json:"instance_id"`
	ProjectID           string `json:"project_id"`
	ExpiresAt           string `json:"expires_at"`
	AllowWrites         bool   `json:"allow_writes"`
	CanEditPresentation bool   `json:"can_edit_presentation"`
}

func (s *Store) browserSessionCodec() *securecookie.SecureCookie {
	derive := func(purpose string) []byte {
		mac := hmac.New(sha256.New, s.signingKey)
		_, _ = mac.Write([]byte(purpose))
		return mac.Sum(nil)
	}
	codec := securecookie.New(derive("firecontroller/browser-session/signing/v1"), derive("firecontroller/browser-session/encryption/v1"))
	codec.SetSerializer(securecookie.JSONEncoder{})
	return codec.MaxAge(int(browserSessionLifetime.Seconds())).MaxLength(4096)
}

// BrowserSessionLocalHTTP permits insecure cookies only on literal loopback
// hosts. Proxy forwarding headers never influence transport security.
func BrowserSessionLocalHTTP(r *http.Request) bool {
	if r.TLS != nil {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func browserSessionCookie(r *http.Request, id string) *http.Cookie {
	secure, prefix := true, "__Host-fc-browser-"
	if BrowserSessionLocalHTTP(r) {
		secure, prefix = false, "fc-local-browser-"
	}
	return &http.Cookie{Name: prefix + id, Path: "/", Secure: secure, HttpOnly: true, SameSite: http.SameSiteStrictMode}
}

// CreateBrowserSession derives an attenuated encrypted transport session from a
// current bearer credential, pinning its effective initial capabilities. It
// allocates no durable mutation or user receipt.
func (s *Store) CreateBrowserSession(r *http.Request, id string) (BrowserSession, *http.Cookie, error) {
	if !browserSessionID.MatchString(id) {
		return BrowserSession{}, nil, Fail("validation_failed", "Provide a 32-character lowercase hexadecimal session identity.", 400)
	}
	if len(r.Header.Values("X-FC-Browser-Session")) != 0 || len(r.Header.Values("Authorization")) != 1 {
		return BrowserSession{}, nil, Fail("unauthorized", "A bearer credential is required to connect a browser session.", 401)
	}
	if len(r.Header.Values("X-FC-Instance")) != 1 {
		return BrowserSession{}, nil, Fail("validation_failed", "Provide one expected instance identity.", 400)
	}
	scope, err := s.Scope(r, "read")
	if err != nil {
		return BrowserSession{}, nil, err
	}
	var writes, presentation bool
	err = s.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		writes, presentation, err = s.browserCapabilities(ctx, tx, scope, true, true)
		return err
	})
	if err != nil {
		return BrowserSession{}, nil, err
	}
	expires := time.Now().UTC().Truncate(time.Second).Add(browserSessionLifetime)
	claims := browserSessionClaims{SessionID: id, InstanceID: s.instanceID, ProjectID: scope.ProjectID, CredentialHash: scope.tokenHash, ExpiresAt: expires.Unix(), AllowWrites: writes, CanEditPresentation: presentation}
	cookie := browserSessionCookie(r, id)
	cookie.Value, err = s.browserSessionCodec().Encode(cookie.Name, claims)
	if err != nil {
		return BrowserSession{}, nil, Fail("internal_error", "The browser session could not be created.", 500)
	}
	cookie.MaxAge = int(browserSessionLifetime.Seconds())
	cookie.Expires = expires
	return BrowserSession{SessionID: id, InstanceID: s.instanceID, ProjectID: scope.ProjectID, ExpiresAt: expires.Format(time.RFC3339), AllowWrites: writes, CanEditPresentation: presentation}, cookie, nil
}

// CurrentBrowserSession returns effective capabilities of the explicitly bound
// cookie only. A bearer credential cannot inspect or promote a browser session.
func (s *Store) CurrentBrowserSession(r *http.Request) (BrowserSession, error) {
	if len(r.Header.Values("Authorization")) != 0 || len(r.Header.Values("X-FC-Browser-Session")) != 1 {
		return BrowserSession{}, Fail("unauthorized", "Select one browser session authentication method.", 401)
	}
	scope, claims, err := s.browserScope(r)
	if err != nil {
		return BrowserSession{}, err
	}
	out := BrowserSession{SessionID: claims.SessionID, InstanceID: claims.InstanceID, ProjectID: claims.ProjectID, ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC().Format(time.RFC3339)}
	err = s.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		out.AllowWrites, out.CanEditPresentation, err = s.browserCapabilities(ctx, tx, scope, claims.AllowWrites, claims.CanEditPresentation)
		return err
	})
	return out, err
}

// RenewBrowserSession renews only an existing cookie-bound connection. Reading
// current authority never replaces the original encrypted capability ceiling.
func (s *Store) RenewBrowserSession(r *http.Request) (BrowserSession, *http.Cookie, error) {
	if len(r.Header.Values("Authorization")) != 0 || len(r.Header.Values("X-FC-Browser-Session")) != 1 {
		return BrowserSession{}, nil, Fail("unauthorized", "Select one browser session authentication method.", 401)
	}
	if err := BrowserSessionOrigin(r, true); err != nil {
		return BrowserSession{}, nil, err
	}
	// The transport is POST because it replaces a cookie, but renewal requires
	// read access even when the original connection cannot perform mutations.
	readRequest := r.Clone(r.Context())
	readRequest.Method = http.MethodGet
	scope, claims, err := s.browserScope(readRequest)
	if err != nil {
		return BrowserSession{}, nil, err
	}
	var writes, presentation bool
	err = s.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		writes, presentation, err = s.browserCapabilities(ctx, tx, scope, claims.AllowWrites, claims.CanEditPresentation)
		return err
	})
	if err != nil {
		return BrowserSession{}, nil, err
	}
	expires := time.Now().UTC().Truncate(time.Second).Add(browserSessionLifetime)
	claims.ExpiresAt = expires.Unix()
	cookie := browserSessionCookie(r, claims.SessionID)
	cookie.Value, err = s.browserSessionCodec().Encode(cookie.Name, claims)
	if err != nil {
		return BrowserSession{}, nil, Fail("internal_error", "The browser session could not be renewed.", 500)
	}
	cookie.MaxAge = int(browserSessionLifetime.Seconds())
	cookie.Expires = expires
	return BrowserSession{SessionID: claims.SessionID, InstanceID: claims.InstanceID, ProjectID: claims.ProjectID, ExpiresAt: expires.Format(time.RFC3339), AllowWrites: writes, CanEditPresentation: presentation}, cookie, nil
}

func (s *Store) browserCapabilities(ctx context.Context, q querier, scope Scope, requested, allowPresentation bool) (bool, bool, error) {
	if !requested {
		return false, false, nil
	}
	// Probe original authority without widening the caller's read scope. Browser
	// lifetime and project binding remain enforced by authorize.
	scope.readOnly = false
	if err := s.authorize(ctx, q, scope, "write"); err != nil {
		var e *Error
		if errors.As(err, &e) && e.Code == "forbidden" {
			return false, false, nil
		}
		return false, false, err
	}
	if !allowPresentation {
		return true, false, nil
	}
	if err := s.authorize(ctx, q, scope, "admin"); err != nil {
		var e *Error
		if errors.As(err, &e) && e.Code == "forbidden" {
			return true, false, nil
		}
		return false, false, err
	}
	return true, true, nil
}

// ExpireBrowserSession clears only the explicitly selected tab's cookie. It
// deliberately works after the session expires and never revokes a credential.
func ExpireBrowserSession(r *http.Request) (*http.Cookie, string, error) {
	values := r.Header.Values("X-FC-Browser-Session")
	if len(values) != 1 || !browserSessionID.MatchString(values[0]) {
		return nil, "", Fail("validation_failed", "Select one valid browser session identity.", 400)
	}
	if len(r.Header.Values("Authorization")) != 0 {
		return nil, "", Fail("unauthorized", "Select one authentication method.", 401)
	}
	cookie := browserSessionCookie(r, values[0])
	cookie.MaxAge = -1
	cookie.Expires = time.Unix(1, 0).UTC()
	return cookie, values[0], nil
}

func (s *Store) authenticateBrowserSession(r *http.Request) (Scope, error) {
	scope, _, err := s.browserScope(r)
	return scope, err
}

func (s *Store) browserScope(r *http.Request) (Scope, browserSessionClaims, error) {
	bad := Fail("unauthorized", "A valid browser session is required; connect again.", 401)
	values := r.Header.Values("X-FC-Browser-Session")
	if len(values) != 1 || !browserSessionID.MatchString(values[0]) {
		return Scope{}, browserSessionClaims{}, bad
	}
	cookie := browserSessionCookie(r, values[0])
	matches := 0
	value := ""
	for _, c := range r.Cookies() {
		if c.Name == cookie.Name {
			matches++
			value = c.Value
		}
	}
	if matches != 1 || len(value) > 4096 {
		return Scope{}, browserSessionClaims{}, bad
	}
	var claims browserSessionClaims
	if s.browserSessionCodec().Decode(cookie.Name, value, &claims) != nil || claims.SessionID != values[0] || claims.InstanceID != s.instanceID || !browserSessionIdentity.MatchString(claims.InstanceID) || !browserSessionIdentity.MatchString(claims.ProjectID) || !browserCredentialHash.MatchString(claims.CredentialHash) || !browserSessionExpiryValid(claims.ExpiresAt, time.Now()) {
		return Scope{}, browserSessionClaims{}, bad
	}
	var principal string
	err := s.db.QueryRowContext(r.Context(), `SELECT principal_id FROM credentials WHERE token_sha256=? AND revoked=0`, claims.CredentialHash).Scan(&principal)
	if errors.Is(err, sql.ErrNoRows) {
		return Scope{}, browserSessionClaims{}, bad
	}
	if err != nil {
		return Scope{}, browserSessionClaims{}, storageError(err)
	}
	worker := r.Header.Get("X-FC-Worker")
	if len(r.Header.Values("X-FC-Worker")) > 1 || len(worker) > 128 {
		return Scope{}, browserSessionClaims{}, Fail("validation_failed", "Select at most one valid worker identity.", 400)
	}
	if len(r.Header.Values("X-FC-Instance")) != 1 {
		return Scope{}, browserSessionClaims{}, Fail("validation_failed", "Provide one expected instance identity.", 400)
	}
	if err := s.checkExpectedInstance(r); err != nil {
		return Scope{}, browserSessionClaims{}, err
	}
	projectReference := r.PathValue("project_id")
	project := projectReference
	if project == "" {
		if r.URL.Path != "/api/v1/instance" {
			return Scope{}, browserSessionClaims{}, Fail("forbidden", "The browser session permits only its project reads.", 403)
		}
		project = claims.ProjectID
	} else if resolved, resolveErr := s.ResolveProjectID(r.Context(), project); resolveErr != nil {
		return Scope{}, browserSessionClaims{}, resolveErr
	} else {
		project = resolved
	}
	if project != claims.ProjectID {
		return Scope{}, browserSessionClaims{}, Fail("forbidden", "The browser session permits only its project reads.", 403)
	}
	scope := Scope{PrincipalID: principal, ProjectID: project, WorkerID: worker, tokenHash: claims.CredentialHash, instanceID: s.instanceID, readOnly: true, expiresAt: time.Unix(claims.ExpiresAt, 0), sessionProject: claims.ProjectID}
	// Every read remains attenuated, including /instance and administrator
	// credentials. Only these explicit mutation routes can widen to write.
	permission := "read"
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if !claims.AllowWrites || !browserMutationAllowed(r, projectReference) || r.Method == http.MethodPut && !claims.CanEditPresentation {
			return Scope{}, browserSessionClaims{}, Fail("forbidden", "The browser session does not permit this operation.", 403)
		}
		if err = BrowserSessionOrigin(r, true); err != nil {
			return Scope{}, browserSessionClaims{}, err
		}
		scope.readOnly = false
		permission = "write"
	}
	if err = s.authorize(r.Context(), s.db, scope, permission); err != nil {
		return Scope{}, browserSessionClaims{}, err
	}
	return scope, claims, nil
}

func browserSessionExpiryValid(expiresAt int64, now time.Time) bool {
	return expiresAt > now.Unix() && expiresAt <= now.Add(browserSessionLifetime+5*time.Second).Unix()
}
