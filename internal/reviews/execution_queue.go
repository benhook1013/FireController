package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/benhook1013/FireController/internal/core"
)

// PublishExecutionQueue makes explicit execution enrollment visible in the same
// transaction. Metadata remains unknown until the normal observer verifies it.
// Retained review-only entries keep their identity, metadata and evidence.
// A nonzero restore explicitly reactivates only that retired PR. Ordinary
// publication passes zero and cannot clear a retirement.
// The result reports whether a new queue snapshot was published. Queue may be
// just the selected relative order for a scoped reorder, not the full schedule.
func PublishExecutionQueue(ctx context.Context, tx *sql.Tx, scope core.Scope, previous, queue []int64, restore int64) (bool, error) {
	if queue == nil || len(queue) > maxQueuePulls || len(previous) > maxQueuePulls {
		return false, validationError("Select a bounded execution queue.")
	}
	if restore != 0 {
		if !slices.Contains(queue, restore) {
			return false, validationError("The restored PR must be enrolled in the execution queue.")
		}
		var retired bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_retired WHERE project_id=? AND number=?) OR (NOT EXISTS(SELECT 1 FROM review_current_pulls WHERE project_id=? AND number=?) AND EXISTS(SELECT 1 FROM review_retired_versions WHERE project_id=? AND number=?))`, scope.ProjectID, restore, scope.ProjectID, restore, scope.ProjectID, restore).Scan(&retired); err != nil {
			return false, err
		}
		if !retired {
			return false, core.Fail("review_target_not_retired", "This PR is not currently retired.", 409)
		}
	}
	if len(queue) == 0 {
		return false, nil
	}
	var repository, revision int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&repository); err != nil {
		return false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM review_workspace WHERE project_id=?`, scope.ProjectID).Scan(&revision); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	req := QueueEditRequest{ExpectedRevision: &revision, RepositoryID: repository, Source: "execution-queue", Queue: []QueuePullInput{}, Retired: []RetiredPull{}}
	type placed struct {
		number   int64
		position int
	}
	var ordered []placed
	var next int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),-1)+1 FROM review_current_pulls WHERE project_id=?`, scope.ProjectID).Scan(&next); err != nil {
		return false, err
	}
	for _, number := range queue {
		var retired bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_retired WHERE project_id=? AND number=?)`, scope.ProjectID, number).Scan(&retired); err != nil {
			return false, err
		}
		if retired && number != restore {
			if !slices.Contains(previous, number) {
				return false, core.Fail("review_target_retired", "An explicitly retired PR must be reactivated before execution enrollment.", 409)
			}
			continue
		}
		body, position, err := retainedTransferPullBody(ctx, tx, scope.ProjectID, number)
		if errors.Is(err, sql.ErrNoRows) {
			var historicallyRetired bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_retired_versions WHERE project_id=? AND number=?)`, scope.ProjectID, number).Scan(&historicallyRetired); err != nil {
				return false, err
			}
			if historicallyRetired && number != restore {
				if !slices.Contains(previous, number) {
					return false, core.Fail("review_target_retired", "An explicitly retired PR must be reactivated before execution enrollment.", 409)
				}
				continue
			}
			req.Queue = append(req.Queue, QueuePullInput{Number: number, Title: fmt.Sprintf("PR #%d", number), State: "unknown", CIState: "unknown", Mergeability: "unknown"})
			position = next
			next++
		} else if err != nil {
			return false, err
		} else if body[13].(int64) == 0 || number == restore {
			m, err := ReadPullMetadata(body[12].(string), body[1].(string))
			if err != nil {
				return false, err
			}
			req.Queue = append(req.Queue, QueuePullInput{Number: number, Title: body[0].(string), State: body[1].(string), HeadSHA: body[2].(string), BaseRef: body[3].(string), BaseSHA: body[4].(string), CIState: body[5].(string), Mergeability: body[6].(string), Diff: m.Diff, IsDraft: m.IsDraft, MergedAt: m.MergedAt})
		}
		req.Order = append(req.Order, number)
		ordered = append(ordered, placed{number, position})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].position < ordered[j].position })
	changed := len(req.Queue) > 0
	for i, p := range ordered {
		if req.Order[i] != p.number {
			changed = true
			break
		}
	}
	if !changed {
		return false, nil
	}
	if len(req.Order) == 0 {
		req.Order = nil
	}
	if err := validateQueueEdit(&req); err != nil {
		return false, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return false, err
	}
	_, err = publishQueueEdit(ctx, tx, scope, req, body)
	return err == nil, err
}
