package core

import (
	"context"
	"database/sql"
	"errors"
)

// AuthenticatedCredentialID identifies the exact current credential behind an
// authenticated scope without exposing its token or hash. It grants no access.
func (s *Store) AuthenticatedCredentialID(ctx context.Context, tx *sql.Tx, scope Scope) (string, error) {
	permission := "read"
	if scope.Administrative() {
		permission = "admin"
	}
	if err := s.authorize(ctx, tx, scope, permission); err != nil {
		return "", err
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM credentials WHERE token_sha256=? AND principal_id=? AND revoked=0`, scope.tokenHash, scope.PrincipalID).Scan(&id)
	if err != nil {
		return "", storageError(err)
	}
	return id, nil
}

// ProjectAutomationScope revalidates an explicitly retained project automation
// authorization. Callers obtain these identities from their own durable control
// record, never from an unauthenticated request. The exact credential must still
// manage this project and its attributed worker must remain active. Subsequent
// Store operations reauthorize the returned scope again at their boundary.
func (s *Store) ProjectAutomationScope(ctx context.Context, tx *sql.Tx, project, principal, worker, credential string) (Scope, error) {
	if project == "" || principal == "" || worker == "" || credential == "" {
		return Scope{}, Fail("forbidden", "Current project automation authorization is required.", 403)
	}
	var hash string
	err := tx.QueryRowContext(ctx, `SELECT token_sha256 FROM credentials WHERE id=? AND principal_id=? AND revoked=0`, credential, principal).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return Scope{}, Fail("forbidden", "The project automation authorization is no longer current.", 403)
	}
	if err != nil {
		return Scope{}, storageError(err)
	}
	scope := Scope{ProjectID: project, PrincipalID: principal, WorkerID: worker, tokenHash: hash, instanceID: s.instanceID, admin: true}
	if err := s.authorize(ctx, tx, scope, "admin"); err != nil {
		return Scope{}, err
	}
	if err := CheckActiveWorker(ctx, tx, project, worker); err != nil {
		return Scope{}, err
	}
	return scope, nil
}
