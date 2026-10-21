package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// settlementDisposition verifies original retained authority, never current
// provider state. External termination remains the administrator's assertion.
func settlementDisposition(ctx context.Context, tx *sql.Tx, a Attempt, o Operation) (state, reason, event string, err error) {
	switch o.Channel {
	case "hosted":
		intent, intentErr := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: o.ID, AttemptID: a.ID, HeadSHA: o.HeadSHA})
		if intentErr != nil || intent != a.Intent {
			return "", "", "", settlementUnavailable()
		}
		var evidence int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, a.ID).Scan(&evidence); err != nil {
			return "", "", "", err
		}
		if evidence != 0 {
			return "", "", "", settlementUnavailable()
		}
		full := o.Reason != "hosted_finished_inventory_unproven"
		if err := validateSettlementObservations(ctx, tx, a, o, full); err != nil {
			return "", "", "", err
		}
		return "cancelled", "operator_verified_external_stop", "review.hosted_settled", nil
	case "cli":
		if o.Reason != "coverage_incomplete" || a.Intent != "" {
			return "", "", "", settlementUnavailable()
		}
		if err := validatePartialSettlementEvidence(ctx, tx, a, o); err != nil {
			return "", "", "", err
		}
		return "failed", "cli_partial_settled", "review.cli_settled", nil
	default:
		return "", "", "", settlementUnavailable()
	}
}

func validatePartialSettlementEvidence(ctx context.Context, tx *sql.Tx, a Attempt, o Operation) error {
	var id, project, operation, body, fingerprint string
	err := tx.QueryRowContext(ctx, `SELECT id,project_id,operation_id,CASE WHEN length(CAST(body_json AS BLOB))<=? THEN body_json ELSE '' END,fingerprint FROM review_execution_evidence WHERE attempt_id=?`, evidenceInputLimit+8192, a.ID).Scan(&id, &project, &operation, &body, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return settlementUnavailable()
	}
	if err != nil {
		return err
	}
	var record ReviewEvidence
	if strictjson.Decode([]byte(body), &record) != nil || !hostedObservationID(id, "review_evidence_") || record.ID != id || project != a.ProjectID || record.ProjectID != project || record.AttemptID != a.ID || operation != o.ID || record.OperationID != operation || record.CapabilityFingerprint != o.CapabilityFingerprint || record.Anchor != *a.Anchor || record.Capture.ExpectedAnchor != *a.Anchor || record.Capture.Hosted != nil || record.Capture.Review.Complete || validateReviewEvidence(record.Capture) != nil {
		return settlementUnavailable()
	}
	if _, ok := attemptRecordedTime(record.RecordedAt); !ok {
		return settlementUnavailable()
	}
	canonical, err := json.Marshal(record)
	if err != nil || string(canonical) != body {
		return settlementUnavailable()
	}
	capture, err := json.Marshal(record.Capture)
	if err != nil {
		return settlementUnavailable()
	}
	digest := sha256.Sum256(capture)
	if !hash.MatchString(fingerprint) || hex.EncodeToString(digest[:]) != fingerprint {
		return settlementUnavailable()
	}
	return nil
}
