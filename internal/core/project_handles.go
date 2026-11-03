package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const maxProjectHandleBytes = 63

var projectHandleSchema = [][]string{{
	`CREATE TABLE project_handles(project_id TEXT PRIMARY KEY REFERENCES projects(id),handle TEXT NOT NULL UNIQUE CHECK(length(handle) BETWEEN 1 AND 63 AND handle=lower(handle) AND handle NOT GLOB '*[^a-z0-9-]*' AND substr(handle,1,1)<>'-' AND substr(handle,-1,1)<>'-'))`,
}}

func (s *Store) ensureProjectHandles(ctx context.Context) error {
	return s.EnsureSchemaWithBackfill(ctx, "project_handles", projectHandleSchema, backfillProjectHandles)
}

func backfillProjectHandles(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `SELECT id,name FROM projects WHERE id NOT IN (SELECT project_id FROM project_handles) ORDER BY id`)
	if err != nil {
		return err
	}
	type project struct{ id, name string }
	var projects []project
	for rows.Next() {
		var value project
		if err = rows.Scan(&value.id, &value.name); err != nil {
			rows.Close()
			return err
		}
		projects = append(projects, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, value := range projects {
		if _, err = assignProjectHandle(ctx, conn, value.id, value.name, ""); err != nil {
			return err
		}
	}
	return nil
}

// ValidProjectHandle is the public syntax shared by project creation and URL
// consumers. Handles are canonical lowercase ASCII path segments.
func ValidProjectHandle(value string) bool {
	if value == "" || len(value) > maxProjectHandleBytes || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, c := range []byte(value) {
		if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func derivedProjectHandle(name, projectID string) string {
	var b strings.Builder
	separator := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if separator && b.Len() > 0 && b.Len() < maxProjectHandleBytes {
				b.WriteByte('-')
			}
			separator = false
			if b.Len() < maxProjectHandleBytes {
				b.WriteByte(byte(r))
			}
		} else if b.Len() > 0 {
			separator = true
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "project-" + stableHandleSuffix(projectID, 0)
	}
	return strings.TrimRight(base, "-")
}

func stableHandleSuffix(projectID string, attempt int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("firecontroller/project-handle/v1\x00%s\x00%d", projectID, attempt)))
	return fmt.Sprintf("%x", sum[:4])
}

func suffixedProjectHandle(base, projectID string, attempt int) string {
	suffix := "-" + stableHandleSuffix(projectID, attempt)
	if len(base)+len(suffix) > maxProjectHandleBytes {
		base = strings.TrimRight(base[:maxProjectHandleBytes-len(suffix)], "-")
	}
	return base + suffix
}

// AssignProjectHandle creates the immutable initial handle in the caller's
// project-creation transaction. An explicit collision is never rewritten.
func AssignProjectHandle(ctx context.Context, tx *sql.Tx, projectID, name, explicit string) (string, error) {
	return assignProjectHandle(ctx, tx, projectID, name, explicit)
}

type projectHandleQuerier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func assignProjectHandle(ctx context.Context, q projectHandleQuerier, projectID, name, explicit string) (string, error) {
	if explicit != "" {
		if !ValidProjectHandle(explicit) {
			return "", Fail("validation_failed", "Provide a lowercase project handle using letters, numbers, and interior hyphens.", 400)
		}
		if assigned, err := insertProjectHandle(ctx, q, projectID, explicit); err != nil || !assigned {
			if err == nil || handleConstraint(err) {
				return "", Fail("project_handle_conflict", "This project handle is already in use.", 409)
			}
			return "", err
		}
		return explicit, nil
	}
	base := derivedProjectHandle(name, projectID)
	for attempt := -1; attempt < 1024; attempt++ {
		handle := base
		if attempt >= 0 {
			handle = suffixedProjectHandle(base, projectID, attempt)
		}
		if assigned, err := insertProjectHandle(ctx, q, projectID, handle); err == nil && assigned {
			return handle, nil
		} else if err != nil && !handleConstraint(err) {
			return "", err
		}
	}
	return "", Fail("project_handle_conflict", "A unique project handle could not be allocated.", 409)
}

func insertProjectHandle(ctx context.Context, q projectHandleQuerier, projectID, handle string) (bool, error) {
	available, err := publicationSlugAvailable(ctx, q, projectID, handle)
	if err != nil || !available {
		return false, err
	}
	result, err := q.ExecContext(ctx, `INSERT INTO project_handles(project_id,handle) SELECT ?,? WHERE NOT EXISTS(SELECT 1 FROM projects WHERE id=? AND id<>?)`, projectID, handle, handle, projectID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func publicationSlugAvailable(ctx context.Context, q projectHandleQuerier, projectID, handle string) (bool, error) {
	var installed int
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='project_publications')`).Scan(&installed); err != nil {
		return false, err
	}
	if installed == 0 {
		return true, nil
	}
	var owner string
	err := q.QueryRowContext(ctx, `SELECT project_id FROM project_publications WHERE slug=? AND project_id<>? LIMIT 1`, handle, projectID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return false, err
}

func handleConstraint(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code()&255 == 19
}

// ResolveProjectID resolves an opaque ID first, then a handle, then an enabled
// publication slug. It performs no authorization and must only be called after
// credential authentication.
func (s *Store) ResolveProjectID(ctx context.Context, reference string) (string, error) {
	if reference == "" {
		return "", nil
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM projects WHERE id=?`, reference).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", storageError(err)
	}
	err = s.db.QueryRowContext(ctx, `SELECT project_id FROM project_handles WHERE handle=?`, reference).Scan(&id)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", storageError(err)
		}
	} else {
		return id, nil
	}
	var installed int
	if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='project_publications')`).Scan(&installed); err != nil {
		return "", storageError(err)
	}
	if installed == 0 {
		return reference, nil
	}
	err = s.db.QueryRowContext(ctx, `SELECT project_id FROM project_publications WHERE slug=? AND enabled=1`, reference).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return reference, nil
	}
	if err != nil {
		return "", storageError(err)
	}
	return id, nil
}

// ProjectHandle reads the canonical handle for an already-authorized project.
func (s *Store) ProjectHandle(ctx context.Context, projectID string) (string, error) {
	var handle string
	if err := s.db.QueryRowContext(ctx, `SELECT handle FROM project_handles WHERE project_id=?`, projectID).Scan(&handle); err != nil {
		return "", storageError(err)
	}
	return handle, nil
}
