package core

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

// ConnectorCredentialSchemaSQL returns the canonical base schema v4 objects.
func ConnectorCredentialSchemaSQL() []string {
	return []string{
		`CREATE TABLE connector_credentials (
		credential_id TEXT NOT NULL PRIMARY KEY REFERENCES credentials(id),
		project_id TEXT NOT NULL CHECK(project_id<>''),
		worker_id TEXT NOT NULL CHECK(worker_id<>''),
		FOREIGN KEY (credential_id,project_id) REFERENCES grants(credential_id,project_id),
		FOREIGN KEY (project_id,worker_id) REFERENCES workers(project_id,id)
	)`,
		`CREATE TRIGGER connector_binding_update BEFORE UPDATE ON connector_credentials
	BEGIN SELECT RAISE(ABORT, 'immutable connector binding'); END`,
		`CREATE TRIGGER connector_binding_delete BEFORE DELETE ON connector_credentials
	BEGIN SELECT RAISE(ABORT, 'immutable connector binding'); END`,
		`CREATE TRIGGER connector_binding_insert BEFORE INSERT ON connector_credentials
	WHEN EXISTS (SELECT 1 FROM connector_credentials WHERE credential_id=NEW.credential_id)
	OR NOT EXISTS (SELECT 1 FROM credentials WHERE id=NEW.credential_id AND is_admin=0 AND revoked=0)
	BEGIN SELECT RAISE(ABORT, 'invalid connector binding'); END`,
	}
}

// BindConnectorCredential is part of the native grant mutation. The new
// credential and grant must be inserted in the same write transaction.
func BindConnectorCredential(ctx context.Context, tx *sql.Tx, id, project, worker string) error {
	if err := CheckActiveWorker(ctx, tx, project, worker); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO connector_credentials(credential_id,project_id,worker_id) VALUES(?,?,?)`, id, project, worker)
	return err
}

type ConnectorBinding struct {
	ProjectID string
	WorkerID  string
	write     bool
	manage    bool
}

type bearerCredential struct {
	id, principal string
	admin         bool
	binding       ConnectorBinding
}

func invalidBearer() error {
	return &Error{Code: "unauthorized", Message: "A valid bearer credential is required.", Status: http.StatusUnauthorized, bearerChallenge: true}
}

func bearerToken(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", invalidBearer()
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if token == "" || len(token) > 256 || strings.ContainsAny(token, " \t\r\n") {
		return "", invalidBearer()
	}
	return token, nil
}

func currentCredential(ctx context.Context, q querier, hash string) (bearerCredential, error) {
	var c bearerCredential
	err := q.QueryRowContext(ctx, `SELECT c.id,c.principal_id,c.is_admin,COALESCE(b.project_id,''),COALESCE(b.worker_id,'')
		FROM credentials c LEFT JOIN connector_credentials b ON b.credential_id=c.id
		WHERE c.token_sha256=? AND c.revoked=0`, hash).Scan(&c.id, &c.principal, &c.admin, &c.binding.ProjectID, &c.binding.WorkerID)
	if errors.Is(err, sql.ErrNoRows) {
		return bearerCredential{}, invalidBearer()
	}
	if err != nil {
		return bearerCredential{}, storageError(err)
	}
	return c, nil
}

func currentConnectorAccess(ctx context.Context, q querier, c bearerCredential) (ConnectorBinding, error) {
	binding := c.binding
	if c.admin || binding.ProjectID == "" || binding.WorkerID == "" {
		return ConnectorBinding{}, invalidBearer()
	}
	err := q.QueryRowContext(ctx, `SELECT g.can_write,g.can_manage FROM workers w JOIN grants g ON g.project_id=w.project_id
		JOIN credentials c ON c.id=g.credential_id AND c.revoked=0 AND c.is_admin=0
		WHERE g.credential_id=? AND g.can_read=1 AND w.project_id=? AND w.id=?
		AND NOT EXISTS(SELECT 1 FROM worker_retirements r WHERE r.project_id=w.project_id AND r.worker_id=w.id)`,
		c.id, binding.ProjectID, binding.WorkerID).Scan(&binding.write, &binding.manage)
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectorBinding{}, invalidBearer()
	}
	if err != nil {
		return ConnectorBinding{}, storageError(err)
	}
	return binding, nil
}

// AuthenticateConnector derives the entire binding from a current dedicated
// bearer. Native handlers independently reauthorize that bearer for each tool.
func (s *Store) AuthenticateConnector(r *http.Request) (ConnectorBinding, string, error) {
	token, err := bearerToken(r)
	if err != nil {
		return ConnectorBinding{}, "", err
	}
	if strings.Contains(token, ",") {
		return ConnectorBinding{}, "", invalidBearer()
	}
	binding, err := s.connectorAccess(r.Context(), token)
	if err != nil {
		return ConnectorBinding{}, "", err
	}
	return binding, token, nil
}

// ReauthorizeConnector checks the original binding and its current access
// before returning a tool result to the authenticated caller.
func (s *Store) ReauthorizeConnector(ctx context.Context, token string, binding ConnectorBinding) error {
	current, err := s.connectorAccess(ctx, token)
	if err != nil {
		return err
	}
	// A long tool may need more than read access. A reduction in any original
	// permission invalidates its pending response, even if read access remains.
	if current.ProjectID != binding.ProjectID || current.WorkerID != binding.WorkerID || binding.write && !current.write || binding.manage && !current.manage {
		return invalidBearer()
	}
	return nil
}

func (s *Store) connectorAccess(ctx context.Context, token string) (ConnectorBinding, error) {
	c, err := currentCredential(ctx, s.db, tokenHash(token))
	if err != nil {
		return ConnectorBinding{}, err
	}
	return currentConnectorAccess(ctx, s.db, c)
}
