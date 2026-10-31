package adoptionholds

import (
	"context"
	"database/sql"
	"errors"
	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/strictjson"
	"strings"
)

func domainPresent(ctx context.Context, q Query) (bool, error) {
	var version any
	err := q.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_adoption_obligations'`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		var count int
		if err = q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE name GLOB 'review_adoption_*'`).Scan(&count); err != nil {
			return false, err
		}
		if count != 0 {
			return false, unavailable()
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if version != "1" {
		return false, unavailable()
	}
	var tables int
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name IN ('review_adoption_sources','review_adoption_imports','review_adoption_obligations','review_adoption_targets','review_adoption_resolutions')`).Scan(&tables); err != nil {
		return false, err
	}
	if tables != 5 {
		return false, unavailable()
	}
	return true, nil
}

// ValidateSnapshot checks this optional domain's physical and logical graph.
// Runtime scoped reads never invoke this repository-wide maintenance traversal.
func ValidateSnapshot(ctx context.Context, q Query) error {
	var version any
	err := q.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_adoption_obligations'`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		var count int
		if err = q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE name GLOB 'review_adoption_*'`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return unavailable()
		}
		return nil
	}
	if err != nil {
		return err
	}
	if version != "1" {
		return unavailable()
	}
	expected := map[string]string{}
	tables := []string{}
	for _, definition := range SchemaSQL() {
		fields := strings.Fields(definition)
		name := strings.Split(fields[2], "(")[0]
		expected[name] = strings.TrimSuffix(strings.TrimSpace(definition), ";")
		if fields[1] == "TABLE" {
			tables = append(tables, name)
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT name,CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql END FROM sqlite_schema WHERE (name GLOB 'review_adoption_*' OR tbl_name IN ('review_adoption_sources','review_adoption_imports','review_adoption_obligations','review_adoption_targets','review_adoption_resolutions')) AND sql IS NOT NULL`)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for rows.Next() {
		var name, definition string
		if err = rows.Scan(&name, &definition); err != nil {
			break
		}
		if expected[name] != strings.TrimSuffix(strings.TrimSpace(definition), ";") {
			rows.Close()
			return unavailable()
		}
		seen[name] = true
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return unavailable()
	}
	if len(seen) != len(expected) {
		return unavailable()
	}
	for _, table := range tables {
		rows, err := q.QueryContext(ctx, `PRAGMA foreign_key_check("`+table+`")`)
		if err != nil {
			return err
		}
		bad := rows.Next()
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if bad {
			return unavailable()
		}
	}
	checks := []string{
		`SELECT EXISTS(SELECT 1 FROM review_adoption_sources WHERE typeof(project_id)!='text' OR project_id='' OR length(CAST(project_id AS BLOB))>128 OR typeof(namespace)!='text' OR namespace='' OR length(CAST(namespace AS BLOB))>200 OR typeof(repository_id)!='integer' OR typeof(snapshot_sha256)!='text' OR typeof(revision)!='integer')`,
		`SELECT EXISTS(SELECT 1 FROM review_adoption_imports WHERE typeof(project_id)!='text' OR project_id='' OR typeof(namespace)!='text' OR namespace='' OR typeof(revision)!='integer' OR typeof(batch_json)!='text' OR length(CAST(batch_json AS BLOB))>262144 OR typeof(batch_sha256)!='text' OR length(CAST(batch_sha256 AS BLOB))!=64 OR typeof(actor_id)!='text' OR typeof(worker_id)!='text' OR typeof(created_at)!='text')`,
		`SELECT EXISTS(SELECT 1 FROM review_adoption_obligations WHERE typeof(id)!='text' OR id='' OR length(CAST(id AS BLOB))>128 OR typeof(project_id)!='text' OR typeof(namespace)!='text' OR typeof(source_key)!='text' OR length(CAST(source_key AS BLOB))>200 OR typeof(source_revision)!='integer' OR typeof(original_json)!='text' OR length(CAST(original_json AS BLOB))>65536 OR typeof(original_sha256)!='text' OR length(CAST(original_sha256 AS BLOB))!=64 OR typeof(status)!='text' OR typeof(revision)!='integer' OR typeof(actor_id)!='text' OR typeof(worker_id)!='text' OR typeof(created_at)!='text')`,
		`SELECT EXISTS(SELECT 1 FROM review_adoption_targets WHERE typeof(obligation_id)!='text' OR typeof(project_id)!='text' OR typeof(pull_number)!='integer')`,
		`SELECT EXISTS(SELECT 1 FROM review_adoption_resolutions WHERE typeof(obligation_id)!='text' OR typeof(revision)!='integer' OR typeof(reason)!='text' OR length(CAST(reason AS BLOB))>4096 OR typeof(evidence_json)!='text' OR length(CAST(evidence_json AS BLOB))>16384 OR typeof(actor_id)!='text' OR typeof(worker_id)!='text' OR typeof(resolved_at)!='text')`,
	}
	for _, query := range checks {
		var bad bool
		if err = q.QueryRowContext(ctx, query).Scan(&bad); err != nil {
			return err
		}
		if bad {
			return unavailable()
		}
	}
	project, namespace := "", ""
	for {
		rows, err := q.QueryContext(ctx, `SELECT project_id,namespace FROM review_adoption_sources WHERE (project_id,namespace)>(?,?) ORDER BY project_id,namespace LIMIT 64`, project, namespace)
		if err != nil {
			return err
		}
		pairs := [][2]string{}
		for rows.Next() {
			var p, n string
			if err = rows.Scan(&p, &n); err != nil {
				break
			}
			pairs = append(pairs, [2]string{p, n})
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, pair := range pairs {
			if err = validateSource(ctx, q, pair[0], pair[1]); err != nil {
				return err
			}
			project, namespace = pair[0], pair[1]
		}
		if len(pairs) < 64 {
			break
		}
	}
	after := ""
	for {
		rows, err := q.QueryContext(ctx, `SELECT id,project_id FROM review_adoption_obligations WHERE id>? ORDER BY id LIMIT 64`, after)
		if err != nil {
			return err
		}
		pairs := [][2]string{}
		for rows.Next() {
			var id, p string
			if err = rows.Scan(&id, &p); err != nil {
				break
			}
			pairs = append(pairs, [2]string{id, p})
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, pair := range pairs {
			if _, err = readDetail(ctx, q, pair[1], pair[0]); err != nil {
				return unavailable()
			}
			after = pair[0]
		}
		if len(pairs) < 64 {
			break
		}
	}
	return nil
}
func validateSource(ctx context.Context, q Query, project, namespace string) error {
	source, err := ReadSource(ctx, q, project, namespace)
	if err != nil {
		return unavailable()
	}
	for revision := int64(1); revision <= source.Revision; revision++ {
		var body, sha, actor, worker, at string
		if err = q.QueryRowContext(ctx, `SELECT batch_json,batch_sha256,actor_id,worker_id,created_at FROM review_adoption_imports WHERE project_id=? AND namespace=? AND revision=?`, project, namespace, revision).Scan(&body, &sha, &actor, &worker, &at); err != nil {
			return unavailable()
		}
		var batch m.Batch
		if strictjson.Decode([]byte(body), &batch) != nil || digest([]byte(body)) != sha || !identifier.MatchString(actor) || !identifier.MatchString(worker) || !validTime(at) {
			return unavailable()
		}
		normalized, canonical, err := NormalizeBatch(batch)
		if err != nil || string(canonical) != body || normalized.Source != source.Source || *normalized.ExpectedRevision != revision-1 {
			return unavailable()
		}
		introduced := int64(0)
		for _, item := range normalized.Obligations {
			var id string
			if err = q.QueryRowContext(ctx, `SELECT id FROM review_adoption_obligations WHERE project_id=? AND namespace=? AND source_key=?`, project, namespace, item.SourceKey).Scan(&id); err != nil {
				return unavailable()
			}
			d, err := readDetail(ctx, q, project, id)
			if err != nil || d.SourceRevision > revision || string(encode(d.Original)) != string(encode(item)) {
				return unavailable()
			}
			if d.SourceRevision == revision {
				introduced++
				if d.ActorID != actor || d.WorkerID != worker || d.CreatedAt != at {
					return unavailable()
				}
			}
		}
		var count int64
		if err = q.QueryRowContext(ctx, `SELECT count(*) FROM review_adoption_obligations WHERE project_id=? AND namespace=? AND source_revision=?`, project, namespace, revision).Scan(&count); err != nil {
			return err
		}
		if introduced == 0 || introduced != count {
			return unavailable()
		}
	}
	var count int64
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM review_adoption_imports WHERE project_id=? AND namespace=?`, project, namespace).Scan(&count); err != nil {
		return err
	}
	if count != source.Revision {
		return unavailable()
	}
	return nil
}
