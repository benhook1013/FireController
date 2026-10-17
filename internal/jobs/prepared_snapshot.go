package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"

	"github.com/benhook1013/FireController/internal/strictjson"
)

type PreparedSnapshotReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ValidatePreparedSnapshot verifies immutable imported originals, allowing the
// ordinary current job and later native revisions to evolve after adoption.
func ValidatePreparedSnapshot(ctx context.Context, db PreparedSnapshotReader, project, id string, item PreparedJob, checkpointIDs []string) error {
	item, err := NormalizePreparedJob(item)
	if err != nil {
		return err
	}
	if len(checkpointIDs) != len(item.Checkpoints) {
		return preparedJobInvalid()
	}
	var actor, body, recorded string
	if err = db.QueryRowContext(ctx, `SELECT actor_id,state_json,created_at FROM job_history WHERE project_id=? AND job_id=? AND revision=1`, project, id).Scan(&actor, &body, &recorded); err != nil {
		return err
	}
	var original job
	if strictjson.Decode([]byte(body), &original) != nil || actor != "imported-history" || recorded != item.UpdatedAt || original != (job{ID: id, ProjectID: project, Title: item.Title, Brief: item.Brief, WorkerID: item.WorkerID, Status: item.Status, Revision: 1, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}) {
		return preparedJobInvalid()
	}
	var lastSequence int64
	for i, cp := range item.Checkpoints {
		var jobID, storedActor, worker, done, next, blocker, evidence, at string
		var rev, sequence int64
		if err = db.QueryRowContext(ctx, `SELECT job_id,job_revision,actor_id,worker_id,done,next,blocker,evidence_json,created_at,sequence FROM job_checkpoints WHERE project_id=? AND id=?`, project, checkpointIDs[i]).Scan(&jobID, &rev, &storedActor, &worker, &done, &next, &blocker, &evidence, &at, &sequence); err != nil {
			return err
		}
		var refs []string
		if json.Unmarshal([]byte(evidence), &refs) != nil || sequence <= lastSequence || jobID != id || rev != 1 || storedActor != cp.ActorID || worker != cp.WorkerID || done != cp.Done || next != cp.Next || blocker != cp.Blocker || at != cp.CreatedAt || !reflect.DeepEqual(refs, cp.Evidence) {
			return preparedJobInvalid()
		}
		lastSequence = sequence
	}
	return nil
}
