package reviewexec

import (
	"context"
	"database/sql"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// A configuration owner change governs new selections. It need not replace an
// already-selected operation whose own lane has never been withdrawn. Require
// complete durable control history instead of inferring continuity from the
// current enabled bit. The caller still validates the original exact credential,
// principal and active worker; both historical attribution and child stay intact.
func retainedAutomaticAuthorization(ctx context.Context, tx *sql.Tx, r Run, revision int64, originalActor string, current schedulingRecord) (bool, error) {
	if r.State != "selected" || r.OperationID == "" || revision < 1 || revision >= current.Revision || current.Revision-revision > 10000 {
		return false, nil
	}
	allowed, err := schedulingAuthorizerAllowed(ctx, tx, current)
	if err != nil || !allowed {
		return false, err
	}
	var bound bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_selections s JOIN review_operations o ON o.project_id=s.project_id AND o.id=s.operation_id WHERE s.project_id=? AND s.id=? AND s.state='selected' AND s.operation_id=? AND s.actor_id=? AND s.worker_id=? AND s.channel=? AND o.actor_id=s.actor_id AND o.worker_id=s.worker_id AND o.channel=s.channel)`, r.ProjectID, r.ID, r.OperationID, r.ActorID, r.WorkerID, r.Channel).Scan(&bound)
	if err != nil || !bound {
		return false, err
	}
	return automaticChannelContinuous(ctx, tx, r, revision, originalActor, current)
}

// An unchanged authorizer may finish an unselected intent after an unrelated
// channel change. Every revision must still prove its own channel stayed on.
func automaticChannelContinuous(ctx context.Context, tx *sql.Tx, r Run, revision int64, originalActor string, current schedulingRecord) (bool, error) {
	if revision < 1 || revision >= current.Revision || current.Revision-revision > 10000 {
		return false, nil
	}
	if _, _, err := schedulingBasisRevisions(ctx, tx, current); err != nil {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT revision,actor_id,CASE WHEN typeof(payload_json)='text' AND length(CAST(payload_json AS BLOB))<=4096 THEN payload_json END FROM events WHERE project_id=? AND event_type='review.scheduling_updated' AND resource_id=? AND revision BETWEEN ? AND ? ORDER BY revision,sequence LIMIT 10002`, r.ProjectID, r.ProjectID, revision, current.Revision)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	expected := revision
	for rows.Next() {
		var retainedRevision any
		var storedActor any
		var payload sql.NullString
		if err = rows.Scan(&retainedRevision, &storedActor, &payload); err != nil {
			return false, err
		}
		recorded, validRevision := retainedRevision.(int64)
		actor, nativeText := storedActor.(string)
		selectedChild := r.State == "selected" && r.OperationID != ""
		if !validRevision || !nativeText || !identifier.MatchString(actor) || recorded != expected ||
			(!selectedChild || recorded == revision) && actor != originalActor ||
			recorded == current.Revision && actor != current.actor {
			return false, nil
		}
		var settings schedulingEvent
		if !payload.Valid || strictjson.Decode([]byte(payload.String), &settings) != nil {
			return false, nil
		}
		enabled := settings.Hosted
		if r.Channel == "cli" {
			enabled = settings.CLI
		}
		if enabled == nil || !*enabled {
			return false, nil
		}
		expected++
	}
	if err = rows.Err(); err != nil {
		return false, err
	}
	return expected == current.Revision+1, nil
}
