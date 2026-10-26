package reviews

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

type transferKey struct{ source, id string }
type transferPullPlan struct {
	body                           []any
	position, nextRun, nextAttempt int
	runs                           map[string]int
	attempts                       map[transferKey]int
}
type transferPlan struct {
	revision           int64
	snapshot, observed string
	pulls              map[int64]*transferPullPlan
	order              []int64
}

func transferConflict(message string) error { return core.Fail("observation_conflict", message, 409) }

// Admission performs only reads. No temporary versions, receipts or events are
// needed to establish whether the exact batch can be applied to this snapshot.
func admitTransfer(ctx context.Context, transaction *sql.Tx, project string, req TransferRequest) (transferPlan, error) {
	tx := newHistoryReads(transaction)
	defer tx.close()
	plan := transferPlan{pulls: map[int64]*transferPullPlan{}}
	var repository int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repository); err != nil {
		return plan, err
	}
	if repository != req.RepositoryID {
		return plan, validationError("repository_id does not match the configured project repository.")
	}
	err := tx.QueryRowContext(ctx, `SELECT w.revision,w.snapshot_id,s.observed_at FROM review_workspace w JOIN review_snapshots s ON s.project_id=w.project_id AND s.snapshot_id=w.snapshot_id WHERE w.project_id=?`, project).Scan(&plan.revision, &plan.snapshot, &plan.observed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return plan, err
	}
	if plan.revision != *req.ExpectedRevision {
		return plan, &core.Error{Code: "revision_conflict", Message: "The review workspace revision changed.", Status: 409, Details: map[string]int64{"current_revision": plan.revision}}
	}
	if plan.revision == math.MaxInt64 {
		return plan, core.Fail("revision_exhausted", "The review workspace revision cannot advance further.", 409)
	}
	var nextPull int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),-1)+1 FROM review_current_pulls WHERE project_id=?`, project).Scan(&nextPull); err != nil {
		return plan, err
	}
	// A run may appear later than its attempt in the batch. Resolve links from
	// all selected producer identities without changing request ordering.
	runs := map[transferKey]struct {
		number int64
		run    RunInput
	}{}
	for _, group := range req.Groups {
		for _, pull := range group.Pulls {
			for _, run := range pull.Runs {
				runs[transferKey{group.Source, run.SourceRunID}] = struct {
					number int64
					run    RunInput
				}{pull.Number, run}
			}
		}
	}
	for _, group := range req.Groups {
		if err := guardSubagentImport(ctx, tx, project, group.Source, group.Pulls); err != nil {
			return plan, err
		}
		if err := validateFinalizedCounts(ctx, tx, project, group.Source, group.Pulls); err != nil {
			return plan, err
		}
		for _, pull := range group.Pulls {
			selected := plan.pulls[pull.Number]
			if selected == nil {
				var err error
				selected, err = planTransferPull(ctx, tx, project, plan.snapshot, pull, nextPull)
				if err != nil {
					return plan, err
				}
				if selected.position == nextPull {
					nextPull++
				}
				plan.pulls[pull.Number] = selected
				plan.order = append(plan.order, pull.Number)
			}
			for _, run := range pull.Runs {
				if _, _, err := retainedRunState(ctx, tx, project, group.Source, pull.Number, run); err != nil {
					return plan, err
				}
				if err := transferNativeRunGuard(ctx, tx, project, group.Source, pull.Number, run); err != nil {
					return plan, err
				}
				id := runOpaqueID(project, group.Source, run.SourceRunID)
				var position int
				err := tx.QueryRowContext(ctx, `SELECT run_order FROM review_current_runs WHERE project_id=? AND id=?`, project, id).Scan(&position)
				if errors.Is(err, sql.ErrNoRows) {
					position = selected.nextRun
					selected.nextRun++
				} else if err != nil {
					return plan, err
				}
				selected.runs[id] = position
			}
			for _, attempt := range pull.Attempts {
				if err := retainedAttemptState(ctx, tx, project, group.Source, pull.Number, attempt); err != nil {
					return plan, err
				}
				if err := transferNativeAttemptGuard(ctx, tx, project, group.Source, pull.Number, attempt); err != nil {
					return plan, err
				}
				if attempt.SourceRunID != "" {
					key := transferKey{group.Source, attempt.SourceRunID}
					if candidate, ok := runs[key]; ok {
						if candidate.number != pull.Number || candidate.run.Channel != attempt.Channel || candidate.run.HeadSHA != attempt.HeadSHA {
							return plan, transferConflict("An attempt must link to its original producer's run on the same PR, channel and head.")
						}
					} else {
						var count int
						if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM review_current_runs c CROSS JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision CROSS JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision WHERE c.project_id=? AND v.source=? AND v.source_run_id=? AND c.pull_number=? AND v.channel=? AND v.head_sha=?`, project, group.Source, attempt.SourceRunID, pull.Number, attempt.Channel, attempt.HeadSHA).Scan(&count); err != nil {
							return plan, err
						}
						if count != 1 {
							return plan, transferConflict("An attempt must link to a current or selected run from its original producer on the same PR, channel and head.")
						}
					}
				}
				var position int
				err := tx.QueryRowContext(ctx, `SELECT position FROM review_current_attempts WHERE project_id=? AND source=? AND source_attempt_id=?`, project, group.Source, attempt.SourceAttemptID).Scan(&position)
				if errors.Is(err, sql.ErrNoRows) {
					position = selected.nextAttempt
					selected.nextAttempt++
				} else if err != nil {
					return plan, err
				}
				selected.attempts[transferKey{group.Source, attempt.SourceAttemptID}] = position
			}
		}
	}
	return plan, nil
}

