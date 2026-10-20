package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Check one current compact summary against its bounded indexed winners.
// Publication checks before deriving a successor; full maintenance reuses this
// check for each current pull and separately audits every nonwinning order row.
func validateCurrentSummaryFront(ctx context.Context, tx HistoryQuery, project string, number int64) error {
	return validateCurrentSummaryFrontColumns(ctx, tx, project, number, summaryAttemptColumns)
}
func validateCurrentSummaryFrontColumns(ctx context.Context, tx HistoryQuery, project string, number int64, columns string) error {
	var runs, attempts int64
	var cachedRuns, cachedAttempts sql.NullInt64
	var stored string
	err := tx.QueryRowContext(ctx, `SELECT v.run_count,v.attempt_count,substr(CAST(v.latest_attempts_json AS BLOB),1,32769),t.run_count,t.attempt_count
 FROM review_current_pulls c
 CROSS JOIN review_pull_memberships m ON m.project_id=c.project_id AND m.number=c.number AND m.start_revision=c.start_revision AND m.position=c.position
 CROSS JOIN review_pull_versions v ON v.project_id=m.project_id AND v.number=m.number AND v.version_revision=m.version_revision
 LEFT JOIN review_current_totals t ON t.project_id=c.project_id AND t.pull_number=c.number
 WHERE c.project_id=? AND c.number=?`, project, number).Scan(&runs, &attempts, &stored, &cachedRuns, &cachedAttempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !cachedRuns.Valid || !cachedAttempts.Valid || runs != cachedRuns.Int64 || attempts != cachedAttempts.Int64 {
		return historyInconsistent()
	}
	// Four admitted summaries, including worst-case JSON escaping of their
	// bounded string fields, fit below 32 KiB. Refuse malformed retained data
	// before allocating/decoding an arbitrary stored document.
	var previous []AttemptSummary
	if len(stored) > 32<<10 || json.Unmarshal([]byte(stored), &previous) != nil || previous == nil || len(previous) > 4 || mustJSON(previous) != stored {
		return historyInconsistent()
	}
	expected := make(map[string]AttemptSummary, len(previous))
	for _, item := range previous {
		if !oneOf(item.Channel, "hosted", "cli", "manual", "subagent") || !ValidAttemptReason(item.State, item.Reason) {
			return historyInconsistent()
		}
		if _, exists := expected[item.Channel]; exists {
			return historyInconsistent()
		}
		expected[item.Channel] = item
	}
	actual := make([]AttemptSummary, 0, len(previous))
	for _, channel := range []string{"hosted", "cli", "manual", "subagent"} {
		var candidate Attempt
		var key, cachedChannel string
		var cachedNumber, cachedPosition, currentNumber, currentPosition, memberNumber, memberPosition, bodyNumber int64
		err := tx.QueryRowContext(ctx, `SELECT `+columns+`,o.sort_key,o.channel,o.pull_number,o.position,c.pull_number,c.position,m.pull_number,m.position,v.pull_number
 FROM review_current_attempt_order o
 CROSS JOIN review_current_attempts c ON c.project_id=o.project_id AND c.source=o.source AND c.source_attempt_id=o.source_attempt_id
 CROSS JOIN review_attempt_memberships m ON m.project_id=c.project_id AND m.source=c.source AND m.source_attempt_id=c.source_attempt_id AND m.start_revision=c.start_revision
 CROSS JOIN review_attempt_versions v ON v.project_id=m.project_id AND v.source=m.source AND v.source_attempt_id=m.source_attempt_id AND v.version_revision=m.version_revision
 WHERE o.project_id=? AND o.pull_number=? AND o.channel=? ORDER BY o.sort_key DESC,o.position DESC LIMIT 1`, project, number, channel).Scan(&candidate.SourceAttemptID, &candidate.Channel, &candidate.HeadSHA, &candidate.State, &candidate.StartedAt, &candidate.FinishedAt, &candidate.DurationSeconds, &candidate.SourceRunID, &candidate.TriggerID, &candidate.ProviderReviewID, &candidate.Reason, &key, &cachedChannel, &cachedNumber, &cachedPosition, &currentNumber, &currentPosition, &memberNumber, &memberPosition, &bodyNumber)
		prior, exists := expected[channel]
		if errors.Is(err, sql.ErrNoRows) {
			if exists {
				return historyInconsistent()
			}
			continue
		}
		if err != nil {
			return err
		}
		canonicalKey, err := attemptSortKey(candidate)
		if err != nil {
			return err
		}
		if !ValidAttemptReason(candidate.State, candidate.Reason) || !exists || mustJSON(AttemptSummary(candidate)) != mustJSON(prior) || key != canonicalKey || candidate.Channel != channel || cachedChannel != candidate.Channel || cachedNumber != number || currentNumber != number || memberNumber != number || bodyNumber != number || cachedPosition != currentPosition || memberPosition != currentPosition {
			return historyInconsistent()
		}
		actual = append(actual, AttemptSummary(candidate))
	}
	if mustJSON(actual) != stored {
		return historyInconsistent()
	}
	return nil
}
