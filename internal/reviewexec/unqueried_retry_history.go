package reviewexec

import (
	"context"
)

// A restart may cancel an unused Hosted retry preparation while preserving its
// original cooldown intent. This is not a cooldown or a new baseline: accept
// only its exact two-row journal and absence of every retained provider effect.
func validateUnqueriedRetryHistory(ctx context.Context, q EvidenceQuery, o Operation, id, baseline string) error {
	a, err := readAttempt(ctx, q, o.ProjectID, id)
	if err != nil {
		return waitDependencyError(err)
	}
	if o.Channel != "hosted" || baseline == "" || id == baseline || id == o.AttemptID || a.OperationID != o.ID || a.ProjectID != o.ProjectID || a.State != "cancelled" || a.Revision != 2 || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" {
		return waitUnavailable()
	}
	first, err := readAttempt(ctx, q, o.ProjectID, baseline)
	if err != nil {
		return waitDependencyError(err)
	}
	if first.OperationID != o.ID || first.ProjectID != o.ProjectID || first.Provider != a.Provider || first.Account != a.Account || first.State != "held" {
		return waitUnavailable()
	}
	for _, table := range []string{"review_hosted_allowance_queries", "review_hosted_observations", "review_hosted_preflight", "review_cli_preflight", "review_execution_evidence", "review_execution_reservations"} {
		var exists bool
		if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE attempt_id=?)", id).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return waitUnavailable()
		}
	}
	var started, ended, baselineEnded string
	var count int
	err = q.QueryRowContext(ctx, `SELECT p.created_at,t.created_at,(SELECT count(*) FROM review_execution_transitions WHERE attempt_id=p.attempt_id)
 FROM review_execution_transitions p JOIN review_execution_transitions t ON t.attempt_id=p.attempt_id AND t.revision=2
 WHERE p.attempt_id=? AND p.revision=1 AND p.state='preparing' AND p.reason='preparing' AND p.external_id=''
 AND t.state='cancelled' AND t.reason='supervisor_interrupted' AND t.external_id=''`, id).Scan(&started, &ended, &count)
	if err != nil {
		return waitDependencyError(err)
	}
	err = q.QueryRowContext(ctx, `SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=? AND state='held' AND reason='hosted_allowance_cooldown'`, baseline, first.Revision).Scan(&baselineEnded)
	if err != nil {
		return waitDependencyError(err)
	}
	start, startOK := attemptRecordedTime(started)
	end, endOK := attemptRecordedTime(ended)
	base, baseOK := attemptRecordedTime(baselineEnded)
	created, createdOK := attemptRecordedTime(o.CreatedAt)
	updated, updatedOK := attemptRecordedTime(o.UpdatedAt)
	if count != 2 || !startOK || !endOK || !baseOK || !createdOK || !updatedOK || base.Before(created) || start.Before(base) || end.Before(start) || end.After(updated) {
		return waitUnavailable()
	}
	return nil
}
