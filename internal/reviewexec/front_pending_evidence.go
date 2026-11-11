package reviewexec

import "context"

// Only inspect pending proofs for this front, never provider captures or a
// recent-history window. Missing adjudication (including a missing capture)
// and non-boolean finalization cannot establish a fixes-only obligation.
const frontPendingFixesSQL = `SELECT count(*) > 0 AND
 coalesce(min(coalesce(json_type(s.body_json,'$.finalized')='true',0)),0)
 FROM review_comparison_proofs p
 LEFT JOIN review_adjudication_state s ON s.evidence_id=p.evidence_id AND s.project_id=p.project_id
 WHERE p.project_id=? AND p.pull_number=? AND p.channel=? AND p.state='pending'`

// frontPendingFixes specializes only the read-only front explanation, after
// channelProgress has rejected dirty proofs in the same transaction. The v2
// ledger validates coverageProof on every source mutation: a finalized pending
// proof can only be waiting for accepted fixes. Mixed/unfinalized obligations
// retain the scheduler's generic reason. This is not an admission decision.
func frontPendingFixes(ctx context.Context, q EvidenceQuery, project string, pull int64, channel string) (bool, error) {
	var fixesPending bool
	err := q.QueryRowContext(ctx, frontPendingFixesSQL, project, pull, channel).Scan(&fixesPending)
	return fixesPending, err
}
