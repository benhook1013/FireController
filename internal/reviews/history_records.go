package reviews

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

var historyFindingColumns = []string{"position", "source_finding_id", "title", "detail", "severity", "disposition", "decision_reason", "actor", "decided_at", "fix_sha", "proof_note", "resolved_at", "route_json"}

func historyFindingValues(position int, finding Finding) []any {
	return []any{int64(position), finding.SourceFindingID, finding.Title, finding.Detail, finding.Severity, finding.Disposition, finding.DecisionReason, finding.Actor, finding.DecidedAt, finding.FixSHA, finding.ProofNote, finding.ResolvedAt, mustJSON(finding.Route)}
}

// A run and its bounded detail share one immutable version. Comparing detail
// separately prevents summary-only equality from hiding an adjudication change.
func historyFindingsEqual(ctx context.Context, tx HistoryQuery, project, id string, version int64, findings []Finding) (bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT "+strings.Join(historyFindingColumns, ",")+" FROM review_finding_versions WHERE project_id=? AND run_id=? AND run_version_revision=? ORDER BY position", project, id, version)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	position := 0
	equal := true
	for rows.Next() {
		stored := make([]any, len(historyFindingColumns))
		targets := make([]any, len(stored))
		for i := range stored {
			targets[i] = &stored[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return false, err
		}
		if position >= len(findings) || !reflect.DeepEqual(stored, historyFindingValues(position, findings[position])) {
			equal = false
		}
		position++
	}
	return equal && position == len(findings), rows.Err()
}

// retainedRunState is SELECT-only so preview and immutable writes share identity checks.
func retainedRunState(ctx context.Context, tx HistoryQuery, project, source string, number int64, run RunInput) (int64, bool, error) {
	summary := summarizeRun(project, source, number, run)
	var oldVersion, oldNumber int64
	var oldSource, oldSourceID, oldChannel, oldHead, oldScope string
	err := tx.QueryRowContext(ctx, `SELECT version_revision,pull_number,source,source_run_id,channel,head_sha,scope FROM review_run_versions WHERE project_id=? AND id=? ORDER BY version_revision DESC LIMIT 1`, project, summary.ID).Scan(&oldVersion, &oldNumber, &oldSource, &oldSourceID, &oldChannel, &oldHead, &oldScope)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	changedDetail := false
	if err == nil {
		if oldNumber != number || oldSource != source || oldSourceID != run.SourceRunID || oldChannel != run.Channel || oldHead != run.HeadSHA || oldScope != run.Scope {
			return 0, false, core.Fail("observation_conflict", "A retained run's source identity and owning PR cannot change.", 409)
		}
		equal, err := historyFindingsEqual(ctx, tx, project, summary.ID, oldVersion, run.Findings)
		if err != nil {
			return 0, false, err
		}
		changedDetail = !equal
	}
	return oldVersion, changedDetail, nil
}

func (w *historySnapshotWrite) run(ctx context.Context, tx *sql.Tx, source string, number int64, order int, run RunInput) error {
	summary := summarizeRun(w.project, source, number, run)
	oldVersion, changedDetail, err := retainedRunState(ctx, w.historyRead(tx), w.project, source, number, run)
	if err != nil {
		return err
	}
	version, err := w.version(ctx, tx, historyRunRecord, []any{summary.ID}, []any{number, source, run.SourceRunID, run.Channel, run.HeadSHA, run.Reviewer, run.Scope, run.Outcome, run.Attributable, run.Complete, run.StartedAt, run.FinishedAt, run.Found, run.Accepted, run.Routed, run.Finalized, summary.Fixed, mustJSON(run.CoverageLimits)}, changedDetail)
	if err != nil {
		return err
	}
	if version != oldVersion {
		columns := append([]string{"project_id", "run_id", "run_version_revision", "pull_number"}, historyFindingColumns...)
		for position, finding := range run.Findings {
			values := append([]any{w.project, summary.ID, version, number}, historyFindingValues(position, finding)...)
			if err := w.insert(ctx, tx, "review_finding_versions", columns, values); err != nil {
				return err
			}
		}
	}
	return w.membership(ctx, tx, historyRunRecord, []any{summary.ID}, version, []any{number, order})
}

// retainedAttemptState performs the same read-only ownership/settlement guards for preview and writes.
func retainedAttemptState(ctx context.Context, tx HistoryQuery, project, source string, number int64, attempt Attempt) error {
	var oldNumber int64
	var channel, head, started, state, run, trigger, provider string
	err := tx.QueryRowContext(ctx, `SELECT pull_number,channel,head_sha,started_at,state,source_run_id,trigger_id,provider_review_id FROM review_attempt_versions WHERE project_id=? AND source=? AND source_attempt_id=? ORDER BY version_revision DESC LIMIT 1`, project, source, attempt.SourceAttemptID).Scan(&oldNumber, &channel, &head, &started, &state, &run, &trigger, &provider)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if oldNumber != number || channel != attempt.Channel || head != attempt.HeadSHA || started != attempt.StartedAt {
			return core.Fail("observation_conflict", "A retained attempt's source identity and owning PR cannot change.", 409)
		}
		for _, identity := range [][2]string{{run, attempt.SourceRunID}, {trigger, attempt.TriggerID}, {provider, attempt.ProviderReviewID}} {
			if identity[0] != "" && identity[0] != identity[1] {
				return core.Fail("observation_conflict", "Known capture and provider identities cannot be cleared or replaced.", 409)
			}
		}
		if oneOf(state, "completed", "failed", "cancelled", "rate_limited", "held") && state != attempt.State {
			return core.Fail("observation_conflict", "A settled retained attempt cannot be reopened.", 409)
		}
	}
	return nil
}

func (w *historySnapshotWrite) attempt(ctx context.Context, tx *sql.Tx, source string, number int64, position int, attempt Attempt) error {
	if err := retainedAttemptState(ctx, w.historyRead(tx), w.project, source, number, attempt); err != nil {
		return err
	}
	version, err := w.version(ctx, tx, historyAttemptRecord, []any{source, attempt.SourceAttemptID}, []any{number, attempt.Channel, attempt.HeadSHA, attempt.State, attempt.StartedAt, attempt.FinishedAt, attempt.DurationSeconds, attempt.SourceRunID, attempt.TriggerID, attempt.ProviderReviewID}, false)
	if err != nil {
		return err
	}
	return w.membership(ctx, tx, historyAttemptRecord, []any{source, attempt.SourceAttemptID}, version, []any{number, position})
}
