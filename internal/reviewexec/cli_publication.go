package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

var publicationEvidenceID = regexp.MustCompile(`^review_evidence_[0-9a-f]{32}$`)

func publicationConflict() error {
	return core.Fail("observation_conflict", "Native review publication must retain its admitted evidence identity and current adjudication.", 409)
}

// PublishEvidence publishes trusted retained evidence, never caller-supplied finding
// text. Publication and retry reconciliation share the authoritative ledger's
// serialized transaction. This does not settle execution or finalize findings.
// A nil metadata argument preserves the current stored PR facts, refusing to
// infer missing facts from the older capture or obtain a provider observation.
func (e *Engine) PublishEvidence(ctx context.Context, project, evidenceID string, metadata *reviews.PullInput) (reviews.Receipt, error) {
	if !identifier.MatchString(project) || !publicationEvidenceID.MatchString(evidenceID) {
		return reviews.Receipt{}, invalid()
	}
	var supplied reviews.PullInput
	useCurrent := metadata == nil
	if metadata != nil {
		supplied = *metadata
		if len(supplied.Runs) != 0 || len(supplied.Attempts) != 0 {
			return reviews.Receipt{}, invalid()
		}
	}
	var receipt reviews.Receipt
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		evidence, state, err := loadReviewAdjudication(ctx, tx, project, evidenceID)
		if err != nil {
			return err
		}
		if err := validateReviewEvidence(evidence.Capture); err != nil {
			return adjudicationStorageError()
		}
		attempt, err := readAttempt(ctx, tx, project, evidence.AttemptID)
		if err != nil {
			return err
		}
		operation, err := readOperation(ctx, tx, project, evidence.OperationID)
		if err != nil {
			return err
		}
		var repositoryID int64
		if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil {
			return err
		}
		if evidence.ProjectID != project || attempt.ProjectID != project || operation.ProjectID != project || attempt.OperationID != operation.ID || operation.AttemptID != attempt.ID || operation.Channel != evidenceChannel(evidence.Capture) || evidence.CapabilityFingerprint != operation.CapabilityFingerprint || !hash.MatchString(evidence.CapabilityFingerprint) || !identifier.MatchString(attempt.Provider) || attempt.Anchor == nil || *attempt.Anchor != evidence.Anchor || evidence.Capture.ExpectedAnchor != evidence.Anchor || repositoryID != evidence.Anchor.RepositoryID || operation.PullNumber != evidence.Anchor.PullNumber || operation.HeadSHA != evidence.Anchor.CandidateSHA {
			return publicationConflict()
		}
		if operation.Channel == "hosted" && (attempt.State != "active" && attempt.State != "uncertain" && attempt.State != "completed" || validateHostedEvidenceIdentity(attempt, operation, evidence.Capture) != nil) {
			return publicationConflict()
		}
		pull := supplied
		if useCurrent {
			pull, err = currentReviewPull(ctx, tx, project, evidence.Anchor.PullNumber)
			if err != nil {
				return err
			}
		}
		if pull.Number != operation.PullNumber {
			return publicationConflict()
		}
		run, err := publicationRun(evidence, state, attempt)
		if err != nil {
			return err
		}
		if err := attachFindingRoutes(ctx, tx, evidence, state, &run); err != nil {
			return err
		}
		current, exists, err := publicationReplay(ctx, tx, project, pull.Number, run)
		if err != nil {
			return err
		}
		if exists {
			receipt = current
			return nil
		}
		receipt, err = reviews.PublishNative(ctx, tx, core.Scope{ProjectID: project, PrincipalID: operation.ActorID, WorkerID: operation.WorkerID}, reviews.NativePublication{Pull: pull, Run: &run})
		return err
	})
	if err != nil {
		return reviews.Receipt{}, err
	}
	return receipt, nil
}

// currentReviewPull reads only current stored metadata, never capture-era PR
// facts or provider state. Callers must use the same transaction for publication.
func currentReviewPull(ctx context.Context, tx *sql.Tx, project string, number int64) (reviews.PullInput, error) {
	var pull reviews.PullInput
	err := tx.QueryRowContext(ctx, `SELECT p.number,p.title,p.state,p.head_sha,p.base_ref,p.base_sha,p.ci_state,p.mergeability,p.complete
		FROM review_workspace w JOIN review_pulls p ON p.project_id=w.project_id AND p.snapshot_id=w.snapshot_id
		WHERE w.project_id=? AND p.number=?`, project, number).Scan(&pull.Number, &pull.Title, &pull.State, &pull.HeadSHA, &pull.BaseRef, &pull.BaseSHA, &pull.CIState, &pull.Mergeability, &pull.Complete)
	if errors.Is(err, sql.ErrNoRows) {
		return reviews.PullInput{}, core.Fail("review_projection_missing", "The current review workspace has no stored facts for this pull request.", 409)
	}
	return pull, err
}

