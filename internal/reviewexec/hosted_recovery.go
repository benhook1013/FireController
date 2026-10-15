package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// HostedRecoveryContext identifies already-authorized hosted work for trusted
// observation only. It never authorizes a post, retry or another provider action.
type HostedRecoveryContext struct {
	Operation       Operation
	Attempt         Attempt
	EarliestPosting time.Time
	EvidenceID      string
}

func hostedRecoveryUnavailable() error {
	return core.Fail("storage_unavailable", "Stored hosted recovery facts are unavailable.", 500)
}

// ReadHostedRecovery reads retained admission and the original capability binding
// in one finite snapshot. Current actor access, latest project configuration and
// policy pauses deliberately do not prevent observing already-authorized work.
func (e *Engine) ReadHostedRecovery(ctx context.Context, project, attemptID, capabilityID string) (HostedRecoveryContext, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(project) || !hostedObservationID(attemptID, "review_attempt_") || !identifier.MatchString(capabilityID) {
		return HostedRecoveryContext{}, invalid()
	}
	capability, ok := e.capabilities[capabilityID]
	if !ok || !slices.Contains(capability.Channels, "hosted") {
		return HostedRecoveryContext{}, conflict()
	}
	var result HostedRecoveryContext
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, o, err := readHostedPostingAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		var configBody string
		err = tx.QueryRowContext(ctx, `SELECT body_json FROM review_execution_config WHERE project_id=? AND revision=?`, project, o.ConfigurationRevision).Scan(&configBody)
		if errors.Is(err, sql.ErrNoRows) {
			return hostedRecoveryUnavailable()
		}
		if err != nil {
			return err
		}
		var configuration Configuration
		if len(configBody) > 32<<10 || json.Unmarshal([]byte(configBody), &configuration) != nil || validateConfigurationShape(configuration) != nil || encoded(configuration) != configBody || *configuration.ExpectedRevision != o.ConfigurationRevision-1 || configuration.Paused || !slices.Contains(configuration.Channels, "hosted") || !slices.Contains(configuration.Queue, o.PullNumber) {
			return hostedRecoveryUnavailable()
		}
		if configuration.CapabilityID != capabilityID || capability.ID != configuration.CapabilityID || capability.Fingerprint != o.CapabilityFingerprint || capability.Provider != a.Provider || capability.Account != a.Account {
			return conflict()
		}
		admittedAt, err := readHostedAdmissionTime(ctx, tx, a, o)
		if err != nil {
			return err
		}
		// GitHub comment timestamps have second precision. Compare the retained
		// nanosecond timestamp first, then floor only the returned matching boundary.
		anchor := *a.Anchor
		a.Anchor = &anchor
		evidenceID, err := readHostedRecoveryEvidence(ctx, tx, a, o)
		if err != nil {
			return err
		}
		result = HostedRecoveryContext{Operation: o, Attempt: a, EarliestPosting: admittedAt.Truncate(time.Second), EvidenceID: evidenceID}
		return nil
	})
	if err != nil {
		return HostedRecoveryContext{}, err
	}
	return result, nil
}

// A final capture may have committed before publication or settlement failed.
// Its identity is recovery input, independent of any finding/finalization credit.
func readHostedRecoveryEvidence(ctx context.Context, tx *sql.Tx, a Attempt, o Operation) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,project_id,operation_id FROM review_execution_evidence WHERE attempt_id=? ORDER BY id LIMIT 2`, a.ID)
	if err != nil {
		return "", err
	}
	count := 0
	id := ""
	for rows.Next() {
		var project, operation string
		if err = rows.Scan(&id, &project, &operation); err != nil {
			rows.Close()
			return "", err
		}
		count++
		if count > 1 || project != a.ProjectID || operation != o.ID || !hostedObservationID(id, "review_evidence_") {
			rows.Close()
			return "", hostedRecoveryUnavailable()
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", err
	}
	if count == 0 {
		return "", nil
	}
	evidence, _, err := loadReviewAdjudication(ctx, tx, a.ProjectID, id)
	if err != nil {
		return "", err
	}
	if evidence.AttemptID != a.ID || evidence.OperationID != o.ID || evidence.CapabilityFingerprint != o.CapabilityFingerprint || evidence.Anchor != *a.Anchor || evidence.Capture.ExpectedAnchor != *a.Anchor || validateReviewEvidence(evidence.Capture) != nil || validateHostedEvidenceIdentity(a, o, evidence.Capture) != nil {
		return "", hostedRecoveryUnavailable()
	}
	return id, nil
}
