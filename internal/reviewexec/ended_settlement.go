package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// settlementDisposition verifies original retained authority, never current
// provider state. External termination is asserted only by a true stop flag.
func settlementDisposition(ctx context.Context, tx *sql.Tx, a Attempt, o Operation, externalStopped bool) (state, reason, event string, err error) {
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
		// False is an explicit local abandonment, never an assertion that the
		// external action stopped. It cannot borrow the older uncertain classes.
		if !externalStopped {
			if err := validateConfirmedHostedSettlement(ctx, tx, a, o); err != nil {
				return "", "", "", err
			}
			if err := validateSettlementObservations(ctx, tx, a, o, false); err != nil {
				return "", "", "", err
			}
			return "cancelled", "operator_abandoned_pending", "review.hosted_settled", nil
		}
		full := o.Reason != "hosted_finished_inventory_unproven"
		// Preserve the existing uncertain capacity/finished-inventory classes.
		// Other issued work needs a retained confirmed trigger, independently of
		// any process-local observer reason or the time spent waiting.
		var observations int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM review_hosted_observations WHERE attempt_id=?`, a.ID).Scan(&observations); err != nil {
			return "", "", "", err
		}
		if a.State == "active" || full && observations != hostedObservationLimit {
			if err := validateConfirmedHostedSettlement(ctx, tx, a, o); err != nil {
				return "", "", "", err
			}
			full = false
		}
		if err := validateSettlementObservations(ctx, tx, a, o, full); err != nil {
			return "", "", "", err
		}
		return "cancelled", "operator_verified_external_stop", "review.hosted_settled", nil
	case "cli":
		if !externalStopped || o.Reason != "coverage_incomplete" || a.Intent != "" {
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

// The original confirmed-trigger transition supplies durable issue authority;
// an allowance query, unknown POST outcome or observer timestamp cannot supply
// it. Current configuration/catalog changes do not prevent cleanup of
// original work. The retained attempt/account and its exact reservation are
// separately checked by the enclosing settlement transaction.
func validateConfirmedHostedSettlement(ctx context.Context, tx *sql.Tx, a Attempt, o Operation) error {
	external, err := strconv.ParseInt(a.ExternalID, 10, 64)
	if err != nil || external <= 0 || strconv.FormatInt(external, 10) != a.ExternalID {
		return settlementUnavailable()
	}
	var body string
	if err := tx.QueryRowContext(ctx, `SELECT body_json FROM review_execution_config WHERE project_id=? AND revision=?`, o.ProjectID, o.ConfigurationRevision).Scan(&body); errors.Is(err, sql.ErrNoRows) {
		return settlementUnavailable()
	} else if err != nil {
		return err
	}
	var original Configuration
	if len(body) > 32<<10 || json.Unmarshal([]byte(body), &original) != nil || validateConfigurationShape(original) != nil || encoded(original) != body || *original.ExpectedRevision != o.ConfigurationRevision-1 || original.Paused || !slices.Contains(original.Channels, "hosted") || !slices.Contains(original.Queue, o.PullNumber) {
		return settlementUnavailable()
	}
	var revision int64
	var reason, confirmedExternal, recorded, admittedAt string
	// The first active transition must confirm this exact trigger; a later
	// arbitrary active transition cannot repair an earlier unconfirmed action.
	err = tx.QueryRowContext(ctx, `SELECT revision,reason,external_id,created_at,(SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=2) FROM review_execution_transitions WHERE attempt_id=? AND state='active' ORDER BY revision LIMIT 1`, a.ID, a.ID).Scan(&revision, &reason, &confirmedExternal, &recorded, &admittedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return settlementUnavailable()
	} else if err != nil {
		return err
	}
	if revision < 3 || revision > a.Revision || reason != "trigger_recorded" && reason != "trigger_posted" && reason != "trigger_recovered" {
		return settlementUnavailable()
	}
	if confirmedExternal != a.ExternalID {
		return settlementUnavailable()
	}
	at, valid := attemptRecordedTime(recorded)
	created, createdOK := attemptRecordedTime(admittedAt)
	current, currentOK := attemptRecordedTime(o.UpdatedAt)
	if !valid || !createdOK || !currentOK || at.Before(created) || at.After(current) {
		return settlementUnavailable()
	}
	return nil
}
