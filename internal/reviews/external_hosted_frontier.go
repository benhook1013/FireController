package reviews

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const externalHostedSource = "github-hosted-observer"

// ExternalHostedSchemaSQL is the schema-ten automatic-discovery boundary.
// Frontiers survive current membership removal and never advance automatically.
func ExternalHostedSchemaSQL() []string {
	return []string{
		`CREATE TABLE review_hosted_discovery_state (
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 repository_id INTEGER NOT NULL CHECK(repository_id>0),
 pull_number INTEGER NOT NULL CHECK(pull_number>0 AND pull_number<=2147483647),
 cutoff_at TEXT NOT NULL,
 initialized_at TEXT NOT NULL,
 PRIMARY KEY(project_id,pull_number)
);`,
		`CREATE INDEX review_run_versions_external_source ON review_run_versions(project_id,source_run_id);`,
	}
}

func externalHostedVisible(ctx context.Context, tx *sql.Tx, project string, repositoryID, number int64) (bool, error) {
	if !validText(project, 128, true) || repositoryID <= 0 || number < 1 || number > 2147483647 {
		return false, validationError("Use a bound external hosted project, repository and pull.")
	}
	var actual int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&actual); err != nil {
		return false, err
	}
	if actual != repositoryID {
		return false, transferConflict("The external hosted repository binding changed.")
	}
	var visible bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_pulls p WHERE p.project_id=? AND p.number=? AND NOT EXISTS(SELECT 1 FROM review_current_retired r WHERE r.project_id=p.project_id AND r.number=p.number))`, project, number).Scan(&visible)
	return visible, err
}
func validExternalHostedTime(value string, optional bool) bool {
	if value == "" {
		return optional
	}
	t, err := parseTimestamp(value, true)
	return err == nil && !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 && t.UTC().Format(time.RFC3339Nano) == value
}
func readExternalHostedFrontier(ctx context.Context, q HistoryQuery, project string, repositoryID, number int64) (string, error) {
	var repo any
	var cutoff, initialized any
	if err := q.QueryRowContext(ctx, `SELECT repository_id,CASE WHEN length(CAST(cutoff_at AS BLOB))<=64 THEN cutoff_at END,CASE WHEN length(CAST(initialized_at AS BLOB))<=64 THEN initialized_at END FROM review_hosted_discovery_state WHERE project_id=? AND pull_number=?`, project, number).Scan(&repo, &cutoff, &initialized); err != nil {
		return "", err
	}
	storedRepo, ok := repo.(int64)
	if !ok || storedRepo <= 0 {
		return "", historyInconsistent()
	}
	cut, ok := cutoff.(string)
	if !ok || !validExternalHostedTime(cut, true) {
		return "", historyInconsistent()
	}
	init, ok := initialized.(string)
	if !ok || !validExternalHostedTime(init, false) {
		return "", historyInconsistent()
	}
	if storedRepo != repositoryID {
		return "", transferConflict("The retained hosted discovery frontier belongs to another repository.")
	}
	return cut, nil
}

// InitializeExternalHostedFrontier must commit before provider collection. It
// freezes the latest retained non-observer hosted finish once, not a moving
// high-water mark. The caller owns configured enrollment and provider binding.
func InitializeExternalHostedFrontier(ctx context.Context, tx *sql.Tx, project string, repositoryID, number int64) (string, error) {
	visible, err := externalHostedVisible(ctx, tx, project, repositoryID, number)
	if err != nil {
		return "", err
	}
	if !visible {
		return "", transferConflict("External hosted discovery requires an existing visible pull.")
	}
	cutoff, err := readExternalHostedFrontier(ctx, tx, project, repositoryID, number)
	if err == nil {
		return cutoff, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	rows, err := tx.QueryContext(ctx, `SELECT v.finished_at
 FROM review_run_memberships m INDEXED BY review_run_memberships_page
 CROSS JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision AND v.pull_number=m.pull_number
 CROSS JOIN review_snapshots s ON s.project_id=v.project_id AND s.revision=v.version_revision
 WHERE m.project_id=? AND m.pull_number=? AND v.channel='hosted' AND v.source!=? AND s.repository_id=?`, project, number, externalHostedSource, repositoryID)
	if err != nil {
		return "", err
	}
	var latest time.Time
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var at time.Time
		at, err = parseTimestamp(raw, false)
		if err != nil {
			break
		}
		if at.After(latest) {
			latest = at
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return "", err
	}
	if !latest.IsZero() {
		cutoff = latest.UTC().Format(time.RFC3339Nano)
		if !validExternalHostedTime(cutoff, false) {
			return "", validationError("Retained Hosted completion exceeds the supported discovery timestamp range.")
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO review_hosted_discovery_state(project_id,repository_id,pull_number,cutoff_at,initialized_at) VALUES(?,?,?,?,?)`, project, repositoryID, number, cutoff, time.Now().UTC().Format(time.RFC3339Nano))
	return cutoff, err
}

// ValidateExternalHostedFrontiers is a read-only backup maintenance check. A
// historical repository binding remains retained even if a project is rebound;
// discovery refuses that mismatch instead of silently resetting its frontier.
func ValidateExternalHostedFrontiers(ctx context.Context, q HistoryQuery) error {
	var bad bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_hosted_discovery_state f LEFT JOIN projects p ON p.id=f.project_id WHERE p.id IS NULL OR typeof(f.project_id)!='text' OR typeof(f.repository_id)!='integer' OR typeof(f.pull_number)!='integer' OR typeof(f.cutoff_at)!='text' OR typeof(f.initialized_at)!='text' OR length(CAST(f.project_id AS BLOB))>128 OR length(CAST(f.cutoff_at AS BLOB))>64 OR length(CAST(f.initialized_at AS BLOB))>64 OR f.repository_id<=0 OR f.pull_number<1 OR f.pull_number>2147483647)`).Scan(&bad)
	if err != nil {
		return err
	}
	if bad {
		return historyInconsistent()
	}
	rows, err := q.QueryContext(ctx, `SELECT project_id,cutoff_at,initialized_at FROM review_hosted_discovery_state`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var project, cutoff, initialized string
		if err = rows.Scan(&project, &cutoff, &initialized); err != nil {
			return err
		}
		if !validText(project, 128, true) || !validExternalHostedTime(cutoff, true) || !validExternalHostedTime(initialized, false) {
			return historyInconsistent()
		}
	}
	return rows.Err()
}
