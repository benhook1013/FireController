package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
)

// PendingCLICaptureRecovery selects one issued CLI attempt whose raw artifact
// may have survived a failed evidence write. It grants no process/provider work.
// The caller must verify the original private capture and owned Git workspace.
func (e *Engine) PendingCLICaptureRecovery(ctx context.Context, capabilityID string) (*Attempt, error) {
	items, err := e.PendingCLICaptureRecoveries(ctx, capabilityID, "")
	if err != nil || len(items) == 0 {
		return nil, err
	}
	return &items[0], nil
}

// PendingCLICaptureRecoveries reads one bounded keyset page. Runtime rotates the
// cursor after every candidate, so an unrecoverable capture cannot starve others.
func (e *Engine) PendingCLICaptureRecoveries(ctx context.Context, capabilityID, after string) ([]Attempt, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(capabilityID) {
		return nil, invalid()
	}
	capability, ok := e.capabilities[capabilityID]
	if !ok || !slices.Contains(capability.Channels, "cli") {
		return nil, conflict()
	}
	var result []Attempt
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT a.project_id,a.id,c.body_json
FROM review_operations o
JOIN review_execution_attempts a ON a.id=o.attempt_id AND a.operation_id=o.id AND a.project_id=o.project_id AND a.state=o.state
JOIN review_execution_config c ON c.project_id=o.project_id AND c.revision=o.configuration_revision
JOIN review_execution_reservations r ON r.attempt_id=a.id AND r.provider=a.provider AND r.account=a.account AND r.action_class='cli' AND r.pull_number=o.pull_number
LEFT JOIN review_execution_evidence v ON v.attempt_id=a.id
WHERE o.channel='cli' AND o.state='uncertain' AND (r.execution_ended_at='' OR v.id IS NULL OR (o.reason='capture_invalid' AND json_valid(v.body_json) AND json_extract(v.body_json,'$.capture.review.Outcome')='uncertain' AND json_array_length(v.body_json,'$.capture.review.Findings')=0))
AND a.provider=? AND a.account=? AND o.capability_fingerprint=?
AND json_valid(c.body_json) AND json_extract(c.body_json,'$.capability_id')=? AND a.id>? ORDER BY a.id LIMIT 16`, capability.Provider, capability.Account, capability.Fingerprint, capabilityID, after)
		if err != nil {
			return err
		}
		type candidate struct{ project, id, configuration string }
		var candidates []candidate
		for rows.Next() {
			var c candidate
			if err = rows.Scan(&c.project, &c.id, &c.configuration); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || len(candidates) == 0 {
			return err
		}
		for _, c := range candidates {
			a, err := readAttempt(ctx, tx, c.project, c.id)
			if err != nil {
				return err
			}
			o, err := readOperation(ctx, tx, c.project, a.OperationID)
			if err != nil {
				return err
			}
			var configuration Configuration
			if len(c.configuration) > 32<<10 || json.Unmarshal([]byte(c.configuration), &configuration) != nil || validateConfigurationShape(configuration) != nil || encoded(configuration) != c.configuration || *configuration.ExpectedRevision != o.ConfigurationRevision-1 || configuration.CapabilityID != capabilityID || configuration.Paused || !slices.Contains(configuration.Channels, "cli") || !slices.Contains(configuration.Queue, o.PullNumber) || a.Revision < 3 || a.Anchor == nil || !validEvidenceAnchor(*a.Anchor) || a.Anchor.PullNumber != o.PullNumber || a.Anchor.CandidateSHA != o.HeadSHA || a.ExternalID != "" {
				return adjudicationStorageError()
			}
			var repositoryID int64
			if err = tx.QueryRowContext(ctx, "SELECT github_repository_id FROM projects WHERE id=?", c.project).Scan(&repositoryID); err != nil {
				return err
			}
			var reserved bool
			if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM review_execution_reservations WHERE attempt_id=? AND repository_id=?)", a.ID, a.Anchor.RepositoryID).Scan(&reserved); err != nil {
				return err
			}
			if repositoryID != a.Anchor.RepositoryID || !reserved {
				return adjudicationStorageError()
			}
			result = append(result, a)
		}
		return nil
	})
	return result, err
}
