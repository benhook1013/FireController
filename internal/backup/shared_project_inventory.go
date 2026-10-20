package backup

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

// Frozen declarations from core/project_handles.go and jobs/schema.go. Like
// the existing inventory, inspection never installs or rewrites these tables.
const projectHandleDDL = `CREATE TABLE project_handles(project_id TEXT PRIMARY KEY REFERENCES projects(id),handle TEXT NOT NULL UNIQUE CHECK(length(handle) BETWEEN 1 AND 63 AND handle=lower(handle) AND handle NOT GLOB '*[^a-z0-9-]*' AND substr(handle,1,1)<>'-' AND substr(handle,-1,1)<>'-'))`
const jobSummaryDDL = `CREATE TABLE job_summaries (
 project_id TEXT NOT NULL,
 job_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK (revision >= 1),
 job_revision INTEGER NOT NULL CHECK (job_revision >= 1),
 title TEXT NOT NULL,
 body TEXT NOT NULL,
 actor_id TEXT NOT NULL,
 worker_id TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY (project_id,job_id),
 FOREIGN KEY (project_id,job_id) REFERENCES jobs(project_id,id) ON DELETE CASCADE
)`
const jobSummaryHistoryDDL = `CREATE TABLE job_summary_history (
 project_id TEXT NOT NULL,
 job_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK (revision >= 1),
 job_revision INTEGER NOT NULL CHECK (job_revision >= 1),
 title TEXT NOT NULL,
 body TEXT NOT NULL,
 actor_id TEXT NOT NULL,
 worker_id TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY (project_id,job_id,revision),
 FOREIGN KEY (project_id,job_id) REFERENCES jobs(project_id,id) ON DELETE CASCADE
)`

func validateSharedProjectPhysicalSchema(ctx context.Context, db sqlReader, versions map[string]int) error {
	definitions := map[string]string{}
	if versions["project_handles"] > 0 {
		definitions["project_handles"] = projectHandleDDL
	}
	if versions["jobs"] >= 7 {
		definitions["job_summaries"] = jobSummaryDDL
		definitions["job_summary_history"] = jobSummaryHistoryDDL
	}
	if len(definitions) == 0 {
		return nil
	}
	// No user-defined indexes belong to these three canonical tables. Table SQL
	// binds their exact checks, keys, affinity, collation and foreign-key actions.
	rows, err := db.QueryContext(ctx, `SELECT type,CASE WHEN length(CAST(name AS BLOB))<=128 THEN name ELSE NULL END,CASE WHEN length(CAST(sql AS BLOB))<=4096 THEN sql ELSE NULL END FROM sqlite_schema WHERE tbl_name IN ('project_handles','job_summaries','job_summary_history') AND (type='table' OR (type='index' AND sql IS NOT NULL)) LIMIT 4`)
	if err != nil {
		return invalid()
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var kind, name string
		var definition sql.NullString
		if rows.Scan(&kind, &name, &definition) != nil || kind != "table" || !definition.Valid || seen[name] || definitions[name] == "" || historyDefinitionSQL(definition.String) != definitions[name] {
			return invalid()
		}
		seen[name] = true
	}
	if rows.Err() != nil || len(seen) != len(definitions) {
		return invalid()
	}
	return nil
}

func validateSharedProjectRows(ctx context.Context, db sqlReader, versions map[string]int) error {
	if versions["project_handles"] > 0 {
		if err := validateProjectHandleRows(ctx, db, versions["public_site"] > 0); err != nil {
			return err
		}
	}
	if versions["jobs"] >= 7 {
		return validateJobSummaryRows(ctx, db)
	}
	return nil
}