func planTransferPull(ctx context.Context, tx HistoryQuery, project, snapshot string, pull PullInput, next int) (*transferPullPlan, error) {
	body, position, err := retainedTransferPullBody(ctx, tx, project, pull.Number)
	metadata := []any{pull.Title, pull.State, pull.HeadSHA, pull.BaseRef, pull.BaseSHA, pull.CIState, pull.Mergeability}
	if errors.Is(err, sql.ErrNoRows) {
		var retired int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_retired_versions WHERE project_id=? AND number=?)`, project, pull.Number).Scan(&retired); err != nil {
			return nil, err
		}
		if retired != 0 {
			return nil, transferConflict("A retired PR requires explicit queue reactivation before historical transfer.")
		}
		body = append(metadata, int64(0), int64(0), int64(0), "[]", "[]", mustJSON(pullMetadata(pull)), int64(1))
		position = next
	} else if err != nil {
		return nil, err
	} else {
		var retired int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_retired WHERE project_id=? AND number=?)`, project, pull.Number).Scan(&retired); err != nil {
			return nil, err
		}
		if retired != 0 {
			return nil, transferConflict("A retired PR requires explicit queue reactivation before historical transfer.")
		}
		if mustJSON(pullMetadata(pull)) != "{}" && body[12] != mustJSON(pullMetadata(pull)) {
			return nil, transferConflict("Selected display metadata differs from the current workspace.")
		}
		if !reflect.DeepEqual(body[:7], metadata) {
			return nil, transferConflict("Selected PR metadata differs from the current workspace; prepare the batch with current metadata.")
		}
	}
	body[7] = int64(0)
	selected := &transferPullPlan{body: body, position: position, runs: map[string]int{}, attempts: map[transferKey]int{}}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(run_order),-1)+1 FROM review_current_runs WHERE project_id=? AND pull_number=?`, project, pull.Number).Scan(&selected.nextRun); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),-1)+1 FROM review_current_attempts WHERE project_id=? AND pull_number=?`, project, pull.Number).Scan(&selected.nextAttempt); err != nil {
		return nil, err
	}
	return selected, nil
}

// Admission is pinned to the current workspace in this read transaction. Resolve
// its indexed references directly rather than preparing the historical UNION
// view for every selected record.
func retainedTransferPullBody(ctx context.Context, tx HistoryQuery, project string, number int64) ([]any, int, error) {
	body := make([]any, len(historyPullRecord.body))
	targets := make([]any, 0, len(body)+1)
	for i := range body {
		targets = append(targets, &body[i])
	}
	var position int
	targets = append(targets, &position)
	columns := make([]string, len(historyPullRecord.body))
	for i, column := range historyPullRecord.body {
		columns[i] = "v." + column
	}
	err := tx.QueryRowContext(ctx, "SELECT "+strings.Join(columns, ",")+`,c.position FROM review_current_pulls c CROSS JOIN review_pull_memberships m ON m.project_id=c.project_id AND m.number=c.number AND m.start_revision=c.start_revision AND m.position=c.position CROSS JOIN review_pull_versions v ON v.project_id=m.project_id AND v.number=m.number AND v.version_revision=m.version_revision WHERE c.project_id=? AND c.number=?`, project, number).Scan(targets...)
	return body, position, err
}
