package reviewexec

import (
	"context"
	"database/sql"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// ReadHostedPostingBoundary returns the exact local admission instant for
// trusted retained-source comparison. It neither authorizes posting nor reads
// provider/current capability facts. Provider comment attribution continues to
// use the separately floored boundary returned by ReadHostedRecovery.
func (e *Engine) ReadHostedPostingBoundary(ctx context.Context, project, attemptID string) (time.Time, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(project) || !hostedObservationID(attemptID, "review_attempt_") {
		return time.Time{}, invalid()
	}
	var result time.Time
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, o, err := readHostedPostingAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		result, err = readHostedAdmissionTime(ctx, tx, a, o)
		return err
	})
	if err != nil {
		return time.Time{}, err
	}
	return result, nil
}

// Both trusted reads validate the same admitted operation/attempt identity in
// their read snapshot; the exact-boundary reader needs no capability catalog.
func readHostedPostingAttempt(ctx context.Context, tx *sql.Tx, project, attemptID string) (Attempt, Operation, error) {
	a, err := readAttempt(ctx, tx, project, attemptID)
	if err != nil {
		return Attempt{}, Operation{}, err
	}
	o, err := readOperation(ctx, tx, project, a.OperationID)
	if err != nil {
		return Attempt{}, Operation{}, err
	}
	if o.Channel != "hosted" || o.State != a.State || o.AttemptID != a.ID || a.ID != attemptID || a.ProjectID != project || o.ProjectID != project || a.OperationID != o.ID {
		return Attempt{}, Operation{}, conflict()
	}
	switch a.State {
	case "posting", "active", "uncertain", "completed":
	default:
		return Attempt{}, Operation{}, conflict()
	}
	if a.Revision < 2 || o.Revision < 1 || o.ConfigurationRevision < 1 || !hash.MatchString(o.CapabilityFingerprint) || a.Anchor == nil || !validEvidenceAnchor(*a.Anchor) || a.Anchor.PullNumber != o.PullNumber || a.Anchor.CandidateSHA != o.HeadSHA {
		return Attempt{}, Operation{}, hostedRecoveryUnavailable()
	}
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: o.ID, AttemptID: a.ID, HeadSHA: o.HeadSHA})
	if err != nil || a.Intent != intent {
		return Attempt{}, Operation{}, hostedRecoveryUnavailable()
	}
	var repositoryID int64
	if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil {
		return Attempt{}, Operation{}, err
	}
	if repositoryID != a.Anchor.RepositoryID {
		return Attempt{}, Operation{}, conflict()
	}

	return a, o, nil
}

func readHostedAdmissionTime(ctx context.Context, tx *sql.Tx, a Attempt, o Operation) (time.Time, error) {
	// The primary key bounds this to the selected attempt. Read two matches so a
	// second admitted row cannot be silently ignored in favour of revision two.
	rows, err := tx.QueryContext(ctx, `SELECT revision,state,external_id,created_at FROM review_execution_transitions WHERE attempt_id=? AND reason='admitted' ORDER BY revision LIMIT 2`, a.ID)
	if err != nil {
		return time.Time{}, err
	}
	count := 0
	var admittedAt time.Time
	for rows.Next() {
		var revision int64
		var state, external, created string
		if err = rows.Scan(&revision, &state, &external, &created); err != nil {
			rows.Close()
			return time.Time{}, err
		}
		count++
		instant, valid := hostedObservationTime(created)
		if count != 1 || revision != 2 || state != "posting" || external != "" || !valid {
			rows.Close()
			return time.Time{}, hostedRecoveryUnavailable()
		}
		admittedAt = instant
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return time.Time{}, err
	}
	// Retained history is independent of the current clock, including after rollback.
	created, valid := hostedObservationTime(o.CreatedAt)
	if count != 1 || !valid || admittedAt.Before(created) {
		return time.Time{}, hostedRecoveryUnavailable()
	}

	return admittedAt, nil
}
