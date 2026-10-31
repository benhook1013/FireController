package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// RecoverRetainedCLI finishes only the local publication and settlement of an
// uncertain original CLI attempt with already validated complete evidence.
// It has no provider, process, source-refresh or evidence-upload interface.
// Missing/partial captures retain their reservation and need separate recovery.
func (e *Engine) RecoverRetainedCLI(ctx context.Context, capabilityID string) error {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(capabilityID) {
		return invalid()
	}
	capability, ok := e.capabilities[capabilityID]
	if !ok || !slices.Contains(capability.Channels, "cli") {
		return conflict()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var attempt Attempt
	var evidenceID string
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// The account reservation admits at most one issued CLI attempt. Select
		// only its original capability; a replaced installation cannot adopt it.
		rows, err := tx.QueryContext(ctx, `SELECT a.project_id,a.id,c.body_json
FROM review_operations o
JOIN review_execution_attempts a ON a.id=o.attempt_id AND a.operation_id=o.id AND a.project_id=o.project_id AND a.state=o.state
JOIN review_execution_config c ON c.project_id=o.project_id AND c.revision=o.configuration_revision
JOIN review_execution_evidence v ON v.attempt_id=a.id
WHERE o.channel='cli' AND o.state='uncertain' AND a.provider=? AND a.account=? AND o.capability_fingerprint=?
AND json_valid(c.body_json) AND json_extract(c.body_json,'$.capability_id')=? LIMIT 2`, capability.Provider, capability.Account, capability.Fingerprint, capabilityID)
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
		if len(candidates) != 1 {
			return adjudicationStorageError()
		}
		c := candidates[0]
		a, err := readAttempt(ctx, tx, c.project, c.id)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, c.project, a.OperationID)
		if err != nil {
			return err
		}
		var configuration Configuration
		if len(c.configuration) > 32<<10 || json.Unmarshal([]byte(c.configuration), &configuration) != nil || validateConfigurationShape(configuration) != nil || encoded(configuration) != c.configuration || *configuration.ExpectedRevision != o.ConfigurationRevision-1 || configuration.CapabilityID != capabilityID || configuration.Paused || !slices.Contains(configuration.Channels, "cli") || !slices.Contains(configuration.Queue, o.PullNumber) || a.Anchor == nil || !validEvidenceAnchor(*a.Anchor) || a.Revision < 3 || o.Revision < 1 || o.AttemptID != a.ID || o.State != a.State || o.Channel != "cli" || a.ExternalID != "" {
			return adjudicationStorageError()
		}
		var id, fingerprint string
		if err := tx.QueryRowContext(ctx, `SELECT id,fingerprint FROM review_execution_evidence WHERE attempt_id=?`, a.ID).Scan(&id, &fingerprint); err != nil {
			return err
		}
		evidence, _, err := loadReviewAdjudication(ctx, tx, c.project, id)
		if err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(encoded(evidence.Capture)))
		if fingerprint != hex.EncodeToString(digest[:]) {
			return adjudicationStorageError()
		}
		var repositoryID int64
		if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, c.project).Scan(&repositoryID); err != nil {
			return err
		}
		var reserved bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_execution_reservations WHERE attempt_id=? AND provider=? AND account=? AND action_class='cli' AND repository_id=? AND pull_number=?)`, a.ID, capability.Provider, capability.Account, repositoryID, o.PullNumber).Scan(&reserved); err != nil {
			return err
		}
		if !reserved || repositoryID != a.Anchor.RepositoryID || o.HeadSHA != a.Anchor.CandidateSHA || o.PullNumber != a.Anchor.PullNumber || evidence.ProjectID != c.project || evidence.AttemptID != a.ID || evidence.OperationID != o.ID || evidence.CapabilityFingerprint != capability.Fingerprint || evidence.Anchor != *a.Anchor || evidence.Capture.ExpectedAnchor != *a.Anchor || evidence.Capture.Hosted != nil || validateReviewEvidence(evidence.Capture) != nil {
			return adjudicationStorageError()
		}
		if !evidence.Capture.Review.Complete || evidence.Capture.Review.Outcome != "completed" {
			return nil
		}
		attempt, evidenceID = a, evidence.ID
		return nil
	})
	if err != nil || evidenceID == "" {
		return err
	}
	// Retry exact retained publication, preserving current PR facts and any
	// decisions already made. Missing display metadata never invents facts.
	if _, err := e.PublishEvidence(ctx, attempt.ProjectID, evidenceID, nil); err != nil {
		var failure *core.Error
		if !errors.As(err, &failure) || failure.Code != "review_projection_missing" {
			return err
		}
	}
	_, err = e.Advance(ctx, attempt.ProjectID, attempt.ID, attempt.Revision, "completed", "cli_evidence_recovered", "")
	return err
}
