package core

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// MaxConnectorProjects bounds the immutable project bindings of one connector.
const MaxConnectorProjects = 16

// ConnectorCredentialV4SchemaSQL retains the canonical predecessor objects for
// validating backups produced before multi-project connectors were supported.
func ConnectorCredentialV4SchemaSQL() []string {
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

// ConnectorCredentialSchemaSQL returns the canonical base schema v5 objects.
func ConnectorCredentialSchemaSQL() []string {
	objects := ConnectorCredentialV4SchemaSQL()
	objects[0] = `CREATE TABLE connector_credentials (
		credential_id TEXT NOT NULL REFERENCES credentials(id),
		project_id TEXT NOT NULL CHECK(project_id<>''),
		worker_id TEXT NOT NULL CHECK(worker_id<>''),
		PRIMARY KEY (credential_id,project_id),
		FOREIGN KEY (credential_id,project_id) REFERENCES grants(credential_id,project_id),
		FOREIGN KEY (project_id,worker_id) REFERENCES workers(project_id,id)
	)`
	objects[3] = `CREATE TRIGGER connector_binding_insert BEFORE INSERT ON connector_credentials
	WHEN EXISTS (SELECT 1 FROM connector_credentials WHERE credential_id=NEW.credential_id AND project_id=NEW.project_id)
	OR (SELECT count(*) FROM connector_credentials WHERE credential_id=NEW.credential_id)>=` + strconv.Itoa(MaxConnectorProjects) + `
	OR NOT EXISTS (SELECT 1 FROM credentials WHERE id=NEW.credential_id AND is_admin=0 AND revoked=0)
	BEGIN SELECT RAISE(ABORT, 'invalid connector binding'); END`
	return objects
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

// ConnectorAccess contains only currently usable bindings. MultiProject is based
// on all immutable bindings, so losing access cannot restore legacy routing.
type ConnectorAccess struct {
	Bindings     []ConnectorBinding
	MultiProject bool
}

type bearerCredential struct {
	id, principal string
	admin         bool
	connector     bool
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
	err := q.QueryRowContext(ctx, `SELECT c.id,c.principal_id,c.is_admin,
		EXISTS(SELECT 1 FROM connector_credentials b WHERE b.credential_id=c.id)
		FROM credentials c WHERE c.token_sha256=? AND c.revoked=0`, hash).Scan(&c.id, &c.principal, &c.admin, &c.connector)
	if errors.Is(err, sql.ErrNoRows) {
		return bearerCredential{}, invalidBearer()
	}
	if err != nil {
		return bearerCredential{}, storageError(err)
	}
	return c, nil
}

func currentConnectorAccess(ctx context.Context, q querier, c bearerCredential, project string) (ConnectorBinding, error) {
	if c.admin || !c.connector {
		return ConnectorBinding{}, invalidBearer()
	}
	var binding ConnectorBinding
	err := q.QueryRowContext(ctx, `SELECT b.project_id,b.worker_id,g.can_write,g.can_manage
		FROM connector_credentials b JOIN grants g ON g.credential_id=b.credential_id AND g.project_id=b.project_id
		JOIN workers w ON w.project_id=b.project_id AND w.id=b.worker_id
		JOIN credentials c ON c.id=g.credential_id AND c.revoked=0 AND c.is_admin=0
		WHERE b.credential_id=? AND b.project_id=? AND g.can_read=1
		AND NOT EXISTS(SELECT 1 FROM worker_retirements r WHERE r.project_id=w.project_id AND r.worker_id=w.id)`,
		c.id, project).Scan(&binding.ProjectID, &binding.WorkerID, &binding.write, &binding.manage)
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectorBinding{}, invalidBearer()
	}
	if err != nil {
		return ConnectorBinding{}, storageError(err)
	}
	return binding, nil
}

// AuthenticateConnector derives all active bindings from a current dedicated
// bearer. Native handlers independently reauthorize that bearer for each tool.
func (s *Store) AuthenticateConnector(r *http.Request) (ConnectorAccess, string, error) {
	token, err := bearerToken(r)
	if err != nil {
		return ConnectorAccess{}, "", err
	}
	if strings.Contains(token, ",") {
		return ConnectorAccess{}, "", invalidBearer()
	}
	access, err := s.connectorAccess(r.Context(), token)
	if err != nil {
		return ConnectorAccess{}, "", err
	}
	return access, token, nil
}

// ReauthorizeConnector checks the original binding and its current access
// before returning a tool result to the authenticated caller.
func (s *Store) ReauthorizeConnector(ctx context.Context, token string, binding ConnectorBinding) error {
	c, err := currentCredential(ctx, s.db, tokenHash(token))
	if err != nil {
		return err
	}
	current, err := currentConnectorAccess(ctx, s.db, c, binding.ProjectID)
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

func (s *Store) connectorAccess(ctx context.Context, token string) (ConnectorAccess, error) {
	// Mode and active grants share one read snapshot. Counting historical bindings
	// separately from filtering active grants prevents implicit legacy fallback.
	rows, err := s.db.QueryContext(ctx, `SELECT b.project_id,b.worker_id,g.can_write,g.can_manage,
		(SELECT count(*) FROM connector_credentials all_bindings WHERE all_bindings.credential_id=c.id)
		FROM credentials c JOIN connector_credentials b ON b.credential_id=c.id
		JOIN grants g ON g.credential_id=b.credential_id AND g.project_id=b.project_id
		JOIN workers w ON w.project_id=b.project_id AND w.id=b.worker_id
		WHERE c.token_sha256=? AND c.revoked=0 AND c.is_admin=0 AND g.can_read=1
		AND NOT EXISTS(SELECT 1 FROM worker_retirements r WHERE r.project_id=w.project_id AND r.worker_id=w.id)
		ORDER BY b.project_id,b.worker_id LIMIT ?`, tokenHash(token), MaxConnectorProjects)
	if err != nil {
		return ConnectorAccess{}, storageError(err)
	}
	defer rows.Close()
	var access ConnectorAccess
	for rows.Next() {
		var binding ConnectorBinding
		var total int
		if err := rows.Scan(&binding.ProjectID, &binding.WorkerID, &binding.write, &binding.manage, &total); err != nil {
			return ConnectorAccess{}, storageError(err)
		}
		access.Bindings = append(access.Bindings, binding)
		access.MultiProject = total > 1
	}
	if err := rows.Err(); err != nil {
		return ConnectorAccess{}, storageError(err)
	}
	if len(access.Bindings) == 0 {
		return ConnectorAccess{}, invalidBearer()
	}
	return access, nil
}