func publicationRun(evidence ReviewEvidence, state ReviewAdjudication, attempt Attempt) (reviews.RunInput, error) {
	capture := evidence.Capture
	run := reviews.RunInput{SourceRunID: evidence.ID, Channel: evidenceChannel(capture), HeadSHA: evidence.Anchor.CandidateSHA, Reviewer: attempt.Provider, Scope: "admitted comparison", Outcome: capture.Review.Outcome, Attributable: capture.Review.Complete, Complete: capture.Review.Complete, StartedAt: capture.StartedAt, FinishedAt: capture.FinishedAt, Found: capture.Review.ReportedFindings, Finalized: state.Finalized, CoverageLimits: []string{}, Findings: make([]reviews.Finding, 0, len(state.Findings))}
	if !run.Complete {
		run.CoverageLimits = []string{"Incomplete CLI capture; retained findings do not establish completed review coverage."}
		if run.Found < int64(len(capture.Review.Findings)) {
			run.Found = int64(len(capture.Review.Findings))
		}
	}
	for i, source := range capture.Review.Findings {
		decision := state.Findings[i]
		if decision.Disposition == "accepted" {
			run.Accepted++
		} else if decision.Disposition == "routed" {
			run.Routed++
		}
		run.Findings = append(run.Findings, reviews.Finding{SourceFindingID: projectionFindingID(evidence.ID, int64(i)), Title: source.Title, Detail: source.Detail, Severity: source.Severity, Disposition: decision.Disposition, DecisionReason: decision.Reason, Actor: decision.ActorID, DecidedAt: decision.DecidedAt, FixSHA: decision.FixSHA, ProofNote: decision.ProofNote, ResolvedAt: decision.FixedAt})
	}
	if state.Finalized {
		counts := state.FrozenCounts
		if !run.Complete || run.Outcome != "completed" || attempt.State != "completed" || counts == nil || counts.Found != int64(len(run.Findings)) || counts.Accepted < 0 || counts.Routed < 0 || counts.Accepted > counts.Found-counts.Routed {
			return run, publicationConflict()
		}
		run.Found, run.Accepted, run.Routed = counts.Found, counts.Accepted, counts.Routed
	}
	return run, nil
}

// Compare the complete current native source and effective decisions. An
// imported producer with the same external ID is a separate source. A native
// source omitted from current history cannot be silently appended again.
func publicationReplay(ctx context.Context, tx *sql.Tx, project string, number int64, expected reviews.RunInput) (reviews.Receipt, bool, error) {
	var receipt reviews.Receipt
	var actual reviews.RunInput
	var runID, limits string
	var actualNumber, fixed int64
	err := tx.QueryRowContext(ctx, `SELECT w.snapshot_id,w.revision,r.pull_number,r.id,r.source_run_id,r.channel,r.head_sha,r.reviewer,r.scope,r.outcome,r.attributable,r.complete,r.started_at,r.finished_at,r.found,r.accepted,r.routed,r.finalized,r.fixed,r.coverage_limits_json
		FROM review_workspace w JOIN review_runs r ON r.project_id=w.project_id AND r.snapshot_id=w.snapshot_id
		WHERE w.project_id=? AND r.source='native-executor' AND r.source_run_id=?`, project, expected.SourceRunID).Scan(&receipt.ID, &receipt.Revision, &actualNumber, &runID, &actual.SourceRunID, &actual.Channel, &actual.HeadSHA, &actual.Reviewer, &actual.Scope, &actual.Outcome, &actual.Attributable, &actual.Complete, &actual.StartedAt, &actual.FinishedAt, &actual.Found, &actual.Accepted, &actual.Routed, &actual.Finalized, &fixed, &limits)
	if errors.Is(err, sql.ErrNoRows) {
		var historical bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_runs WHERE project_id=? AND source='native-executor' AND source_run_id=?)`, project, expected.SourceRunID).Scan(&historical); err != nil {
			return reviews.Receipt{}, false, err
		}
		if historical {
			return reviews.Receipt{}, false, publicationConflict()
		}
		return reviews.Receipt{}, false, nil
	}
	if err != nil {
		return reviews.Receipt{}, false, err
	}
	if actualNumber != number || len(limits) > 100<<10 || json.Unmarshal([]byte(limits), &actual.CoverageLimits) != nil {
		return reviews.Receipt{}, false, publicationConflict()
	}
	actual.Findings = []reviews.Finding{}
	rows, err := tx.QueryContext(ctx, `SELECT source_finding_id,title,detail,severity,disposition,decision_reason,actor,decided_at,fix_sha,proof_note,resolved_at,route_json FROM review_findings WHERE project_id=? AND snapshot_id=? AND pull_number=? AND run_id=? ORDER BY position LIMIT 101`, project, receipt.ID, number, runID)
	if err != nil {
		return reviews.Receipt{}, false, err
	}
	for rows.Next() {
		var finding reviews.Finding
		var routeJSON string
		if err = rows.Scan(&finding.SourceFindingID, &finding.Title, &finding.Detail, &finding.Severity, &finding.Disposition, &finding.DecisionReason, &finding.Actor, &finding.DecidedAt, &finding.FixSHA, &finding.ProofNote, &finding.ResolvedAt, &routeJSON); err != nil {
			break
		}
		if len(routeJSON) > 32<<10 || json.Unmarshal([]byte(routeJSON), &finding.Route) != nil || encoded(finding.Route) != routeJSON {
			err = publicationConflict()
			break
		}
		actual.Findings = append(actual.Findings, finding)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return reviews.Receipt{}, false, err
	}
	var expectedFixed int64
	for _, finding := range expected.Findings {
		if finding.Disposition == "accepted" && finding.FixSHA != "" {
			expectedFixed++
		}
	}
	if len(actual.Findings) > 100 || fixed != expectedFixed || encoded(actual) != encoded(expected) {
		return reviews.Receipt{}, false, publicationConflict()
	}
	receipt.Outcome = "observed"
	return receipt, true, nil
}
