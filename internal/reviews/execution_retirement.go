package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/benhook1013/FireController/internal/core"
)

// RetireExecutionPull hides a deliberately superseded/defunct scheduling target
// in the caller's queue-change transaction. It preserves every retained record.
// The caller establishes project-write authority and checks pending operations.
func RetireExecutionPull(ctx context.Context, tx *sql.Tx, scope core.Scope, pull int64, reason string) error {
	var visible bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_pulls p WHERE p.project_id=? AND p.number=? AND NOT EXISTS(SELECT 1 FROM review_current_retired r WHERE r.project_id=p.project_id AND r.number=p.number))`, scope.ProjectID, pull).Scan(&visible)
	if err == nil && !visible {
		return core.Fail("review_queue_target_missing", "This PR has no visible queue entry to retire.", 409)
	}
	if err != nil {
		return err
	}
	body, _, err := retainedTransferPullBody(ctx, tx, scope.ProjectID, pull)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Fail("review_queue_target_missing", "This PR has no retained queue entry to retire.", 409)
	}
	if err != nil {
		return err
	}
	if body[1] == "merged" {
		return core.Fail("review_queue_merged", "Merged PRs stay in completed queue history.", 409)
	}
	var repository, revision int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&repository); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM review_workspace WHERE project_id=?`, scope.ProjectID).Scan(&revision); err != nil {
		return err
	}
	req := QueueEditRequest{ExpectedRevision: &revision, RepositoryID: repository, Source: "execution-queue-removal", Queue: []QueuePullInput{}, Retired: []RetiredPull{{Number: pull, Reason: reason}}}
	if err := validateQueueEdit(&req); err != nil {
		return err
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = publishQueueEdit(ctx, tx, scope, req, encoded)
	return err
}
