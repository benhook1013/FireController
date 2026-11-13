package coordimport

import (
	"context"
	"time"
)

func validateMessageSnapshot(ctx context.Context, db SnapshotReader, state map[string]int64) error {
	type ref struct {
		project, source, key, hash, at string
		revision                       int64
	}
	var oversized bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM coordination_import_messages GROUP BY project_id,import_revision HAVING count(*)>100)`).Scan(&oversized); err != nil {
		return err
	}
	if oversized {
		return conflict()
	}
	lastProject, lastSource, lastKey := "", "", ""
	for {
		rows, err := db.QueryContext(ctx, `SELECT CASE WHEN length(CAST(project_id AS BLOB))<=128 THEN project_id END,CASE WHEN length(CAST(source AS BLOB))<=200 THEN source END,CASE WHEN length(CAST(source_key AS BLOB))<=200 THEN source_key END,import_revision,CASE WHEN length(source_sha256)=64 THEN source_sha256 END,CASE WHEN length(CAST(created_at AS BLOB))<=64 THEN created_at END FROM coordination_import_messages WHERE (project_id,source,source_key)>(?,?,?) ORDER BY project_id,source,source_key LIMIT 25`, lastProject, lastSource, lastKey)
		if err != nil {
			return err
		}
		refs := make([]ref, 0, 25)
		for rows.Next() {
			var r ref
			if err = rows.Scan(&r.project, &r.source, &r.key, &r.revision, &r.hash, &r.at); err != nil {
				break
			}
			refs = append(refs, r)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, r := range refs {
			at, e := time.Parse(time.RFC3339Nano, r.at)
			if state[r.project] < r.revision || r.revision < 1 || !validHash(r.hash) || e != nil || at.IsZero() {
				return conflict()
			}
			v, err := readMessage(ctx, db, r.project, r.source, r.key)
			if err != nil {
				return err
			}
			var original bool
			if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM coordination_import_batches WHERE project_id=? AND revision=? AND source=? AND source_sha256=? AND created_at=?)`, r.project, r.revision, r.source, r.hash, r.at).Scan(&original); err != nil {
				return err
			}
			if !original {
				return conflict()
			}
			if v.item.ReplyToSourceKey != "" {
				var parent bool
				if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM coordination_import_messages WHERE project_id=? AND source=? AND source_key=? AND message_id=? AND conversation_id=?)`, r.project, r.source, v.item.ReplyToSourceKey, v.reply, v.conversation).Scan(&parent); err != nil {
					return err
				}
				if !parent {
					return conflict()
				}
			}
			lastProject, lastSource, lastKey = r.project, r.source, r.key
		}
		if len(refs) < 25 {
			break
		}
	}
	return ctx.Err()
}
