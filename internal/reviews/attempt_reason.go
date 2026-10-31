package reviews

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/benhook1013/FireController/internal/strictjson"
)

var attemptReasonCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,127}$`)

// ValidAttemptReason admits only optional bounded diagnostics, never provider prose.
func ValidAttemptReason(state, reason string) bool {
	return reason == "" || oneOf(state, "held", "uncertain", "rate_limited", "failed") && attemptReasonCode.MatchString(reason)
}

func attemptReasonMigrationSQL() []string {
	return []string{
		`ALTER TABLE review_attempt_versions ADD COLUMN reason TEXT NOT NULL DEFAULT '' CHECK (length(CAST(reason AS BLOB)) <= 128)`,
		`DROP VIEW review_attempts`, HistoryReadViewSQL()["review_attempts"],
	}
}

// ValidateAttemptReasonsHistory includes older, nonwinning immutable versions.
func ValidateAttemptReasonsHistory(ctx context.Context, q HistoryQuery) error {
	rows, err := q.QueryContext(ctx, `SELECT state,CASE WHEN length(CAST(reason AS BLOB))<=128 THEN reason END FROM review_attempt_versions`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var state, reason string
		if rows.Scan(&state, &reason) != nil || !ValidAttemptReason(state, reason) {
			return historyInconsistent()
		}
	}

	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	rows, err = q.QueryContext(ctx, `SELECT CASE WHEN length(CAST(latest_attempts_json AS BLOB))<=32768 THEN latest_attempts_json END FROM review_pull_versions`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var summaries []AttemptSummary
		if rows.Scan(&raw) != nil || strictStoredJSON(raw, &summaries) != nil || summaries == nil || len(summaries) > 4 {
			return historyInconsistent()
		}
	}
	return rows.Err()
}

func (a *Attempt) UnmarshalJSON(raw []byte) error {
	type wire Attempt
	var v wire
	if err := strictjson.Decode(raw, &v); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || !ValidAttemptReason(v.State, v.Reason) {
		return errors.New("invalid attempt diagnostic")
	}
	if _, provided := fields["reason"]; provided && v.Reason == "" {
		return errors.New("invalid attempt diagnostic")
	}
	*a = Attempt(v)
	return nil
}
