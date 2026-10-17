package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/coordimportmodel"
	"github.com/benhook1013/FireController/internal/core"
)

// PreparedJob is an explicitly selected historical snapshot. SourceRevision and
// source keys belong in the caller's provenance, not native revision history.
type PreparedJob = coordimportmodel.PreparedJob
type PreparedCheckpoint = coordimportmodel.PreparedCheckpoint

func preparedJobInvalid() error {
	return core.Fail("validation_failed", "Prepared job snapshot is invalid.", 400)
}

func validPreparedSourceKey(value string) bool {
	return len(value) <= 200 && utf8.ValidString(value) && strings.TrimSpace(value) != ""
}

// NormalizePreparedJob validates without rewriting historical text or times.
// Only absent collections become empty collections. It does not allocate IDs.
func NormalizePreparedJob(item PreparedJob) (PreparedJob, error) {
	if !validPreparedSourceKey(item.SourceKey) || item.SourceRevision < 1 ||
		item.Title != strings.TrimSpace(item.Title) || !utf8.ValidString(item.Title) || !utf8.ValidString(item.Brief) ||
		item.WorkerID != "" && !validPrimaryID(item.WorkerID) ||
		!contextTime(item.CreatedAt) || !contextTime(item.UpdatedAt) || len(item.Checkpoints) > 100 {
		return PreparedJob{}, preparedJobInvalid()
	}
	if err := validateContent(item.Title, item.Brief, item.Status); err != nil {
		return PreparedJob{}, err
	}
	checkpoints := make([]PreparedCheckpoint, len(item.Checkpoints))
	seen := make(map[string]bool, len(item.Checkpoints))
	for i, checkpoint := range item.Checkpoints {
		if !validPreparedSourceKey(checkpoint.SourceKey) || seen[checkpoint.SourceKey] || !validPrimaryID(checkpoint.ActorID) ||
			checkpoint.WorkerID != "" && !validPrimaryID(checkpoint.WorkerID) || !contextTime(checkpoint.CreatedAt) {
			return PreparedJob{}, preparedJobInvalid()
		}
		seen[checkpoint.SourceKey] = true
		if err := validateCheckpoint(checkpointRequest{ExpectedJobRevision: 1, Done: checkpoint.Done, Next: checkpoint.Next,
			Blocker: &checkpoint.Blocker, Evidence: checkpoint.Evidence}); err != nil {
			return PreparedJob{}, err
		}
		checkpoint.Evidence = append([]string{}, checkpoint.Evidence...)
		checkpoints[i] = checkpoint
	}
	item.Checkpoints = checkpoints
	return item, nil
}

// InsertPreparedJob adds one selected snapshot as native revision 1. The caller
// owns authorization, the transaction, provenance, and rollback on any error.
// No events, receipts, source revisions, or historical deliveries are created.
func InsertPreparedJob(ctx context.Context, tx *sql.Tx, projectID, jobID string, item PreparedJob, checkpointIDs []string) error {
	if ctx == nil || tx == nil || !validPrimaryID(projectID) || !validPrimaryID(jobID) {
		return preparedJobInvalid()
	}
	item, err := NormalizePreparedJob(item)
	if err != nil {
		return err
	}
	if len(checkpointIDs) != len(item.Checkpoints) {
		return preparedJobInvalid()
	}
	seenIDs := make(map[string]bool, len(checkpointIDs))
	for _, id := range checkpointIDs {
		if !validPrimaryID(id) || seenIDs[id] {
			return preparedJobInvalid()
		}
		seenIDs[id] = true
	}
	if item.WorkerID != "" {
		if err := core.CheckWorker(ctx, tx, projectID, item.WorkerID); err != nil {
			return err
		}
	}
	for _, checkpoint := range item.Checkpoints {
		if checkpoint.WorkerID != "" {
			if err := core.CheckWorker(ctx, tx, projectID, checkpoint.WorkerID); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO jobs(project_id,id,title,brief,worker_id,status,revision,created_at,updated_at)
		VALUES(?,?,?,?,NULLIF(?,''),?,1,?,?)`, projectID, jobID, item.Title, item.Brief, item.WorkerID, item.Status, item.CreatedAt, item.UpdatedAt)
	if err != nil {
		return err
	}
	current := job{ID: jobID, ProjectID: projectID, Title: item.Title, Brief: item.Brief, WorkerID: item.WorkerID,
		Status: item.Status, Revision: 1, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
	if err = appendHistory(ctx, tx, core.Scope{PrincipalID: "imported-history"}, current); err != nil {
		return err
	}
	for i, checkpoint := range item.Checkpoints {
		evidence, err := json.Marshal(checkpoint.Evidence)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO job_checkpoints
			(id,project_id,job_id,job_revision,actor_id,worker_id,done,next,blocker,evidence_json,created_at)
			VALUES(?,?,?,1,?,?,?,?,?,?,?)`, checkpointIDs[i], projectID, jobID, checkpoint.ActorID, checkpoint.WorkerID,
			checkpoint.Done, checkpoint.Next, checkpoint.Blocker, string(evidence), checkpoint.CreatedAt)
		if err != nil {
			return err
		}
	}
	return nil
}
