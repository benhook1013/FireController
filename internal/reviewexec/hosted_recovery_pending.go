package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
)

const pendingHostedRecoverySQL = `SELECT a.project_id,a.id,c.body_json
FROM review_operations o
JOIN review_execution_attempts a ON a.id=o.attempt_id AND a.operation_id=o.id AND a.project_id=o.project_id AND a.state=o.state
JOIN review_execution_config c ON c.project_id=o.project_id AND c.revision=o.configuration_revision
WHERE o.state IN ('uncertain','active') AND o.channel='hosted'
AND a.provider=? AND a.account=? AND o.capability_fingerprint=?
AND json_extract(c.body_json,'$.capability_id')=?
AND (a.state='uncertain' OR EXISTS(
 SELECT 1 FROM review_execution_transitions t WHERE t.attempt_id=a.id AND t.revision=a.revision AND t.state='active' AND t.reason='trigger_recovered' AND t.external_id=a.external_id
)) LIMIT 2`

// HostedRecoveryCandidate selects originally admitted work for observation.
// It grants no permission to post, retry a review or claim completed coverage.
type HostedRecoveryCandidate struct {
	OperationID string
	ProjectID   string
	AttemptID   string
}

// PendingHostedRecovery returns at most one originally bound observation target.
// Live driver-owned active attempts are excluded. Current project pauses and
// caller grants do not erase the need to observe an already-issued action.
func (e *Engine) PendingHostedRecovery(ctx context.Context, capabilityID string) (*HostedRecoveryCandidate, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(capabilityID) {
		return nil, invalid()
	}
	capability, ok := e.capabilities[capabilityID]
	if !ok || capability.ID != capabilityID || !identifier.MatchString(capability.Provider) || !identifier.MatchString(capability.Account) || !hash.MatchString(capability.Fingerprint) || !slices.Contains(capability.Channels, "hosted") {
		return nil, conflict()
	}
	var result *HostedRecoveryCandidate
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// The state index bounds selection to potentially recoverable operations;
		// the account/channel reservation permits only one issued target. Retain
		// a second row so corrupt multiplicity cannot silently choose a target.
		rows, err := tx.QueryContext(ctx, pendingHostedRecoverySQL, capability.Provider, capability.Account, capability.Fingerprint, capabilityID)
		if err != nil {
			return err
		}
		type retainedCandidate struct {
			HostedRecoveryCandidate
			configuration string
		}
		var candidates []retainedCandidate
		for rows.Next() {
			var candidate retainedCandidate
			if err = rows.Scan(&candidate.ProjectID, &candidate.AttemptID, &candidate.configuration); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, candidate)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(candidates) == 0 {
			return nil
		}
		if len(candidates) != 1 {
			return hostedRecoveryUnavailable()
		}
		candidate := candidates[0]
		a, err := readAttempt(ctx, tx, candidate.ProjectID, candidate.AttemptID)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, candidate.ProjectID, a.OperationID)
		if err != nil {
			return err
		}
		var configuration Configuration
		if !identifier.MatchString(candidate.ProjectID) || !hostedObservationID(candidate.AttemptID, "review_attempt_") || a.Revision < 2 || o.Revision < 1 || o.ConfigurationRevision < 1 || len(candidate.configuration) > 32<<10 || json.Unmarshal([]byte(candidate.configuration), &configuration) != nil || validateConfigurationShape(configuration) != nil || encoded(configuration) != candidate.configuration || *configuration.ExpectedRevision != o.ConfigurationRevision-1 || configuration.CapabilityID != capabilityID || configuration.Paused || !slices.Contains(configuration.Channels, "hosted") || !slices.Contains(configuration.Queue, o.PullNumber) {
			return hostedRecoveryUnavailable()
		}
		// Full anchor, intent and final-evidence checks remain owned by the
		// subsequent ReadHostedRecovery call, using this exact target identity.
		result = &HostedRecoveryCandidate{ProjectID: candidate.ProjectID, OperationID: o.ID, AttemptID: candidate.AttemptID}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
