package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
)

const hostedQueryDiagnosticHeader = "X-FC-Hosted-Query"

// HostedQueryDiagnostic exposes original source handles, never provider bodies,
// private credentials or a new claim about availability.
type HostedQueryDiagnostic struct {
	QueryID        string `json:"query_id"`
	QueryCommentID int64  `json:"query_comment_id,omitempty"`
	QueryURL       string `json:"query_url,omitempty"`
	ObservationID  string `json:"observation_id,omitempty"`
	CaptureID      string `json:"capture_id,omitempty"`
	CapturedAt     string `json:"captured_at,omitempty"`
}

func readHostedQueryDiagnostic(ctx context.Context, tx *sql.Tx, o Operation) (*HostedQueryDiagnostic, error) {
	if o.Channel != "hosted" || o.State != "held" || o.AttemptID == "" || o.Reason != "hosted_allowance_unproven" && o.Reason != "hosted_allowance_reply_pending" && o.Reason != "hosted_allowance_cooldown" {
		return nil, nil
	}
	a, err := readAttempt(ctx, tx, o.ProjectID, o.AttemptID)
	if err != nil {
		return nil, err
	}
	query, err := readHostedQuery(ctx, tx, a, o)
	if err != nil {
		var missing *core.Error
		// Preparation can fail before any query intent was recorded.
		if errors.As(err, &missing) && missing.Code == "not_found" {
			return nil, nil
		}
		return nil, err
	}
	result := &HostedQueryDiagnostic{QueryID: query.ID}
	if query.Comment != nil {
		result.QueryCommentID = query.Comment.ID
		result.QueryURL = "https://github.com/" + query.Anchor.Repository + "/pull/" + strconv.FormatInt(o.PullNumber, 10) + "#issuecomment-" + strconv.FormatInt(query.Comment.ID, 10)
	}
	var id, project, attempt, operation, checksum, body string
	var position, size int64
	err = tx.QueryRowContext(ctx, `SELECT id,project_id,attempt_id,operation_id,position,capture_sha256,capture_bytes,CASE WHEN typeof(body_json)='text' AND length(CAST(body_json AS BLOB))<=24576 THEN body_json END FROM review_hosted_observations WHERE project_id=? AND attempt_id=? ORDER BY position DESC LIMIT 1`, o.ProjectID, a.ID).Scan(&id, &project, &attempt, &operation, &position, &checksum, &size, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	observation, err := decodeHostedObservation(body, id, project, attempt, operation, position, checksum, size, a, o)
	if err != nil {
		return nil, err
	}
	if observation.Input.Kind != "allowance_query" || project != o.ProjectID || attempt != a.ID || operation != o.ID {
		return nil, hostedObservationUnavailable()
	}
	if err := hostedObservationQueryBinding(ctx, tx, a, o, observation.Input); err != nil {
		return nil, err
	}
	result.ObservationID, result.CaptureID, result.CapturedAt = observation.ID, observation.Input.CaptureID, observation.Input.ObservedAt
	return result, nil
}
