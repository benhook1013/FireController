package coordimport

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type SnapshotReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ValidateSnapshot checks provenance and original native rows, not current
// equality: normal later edits must remain valid and must survive restore.
func ValidateSnapshot(ctx context.Context, db SnapshotReader) error {
	if err := ValidatePhysicalSchema(ctx, db); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT CASE WHEN length(CAST(project_id AS BLOB))<=128 THEN project_id END,revision FROM coordination_import_state`)
	if err != nil {
		return err
	}
	state := map[string]int64{}
	for rows.Next() {
		var project string
		var revision int64
		if err = rows.Scan(&project, &revision); err != nil {
			break
		}
		if revision < 1 {
			err = conflict()
			break
		}
		state[project] = revision
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for project, head := range state {
		var count, maximum int64
		if err = db.QueryRowContext(ctx, `SELECT count(*),coalesce(max(revision),0) FROM coordination_import_batches WHERE project_id=?`, project).Scan(&count, &maximum); err != nil {
			return err
		}
		if count != head || maximum != head {
			return conflict()
		}
	}
	rows, err = db.QueryContext(ctx, `SELECT CASE WHEN length(CAST(project_id AS BLOB))<=128 THEN project_id END,revision,CASE WHEN length(CAST(source AS BLOB))<=200 THEN source END,CASE WHEN length(source_sha256)=64 THEN source_sha256 END,CASE WHEN length(payload_sha256)=64 THEN payload_sha256 END,CASE WHEN length(CAST(actor_id AS BLOB))<=128 THEN actor_id END,CASE WHEN length(CAST(worker_id AS BLOB))<=128 THEN worker_id END,CASE WHEN length(CAST(created_at AS BLOB))<=64 THEN created_at END FROM coordination_import_batches`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var project, source, sourceHash, payloadHash, actor, worker, at string
		var revision int64
		if err = rows.Scan(&project, &revision, &source, &sourceHash, &payloadHash, &actor, &worker, &at); err != nil {
			break
		}
		parsed, e := time.Parse(time.RFC3339Nano, at)
		if state[project] < revision || revision < 1 || source == "" || len(source) > 200 || strings.TrimSpace(source) != source || !validHash(sourceHash) || !validHash(payloadHash) || actor == "" || worker == "" || e != nil || parsed.IsZero() {
			err = conflict()
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	// Page provenance before nested original-row checks. Memory stays bounded
	// independently of retained import history, including on one SQL connection.
	type retained struct {
		project, source, key, id, hash, sourceHash, body, at string
		revision                                             int64
	}
	lastProject, lastSource, lastKey := "", "", ""
	for {
		rows, err = db.QueryContext(ctx, `SELECT CASE WHEN length(CAST(project_id AS BLOB))<=128 THEN project_id END,CASE WHEN length(CAST(source AS BLOB))<=200 THEN source END,CASE WHEN length(CAST(source_key AS BLOB))<=200 THEN source_key END,CASE WHEN length(CAST(job_id AS BLOB))<=128 THEN job_id END,import_revision,CASE WHEN length(item_sha256)=64 THEN item_sha256 END,CASE WHEN length(source_sha256)=64 THEN source_sha256 END,CASE WHEN length(CAST(item_json AS BLOB))<=262144 THEN item_json END,CASE WHEN length(CAST(created_at AS BLOB))<=64 THEN created_at END FROM coordination_import_records WHERE (project_id,source,source_key)>(?,?,?) ORDER BY project_id,source,source_key LIMIT 25`, lastProject, lastSource, lastKey)
		if err != nil {
			return err
		}
		items := make([]retained, 0, 25)
		for rows.Next() {
			var v retained
			if err = rows.Scan(&v.project, &v.source, &v.key, &v.id, &v.revision, &v.hash, &v.sourceHash, &v.body, &v.at); err != nil {
				break
			}
			items = append(items, v)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, v := range items {
			if state[v.project] < 1 || v.revision < 1 || !validHash(v.sourceHash) || digest([]byte(v.body)) != v.hash || resourceID(v.project, v.source, "job", "", v.key) != v.id {
				return conflict()
			}
			var item jobs.PreparedJob
			if strictjson.Decode([]byte(v.body), &item) != nil || item.SourceKey != v.key {
				return conflict()
			}
			normalized, e := jobs.NormalizePreparedJob(item)
			if e != nil {
				return e
			}
			body, _ := json.Marshal(normalized)
			if string(body) != v.body {
				return conflict()
			}
			ids := make([]string, len(item.Checkpoints))
			for i, cp := range item.Checkpoints {
				ids[i] = resourceID(v.project, v.source, "checkpoint", item.SourceKey, cp.SourceKey)
			}
			if err = jobs.ValidatePreparedSnapshot(ctx, db, v.project, v.id, item, ids); err != nil {
				return err
			}
			var original bool
			if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM coordination_import_batches WHERE project_id=? AND revision=? AND source=? AND source_sha256=? AND created_at=?)`, v.project, v.revision, v.source, v.sourceHash, v.at).Scan(&original); err != nil {
				return err
			}
			if !original {
				return conflict()
			}
			lastProject, lastSource, lastKey = v.project, v.source, v.key
		}
		if len(items) < 25 {
			break
		}
	}
	var emptyBatch bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM coordination_import_batches b WHERE NOT EXISTS(SELECT 1 FROM coordination_import_records r WHERE r.project_id=b.project_id AND r.import_revision=b.revision))`).Scan(&emptyBatch); err != nil {
		return err
	}
	if emptyBatch {
		return conflict()
	}
	return nil
}
