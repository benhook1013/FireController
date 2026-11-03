package jobs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"time"
)

type legacyProgressKey struct {
	time string
	kind int
	id   string
}

func conversionID(project, job string, kind int, id string) string {
	sum := sha256.Sum256([]byte(project + "\x00" + job + "\x00" + strconv.Itoa(kind) + "\x00" + id))
	return "job_progress_" + hex.EncodeToString(sum[:16])
}

// Startup stages only sort keys on SQLite's temporary store. Source bodies are
// read one at a time; neither a job's lifetime archive nor all jobs enter RAM.
func migrateProgress(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `CREATE TEMP TABLE job_progress_conversion(time_key TEXT NOT NULL,kind INTEGER NOT NULL,id TEXT NOT NULL,PRIMARY KEY(time_key,kind,id)) WITHOUT ROWID`); err != nil {
		return err
	}
	project, id := "", ""
	for {
		rows, err := conn.QueryContext(ctx, `SELECT project_id,id,revision FROM jobs WHERE (project_id,id)>(?,?) ORDER BY project_id,id LIMIT 64`, project, id)
		if err != nil {
			return err
		}
		var batch []job
		for rows.Next() {
			var j job
			if err = rows.Scan(&j.ProjectID, &j.ID, &j.Revision); err != nil {
				break
			}
			batch = append(batch, j)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, j := range batch {
			if !validPrimaryID(j.ProjectID) || !validPrimaryID(j.ID) || j.Revision < 1 {
				return progressUnavailable()
			}
			if err := seedEmptyProgress(ctx, conn, j); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM job_progress_conversion`); err != nil {
				return err
			}
			for kind, table := range []string{"job_checkpoints", "job_updates"} {
				var after int64
				for {
					rows, err := conn.QueryContext(ctx, `SELECT sequence,id,created_at FROM `+table+` WHERE project_id=? AND job_id=? AND sequence>? ORDER BY sequence LIMIT 64`, j.ProjectID, j.ID, after)
					if err != nil {
						return err
					}
					type key struct {
						sequence int64
						id, at   string
					}
					var keys []key
					for rows.Next() {
						var k key
						if err = rows.Scan(&k.sequence, &k.id, &k.at); err != nil {
							break
						}
						keys = append(keys, k)
					}
					if err == nil {
						err = rows.Err()
					}
					rows.Close()
					if err != nil {
						return err
					}
					for _, k := range keys {
						at, err := time.Parse(time.RFC3339Nano, k.at)
						if err != nil || !contextTime(k.at) || !validPrimaryID(k.id) || k.sequence <= after {
							return progressUnavailable()
						}
						if _, err := conn.ExecContext(ctx, `INSERT INTO job_progress_conversion VALUES(?,?,?)`, at.UTC().Format(noteTimeFormat), kind, k.id); err != nil {
							return err
						}
						after = k.sequence
					}
					if len(keys) < 64 {
						break
					}
				}
			}
			current := emptyProgress()
			after := legacyProgressKey{kind: -1}
			for {
				rows, err := conn.QueryContext(ctx, `SELECT time_key,kind,id FROM job_progress_conversion WHERE (time_key,kind,id)>(?,?,?) ORDER BY time_key,kind,id LIMIT 64`, after.time, after.kind, after.id)
				if err != nil {
					return err
				}
				var keys []legacyProgressKey
				for rows.Next() {
					var k legacyProgressKey
					if err = rows.Scan(&k.time, &k.kind, &k.id); err != nil {
						break
					}
					keys = append(keys, k)
				}
				if err == nil {
					err = rows.Err()
				}
				rows.Close()
				if err != nil {
					return err
				}
				for _, k := range keys {
					current, err = convertLegacyProgress(ctx, conn, j, current, k.kind, k.id)
					if err != nil {
						return err
					}
					after = k
				}
				if len(keys) < 64 {
					break
				}
			}
			project, id = j.ProjectID, j.ID
		}
		if len(batch) < 64 {
			break
		}
	}
	_, err := conn.ExecContext(ctx, `DROP TABLE job_progress_conversion`)
	return err
}

func convertLegacyProgress(ctx context.Context, q progressWriter, j job, current progress, kind int, id string) (progress, error) {
	var actor, worker, at string
	var revision int64
	if kind == 0 {
		row := q.QueryRowContext(ctx, `SELECT sequence,id,project_id,job_id,job_revision,actor_id,worker_id,CASE WHEN length(CAST(done AS BLOB))<=8192 THEN done END,CASE WHEN length(CAST(next AS BLOB))<=8192 THEN next END,CASE WHEN length(CAST(blocker AS BLOB))<=4096 THEN blocker END,CASE WHEN length(CAST(evidence_json AS BLOB))<=65536 THEN evidence_json END,created_at FROM job_checkpoints WHERE project_id=? AND job_id=? AND id=?`, j.ProjectID, j.ID, id)
		cp, err := scanCheckpoint(row)
		if err != nil {
			return current, err
		}
		if !validContextCheckpoint(cp, j) {
			return current, progressUnavailable()
		}
		current = progress{Summary: cp.Done, Next: cp.Next, Blocker: cp.Blocker, Evidence: cp.Evidence}
		revision, actor, worker, at = cp.JobRevision, cp.ActorID, cp.WorkerID, cp.CreatedAt
	} else {
		var body, refs string
		err := q.QueryRowContext(ctx, `SELECT job_revision,actor_id,worker_id,CASE WHEN length(CAST(body AS BLOB))<=8192 THEN body END,CASE WHEN length(CAST(evidence_json AS BLOB))<=65536 THEN evidence_json END,created_at FROM job_updates WHERE project_id=? AND job_id=? AND id=?`, j.ProjectID, j.ID, id).Scan(&revision, &actor, &worker, &body, &refs, &at)
		if err != nil {
			return current, err
		}
		var evidence []string
		if json.Unmarshal([]byte(refs), &evidence) != nil || evidence == nil || validateUpdate(updateRequest{ExpectedJobRevision: revision, Body: body, Evidence: evidence}) != nil {
			return current, progressUnavailable()
		}
		current.Summary, current.Evidence = body, evidence
		// Valid legacy records can exceed the new aggregate when combined. Keep
		// the whole newest narrative; earlier next/blocker survive in history/source.
		if !validProgress(current) {
			current = progress{Summary: body, Evidence: evidence}
		}
	}
	if !validPrimaryID(actor) || worker != "" && !validPrimaryID(worker) || !contextTime(at) || revision < 1 || revision > j.Revision || !validProgress(current) {
		return current, progressUnavailable()
	}
	recorded := j
	recorded.Revision = revision
	return current, storeProgress(ctx, q, recorded, current, conversionID(j.ProjectID, j.ID, kind, id), actor, worker, at)
}

// Prepared imports have at most one hundred checkpoints and retain their source
// order in the archive while deriving canonical progress in actual time order.
func seedPreparedProgress(ctx context.Context, tx *sql.Tx, j job, ids []string, checkpoints []PreparedCheckpoint) error {
	if err := seedEmptyProgress(ctx, tx, j); err != nil {
		return err
	}
	keys := make([]legacyProgressKey, len(ids))
	for i, id := range ids {
		at, _ := time.Parse(time.RFC3339Nano, checkpoints[i].CreatedAt)
		keys[i] = legacyProgressKey{time: at.UTC().Format(noteTimeFormat), id: id}
	}
	sort.Slice(keys, func(i, k int) bool {
		if keys[i].time != keys[k].time {
			return keys[i].time < keys[k].time
		}
		return keys[i].id < keys[k].id
	})
	current := emptyProgress()
	for _, k := range keys {
		var err error
		current, err = convertLegacyProgress(ctx, tx, j, current, 0, k.id)
		if err != nil {
			return err
		}
	}
	return nil
}