func validateProjectHandleRows(ctx context.Context, db sqlReader, publications bool) error {
	var bad int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM projects p LEFT JOIN project_handles h ON h.project_id=p.id WHERE h.project_id IS NULL LIMIT 1`).Scan(&bad)
	if !errors.Is(err, sql.ErrNoRows) {
		return invalid()
	}
	rows, err := db.QueryContext(ctx, `SELECT CASE WHEN typeof(project_id)='text' AND length(CAST(project_id AS BLOB))<=128 THEN project_id END,CASE WHEN typeof(handle)='text' AND length(CAST(handle AS BLOB))<=63 THEN handle END FROM project_handles`)
	if err != nil {
		return invalid()
	}
	for rows.Next() {
		var project, handle string
		if rows.Scan(&project, &handle) != nil || !summaryIdentity(project) || !core.ValidProjectHandle(handle) {
			rows.Close()
			return invalid()
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return invalid()
	}
	err = db.QueryRowContext(ctx, `SELECT 1 FROM project_handles h JOIN projects p ON p.id=h.handle WHERE p.id<>h.project_id LIMIT 1`).Scan(&bad)
	if !errors.Is(err, sql.ErrNoRows) {
		return invalid()
	}
	// The DDL's unique BINARY key prevents duplicate handles. Its relation to a
	// separately published slug is also part of the shared address namespace.
	if publications {
		err = db.QueryRowContext(ctx, `SELECT 1 FROM project_handles h JOIN project_publications p ON p.slug=h.handle WHERE p.project_id<>h.project_id LIMIT 1`).Scan(&bad)
		if !errors.Is(err, sql.ErrNoRows) {
			return invalid()
		}
	}
	return nil
}

// Keep the owning jobs' accepted identifier/text/time semantics; do not repair
// source strings or require clocks to move monotonically between revisions.
func summaryIdentity(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value) && value != "." && value != ".." && !strings.ContainsAny(value, "/\\%?#") && strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}
func validRetainedSummary(title, body string) bool {
	if title == "" && body == "" {
		return true
	}
	if strings.TrimSpace(title) == "" || strings.TrimSpace(body) == "" || len(title) > 240 || len(body) > 2000 || !utf8.ValidString(title) || !utf8.ValidString(body) {
		return false
	}
	if strings.IndexFunc(title, unicode.IsControl) >= 0 {
		return false
	}
	for _, r := range body {
		if r < 0x20 && r != '\t' && r != '\r' && r != '\n' || r == 0x7f {
			return false
		}
	}
	return true
}

// SQL bounds each field before scanning so resealed oversized SQLite values
// never allocate unbounded Go strings. Stream indexed history, one job at a time.
const boundedSummaryColumns = `CASE WHEN typeof(project_id)='text' AND length(CAST(project_id AS BLOB))<=128 THEN project_id END,
 CASE WHEN typeof(job_id)='text' AND length(CAST(job_id AS BLOB))<=128 THEN job_id END,
 CASE WHEN typeof(revision)='integer' THEN revision END,
 CASE WHEN typeof(job_revision)='integer' THEN job_revision END,
 CASE WHEN typeof(title)='text' AND length(CAST(title AS BLOB))<=240 THEN title END,
 CASE WHEN typeof(body)='text' AND length(CAST(body AS BLOB))<=2000 THEN body END,
 CASE WHEN typeof(actor_id)='text' AND length(CAST(actor_id AS BLOB))<=128 THEN actor_id END,
 CASE WHEN typeof(worker_id)='text' AND length(CAST(worker_id AS BLOB))<=128 THEN worker_id END,
 CASE WHEN typeof(created_at)='text' AND length(CAST(created_at AS BLOB))<=64 THEN created_at END`

type retainedSummary struct {
	project, job                        string
	revision, jobRevision               int64
	title, body, actor, worker, created string
}

func validateJobSummaryRows(ctx context.Context, db sqlReader) error {
	// Current rows must equal every retained field of their latest revision.
	var bad int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM job_summaries c WHERE NOT EXISTS(SELECT 1 FROM job_summary_history h WHERE h.project_id=c.project_id AND h.job_id=c.job_id AND h.revision=c.revision AND h.job_revision=c.job_revision AND h.title=c.title AND h.body=c.body AND h.actor_id=c.actor_id AND h.worker_id=c.worker_id AND h.created_at=c.created_at) OR EXISTS(SELECT 1 FROM job_summary_history h WHERE h.project_id=c.project_id AND h.job_id=c.job_id AND h.revision>c.revision) LIMIT 1`).Scan(&bad)
	if !errors.Is(err, sql.ErrNoRows) {
		return invalid()
	}
	err = db.QueryRowContext(ctx, `SELECT 1 FROM job_summary_history h WHERE NOT EXISTS(SELECT 1 FROM job_summaries c WHERE c.project_id=h.project_id AND c.job_id=h.job_id) OR NOT EXISTS(SELECT 1 FROM job_history j WHERE j.project_id=h.project_id AND j.job_id=h.job_id AND j.revision=h.job_revision) OR NOT EXISTS(SELECT 1 FROM jobs j WHERE j.project_id=h.project_id AND j.id=h.job_id AND j.revision>=h.job_revision) LIMIT 1`).Scan(&bad)
	if !errors.Is(err, sql.ErrNoRows) {
		return invalid()
	}
	for _, table := range []string{"job_summaries", "job_summary_history"} {
		rows, err := db.QueryContext(ctx, `SELECT `+boundedSummaryColumns+` FROM `+table+` ORDER BY project_id,job_id,revision`)
		if err != nil {
			return invalid()
		}
		var previous retainedSummary
		for rows.Next() {
			var row retainedSummary
			if rows.Scan(&row.project, &row.job, &row.revision, &row.jobRevision, &row.title, &row.body, &row.actor, &row.worker, &row.created) != nil || !summaryIdentity(row.project) || !summaryIdentity(row.job) || row.revision < 1 || row.jobRevision < 1 || !validRetainedSummary(row.title, row.body) || !summaryIdentity(row.actor) || row.worker != "" && !summaryIdentity(row.worker) {
				rows.Close()
				return invalid()
			}
			at, err := time.Parse(time.RFC3339Nano, row.created)
			if err != nil || at.IsZero() {
				rows.Close()
				return invalid()
			}
			if table == "job_summary_history" {
				if previous.project == row.project && previous.job == row.job {
					if row.revision-1 != previous.revision {
						rows.Close()
						return invalid()
					}
				} else if row.revision != 1 {
					rows.Close()
					return invalid()
				}
			}
			previous = row
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return invalid()
		}
	}
	return nil
}
