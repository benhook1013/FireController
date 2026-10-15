package core

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Authenticate validates a current credential for instance identity inspection.
// Project data still requires Scope and administrative actions AdminScope.
func (s *Store) Authenticate(r *http.Request) (Scope, error) { return s.authenticate(r) }

func (s *Store) Scope(r *http.Request, permission string) (Scope, error) {
	if permission != "read" && permission != "write" {
		return Scope{}, Fail("forbidden", "The requested permission is unavailable.", 403)
	}
	scope, err := s.authenticate(r)
	if err != nil {
		return Scope{}, err
	}
	if scope.ProjectID == "" {
		return Scope{}, Fail("validation_failed", "A project identity is required.", 400)
	}
	if err = s.authorize(r.Context(), s.db, scope, permission); err != nil {
		return Scope{}, err
	}
	return scope, nil
}

func (s *Store) AdminScope(r *http.Request) (Scope, error) {
	scope, err := s.authenticate(r)
	if err != nil {
		return Scope{}, err
	}
	if err = s.authorize(r.Context(), s.db, scope, "admin"); err != nil {
		return Scope{}, err
	}
	scope.admin = true
	return scope, nil
}

func (s *Store) authenticate(r *http.Request) (Scope, error) {
	selectors := r.Header.Values("X-FC-Browser-Session")
	if len(selectors) != 0 {
		if len(r.Header.Values("Authorization")) != 0 {
			return Scope{}, Fail("unauthorized", "Select one authentication method.", 401)
		}
		return s.authenticateBrowserSession(r)
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return Scope{}, Fail("unauthorized", "A valid bearer credential is required.", 401)
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if token == "" || len(token) > 256 || strings.ContainsAny(token, " \t\r\n") {
		return Scope{}, Fail("unauthorized", "A valid bearer credential is required.", 401)
	}
	scope := Scope{ProjectID: r.PathValue("project_id"), WorkerID: r.Header.Get("X-FC-Worker"), tokenHash: tokenHash(token), instanceID: s.instanceID}
	var admin int
	err := s.db.QueryRowContext(r.Context(), "SELECT principal_id,is_admin FROM credentials WHERE token_sha256=? AND revoked=0", scope.tokenHash).Scan(&scope.PrincipalID, &admin)
	if errors.Is(err, sql.ErrNoRows) {
		return Scope{}, Fail("unauthorized", "A valid bearer credential is required.", 401)
	}
	if err != nil {
		return Scope{}, storageError(err)
	}
	if err := s.checkExpectedInstance(r); err != nil {
		return Scope{}, err
	}
	if len(r.Header.Values("X-FC-Worker")) > 1 || len(scope.WorkerID) > 128 {
		return Scope{}, Fail("validation_failed", "Select at most one valid worker identity.", 400)
	}
	return scope, nil
}

func (s *Store) checkExpectedInstance(r *http.Request) error {
	expected := r.Header.Values("X-FC-Instance")
	if len(expected) > 1 || len(expected) == 1 && (expected[0] == "" || len(expected[0]) > 128 || strings.TrimSpace(expected[0]) != expected[0] || !utf8.ValidString(expected[0]) || strings.IndexFunc(expected[0], func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0) {
		return Fail("validation_failed", "Select at most one valid expected instance identity.", 400)
	}
	if len(expected) == 1 && expected[0] != s.instanceID {
		return Fail("instance_mismatch", "The service instance differs from the bound profile; explicitly rebind before continuing.", 409)
	}
	return nil
}

func (s *Store) authorize(ctx context.Context, q querier, scope Scope, permission string) error {
	if scope.readOnly {
		if scope.expiresAt.IsZero() || !time.Now().Before(scope.expiresAt) {
			return Fail("unauthorized", "The browser session has expired; connect again.", 401)
		}
		if permission != "read" || scope.sessionProject == "" || scope.ProjectID != scope.sessionProject {
			return Fail("forbidden", "The browser session permits only its project reads.", 403)
		}
	}
	if scope.instanceID != s.instanceID || scope.tokenHash == "" {
		return Fail("unauthorized", "A valid bearer credential is required.", 401)
	}
	var principal, credential string
	var admin int
	err := q.QueryRowContext(ctx, "SELECT id,principal_id,is_admin FROM credentials WHERE token_sha256=? AND revoked=0", scope.tokenHash).Scan(&credential, &principal, &admin)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && principal != scope.PrincipalID) {
		return Fail("unauthorized", "A valid bearer credential is required.", 401)
	}
	if err != nil {
		return storageError(err)
	}
	if permission == "admin" && admin != 1 {
		return Fail("forbidden", "Administrative permission is required.", 403)
	}
	if scope.ProjectID != "" {
		var exists int
		if err = q.QueryRowContext(ctx, "SELECT 1 FROM projects WHERE id=?", scope.ProjectID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return Fail("not_found", "The selected project is unavailable.", 404)
		} else if err != nil {
			return storageError(err)
		}
		if admin != 1 {
			var read, write int
			err = q.QueryRowContext(ctx, "SELECT can_read,can_write FROM grants WHERE credential_id=? AND project_id=?", credential, scope.ProjectID).Scan(&read, &write)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && (read != 1 || permission == "write" && write != 1)) {
				return Fail("forbidden", "The credential does not grant this project operation.", 403)
			}
			if err != nil {
				return storageError(err)
			}
		}
		if scope.WorkerID != "" {
			var exists int
			err = q.QueryRowContext(ctx, "SELECT 1 FROM workers WHERE project_id=? AND id=?", scope.ProjectID, scope.WorkerID).Scan(&exists)
			if errors.Is(err, sql.ErrNoRows) {
				return Fail("not_found", "The selected worker is unavailable in this project.", 404)
			}
			if err != nil {
				return storageError(err)
			}
		}
	} else if scope.WorkerID != "" {
		return Fail("validation_failed", "Worker attribution requires a project.", 400)
	}
	return nil
}

func CheckWorker(ctx context.Context, tx *sql.Tx, projectID, workerID string) error {
	var exists int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM workers WHERE project_id=? AND id=?", projectID, workerID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return Fail("not_found", "The worker is unavailable in this project.", 404)
	}
	if err != nil {
		return storageError(err)
	}
	return nil
}
