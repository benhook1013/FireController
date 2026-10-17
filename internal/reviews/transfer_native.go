package reviews

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

func transferNativeConflict() error {
	return core.Fail("observation_conflict", "Imported native history cannot replace locally owned or retained native records.", 409)
}

// The executor is optional in reviews-only installations. Never initialize it,
// mistake an incompatible object for an absent table, or ignore a failed read.
// Table and column identifiers below are repository-owned literals.
func transferNativeCollision(ctx context.Context, tx HistoryQuery, project, table, column, id string) error {
	if id == "" {
		return nil
	}
	var kind string
	err := tx.QueryRowContext(ctx, `SELECT type FROM sqlite_schema WHERE name=?`, table).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if kind != "table" {
		return historyInconsistent()
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE project_id=? AND "+column+"=?)", project, id).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return transferNativeConflict()
	}
	return nil
}

// Compare the latest retained body, even when omitted from current membership.
// The same canonical column lists and SQLite value normalization drive writes.
func transferNativeExactRecord(ctx context.Context, tx HistoryQuery, project string, spec historyRecordSpec, keys, body []any) error {
	expected, err := historyParameters(body)
	if err != nil {
		return err
	}
	stored := make([]any, len(spec.body))
	targets := make([]any, len(stored))
	for i := range stored {
		targets[i] = &stored[i]
	}
	args := append([]any{project}, keys...)
	err = tx.QueryRowContext(ctx, "SELECT "+strings.Join(spec.body, ",")+" FROM "+spec.versions+" WHERE project_id=? AND "+historyPredicate(spec.keys)+" ORDER BY version_revision DESC LIMIT 1", args...).Scan(targets...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(stored, expected) {
		return transferNativeConflict()
	}
	return nil
}

func transferNativeRunGuard(ctx context.Context, tx HistoryQuery, project, source string, number int64, run RunInput) error {
	if source != nativeSource {
		return nil
	}
	if err := transferNativeCollision(ctx, tx, project, "review_execution_evidence", "id", run.SourceRunID); err != nil {
		return err
	}
	_, changedDetail, err := retainedRunState(ctx, tx, project, source, number, run)
	if err != nil {
		return err
	}
	if changedDetail {
		return transferNativeConflict()
	}
	summary := summarizeRun(project, source, number, run)
	return transferNativeExactRecord(ctx, tx, project, historyRunRecord, []any{summary.ID}, []any{number, source, run.SourceRunID, run.Channel, run.HeadSHA, run.Reviewer, run.Scope, run.Outcome, run.Attributable, run.Complete, run.StartedAt, run.FinishedAt, run.Found, run.Accepted, run.Routed, run.Finalized, summary.Fixed, mustJSON(run.CoverageLimits)})
}

func transferNativeAttemptGuard(ctx context.Context, tx HistoryQuery, project, source string, number int64, attempt Attempt) error {
	if source != nativeSource {
		return nil
	}
	for _, owner := range []struct{ table, column, id string }{
		{"review_execution_attempts", "id", attempt.SourceAttemptID},
		{"review_operations", "attempt_id", attempt.SourceAttemptID},
		{"review_execution_evidence", "id", attempt.SourceRunID},
	} {
		if err := transferNativeCollision(ctx, tx, project, owner.table, owner.column, owner.id); err != nil {
			return err
		}
	}
	if err := retainedAttemptState(ctx, tx, project, source, number, attempt); err != nil {
		return err
	}
	return transferNativeExactRecord(ctx, tx, project, historyAttemptRecord, []any{source, attempt.SourceAttemptID}, []any{number, attempt.Channel, attempt.HeadSHA, attempt.State, attempt.StartedAt, attempt.FinishedAt, attempt.DurationSeconds, attempt.SourceRunID, attempt.TriggerID, attempt.ProviderReviewID})
}
