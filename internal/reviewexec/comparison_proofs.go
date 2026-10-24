package reviewexec

import (
	"context"
	"database/sql"
	"errors"
)

type comparisonProof struct {
	operation, project      string
	number                  int64
	channel, state          string
	repository              int64
	candidate, base, parent string
	parentNumber            int64
	baseRef, evidence       string
}

const comparisonProofColumns = `operation_id,project_id,pull_number,channel,state,repository_id,candidate_sha,base_sha,parent_sha,parent_pull_number,base_ref,evidence_id`

type comparisonWriter interface {
	EvidenceQuery
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Evaluate exactly one native operation using the same retained authority as
// the former all-history comparison loop. Missing capture remains an obligation.
func deriveComparisonProof(ctx context.Context, q EvidenceQuery, project, id string) (comparisonProof, error) {
	if !identifier.MatchString(project) || !identifier.MatchString(id) {
		return comparisonProof{}, adjudicationStorageError()
	}
	operation, err := readOperation(ctx, q, project, id)
	if err != nil {
		return comparisonProof{}, err
	}
	proof := comparisonProof{operation: id, project: project, number: operation.PullNumber, channel: operation.Channel, state: "pending"}
	if operation.State != "completed" || operation.Channel != "cli" && operation.Channel != "hosted" {
		return proof, adjudicationStorageError()
	}
	rows, err := q.QueryContext(ctx, `SELECT id,attempt_id FROM review_execution_evidence WHERE project_id=? AND operation_id=? ORDER BY id LIMIT 2`, project, id)
	if err != nil {
		return proof, err
	}
	var evidenceID, evidenceAttempt string
	found := 0
	for rows.Next() {
		if err = rows.Scan(&evidenceID, &evidenceAttempt); err != nil {
			break
		}
		found++
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return proof, err
	}
	if found == 0 {
		return proof, nil
	}
	if found != 1 || evidenceAttempt != operation.AttemptID {
		return proof, adjudicationStorageError()
	}
	evidence, state, err := loadReviewAdjudication(ctx, q, project, evidenceID)
	if err != nil {
		return proof, err
	}
	attempt, err := readAttempt(ctx, q, project, operation.AttemptID)
	if err != nil {
		return proof, err
	}
	var repositoryID int64
	if err = q.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil {
		return proof, err
	}
	if validateReviewEvidence(evidence.Capture) != nil || !identifier.MatchString(evidence.ID) || !identifier.MatchString(evidence.AttemptID) || !identifier.MatchString(evidence.OperationID) || !identifier.MatchString(operation.ActorID) || operation.WorkerID != "" && !identifier.MatchString(operation.WorkerID) || evidence.AttemptID != attempt.ID || evidence.OperationID != operation.ID || attempt.OperationID != operation.ID || operation.AttemptID != attempt.ID || attempt.State != "completed" || operation.ConfigurationRevision < 1 ||
		!identifier.MatchString(attempt.Provider) || !identifier.MatchString(attempt.Account) || !hash.MatchString(evidence.CapabilityFingerprint) || evidence.CapabilityFingerprint != operation.CapabilityFingerprint || attempt.Anchor == nil || !validEvidenceAnchor(evidence.Anchor) || *attempt.Anchor != evidence.Anchor || evidence.Capture.ExpectedAnchor != evidence.Anchor || operation.PullNumber != evidence.Anchor.PullNumber || operation.HeadSHA != evidence.Anchor.CandidateSHA || evidence.Anchor.RepositoryID != repositoryID {
		return proof, adjudicationStorageError()
	}
	if operation.Channel == "hosted" && validateHostedEvidenceIdentity(attempt, operation, evidence.Capture) != nil {
		return proof, adjudicationStorageError()
	}
	ready, err := coverageProof(ctx, q, evidence, state, operation.Channel)
	if err != nil {
		return proof, err
	}
	if ready {
		proof.state = "ready"
	}
	a := evidence.Anchor
	proof.repository, proof.candidate, proof.base, proof.parent, proof.parentNumber, proof.baseRef, proof.evidence = a.RepositoryID, a.CandidateSHA, a.BaseSHA, a.ParentSHA, a.ParentPullNumber, a.BaseRef, evidence.ID
	return proof, nil
}

func refreshComparisonProof(ctx context.Context, q comparisonWriter, project, id string) error {
	var state string
	if err := q.QueryRowContext(ctx, `SELECT state FROM review_operations WHERE project_id=? AND id=?`, project, id).Scan(&state); err != nil {
		return err
	}
	if state != "completed" {
		var exists bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_comparison_proofs WHERE operation_id=?)`, id).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return adjudicationStorageError()
		}
		return nil
	}
	proof, err := deriveComparisonProof(ctx, q, project, id)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO review_comparison_proofs(`+comparisonProofColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(operation_id) DO UPDATE SET project_id=excluded.project_id,pull_number=excluded.pull_number,channel=excluded.channel,state=excluded.state,repository_id=excluded.repository_id,candidate_sha=excluded.candidate_sha,base_sha=excluded.base_sha,parent_sha=excluded.parent_sha,parent_pull_number=excluded.parent_pull_number,base_ref=excluded.base_ref,evidence_id=excluded.evidence_id`, proof.operation, proof.project, proof.number, proof.channel, proof.state, proof.repository, proof.candidate, proof.base, proof.parent, proof.parentNumber, proof.baseRef, proof.evidence)
	return err
}

func refreshEvidenceComparison(ctx context.Context, q comparisonWriter, project, evidence string) error {
	var operation string
	if err := q.QueryRowContext(ctx, `SELECT operation_id FROM review_execution_evidence WHERE project_id=? AND id=?`, project, evidence).Scan(&operation); err != nil {
		return err
	}
	return refreshComparisonProof(ctx, q, project, operation)
}

func scanComparisonProof(row interface{ Scan(...any) error }) (comparisonProof, error) {
	var p comparisonProof
	err := row.Scan(&p.operation, &p.project, &p.number, &p.channel, &p.state, &p.repository, &p.candidate, &p.base, &p.parent, &p.parentNumber, &p.baseRef, &p.evidence)
	return p, err
}

// validateComparisonProofSet checks that proofs and completed operations have
// matching identities and scope. It does not validate the derived proof values.
func validateComparisonProofSet(ctx context.Context, q EvidenceQuery) error {
	var invalid int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM review_comparison_proofs p LEFT JOIN review_operations o ON o.id=p.operation_id WHERE p.operation_id IS NULL OR p.operation_id='' OR o.id IS NULL OR o.project_id!=p.project_id OR o.pull_number!=p.pull_number OR o.channel!=p.channel OR o.state!='completed' LIMIT 1`).Scan(&invalid)
	if err == nil {
		return adjudicationStorageError()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var missing int
	err = q.QueryRowContext(ctx, `SELECT 1 FROM review_operations o LEFT JOIN review_comparison_proofs p ON p.operation_id=o.id WHERE o.state='completed' AND p.operation_id IS NULL LIMIT 1`).Scan(&missing)
	if err == nil {
		return adjudicationStorageError()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

// ValidateComparisonProofs is full read-only maintenance validation, not a
// per-command scan. Re-derive credit from actual journals, captures and routes.
func ValidateComparisonProofs(ctx context.Context, q EvidenceQuery) error {
	if err := validateComparisonProofSet(ctx, q); err != nil {
		return err
	}
	last := ""
	for {
		rows, err := q.QueryContext(ctx, `SELECT `+comparisonProofColumns+` FROM review_comparison_proofs WHERE operation_id>? ORDER BY operation_id LIMIT 128`, last)
		if err != nil {
			return err
		}
		var page []comparisonProof
		for rows.Next() {
			p, e := scanComparisonProof(rows)
			if e != nil {
				err = e
				break
			}
			page = append(page, p)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, stored := range page {
			actual, err := deriveComparisonProof(ctx, q, stored.project, stored.operation)
			if err != nil {
				return err
			}
			if stored != actual {
				return adjudicationStorageError()
			}
			last = stored.operation
		}
		if len(page) < 128 {
			break
		}
	}
	return nil
}
