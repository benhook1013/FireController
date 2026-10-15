package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

type CLIDecisionRequest struct {
	ExpectedRevision *int64 `json:"expected_revision"`
	Finding          int64  `json:"finding"`
	Disposition      string `json:"disposition"`
	Reason           string `json:"reason"`
}

type CLIFinalizeRequest struct {
	ExpectedRevision *int64 `json:"expected_revision"`
}

type CLIFixRequest struct {
	ExpectedRevision *int64  `json:"expected_revision"`
	Finding          int64   `json:"finding"`
	ExpectedSHA      *string `json:"expected_sha"`
	FixSHA           string  `json:"fix_sha"`
	ProofNote        string  `json:"proof_note"`
}

type CLICounts struct {
	Found    int64 `json:"found"`
	Accepted int64 `json:"accepted"`
	Routed   int64 `json:"routed"`
}

type CLIFindingDecision struct {
	Finding     int64  `json:"finding"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
	ActorID     string `json:"actor_id"`
	DecidedAt   string `json:"decided_at"`
	FixSHA      string `json:"fix_sha"`
	ProofNote   string `json:"proof_note"`
	FixedAt     string `json:"fixed_at"`
}

// CLIAdjudication contains dispositions and proof references, not finding text.
// FrozenCounts remain the original finalized source counts after corrections.
type CLIAdjudication struct {
	EvidenceID   string               `json:"evidence_id"`
	Revision     int64                `json:"revision"`
	Finalized    bool                 `json:"finalized"`
	FrozenCounts *CLICounts           `json:"frozen_counts"`
	Findings     []CLIFindingDecision `json:"findings"`
}

func adjudicationStorageError() error {
	return core.Fail("storage_unavailable", "Stored review adjudication is unavailable.", 500)
}

// loadCLIAdjudication reads the immutable source and current projection in the
// same snapshot. Source text is used internally and never included in the view.
func loadCLIAdjudication(ctx context.Context, tx *sql.Tx, project, id string) (CLIEvidence, CLIAdjudication, error) {
	var captureBody string
	var stateBody sql.NullString
	var revision sql.NullInt64
	var evidence CLIEvidence
	var state CLIAdjudication
	err := tx.QueryRowContext(ctx, `SELECT e.body_json,s.revision,s.body_json FROM review_execution_evidence e LEFT JOIN review_adjudication_state s ON s.evidence_id=e.id AND s.project_id=e.project_id WHERE e.project_id=? AND e.id=?`, project, id).Scan(&captureBody, &revision, &stateBody)
	if errors.Is(err, sql.ErrNoRows) {
		return evidence, state, core.Fail("not_found", "The review capture is unavailable in this project.", 404)
	}
	if err != nil {
		return evidence, state, err
	}
	if len(captureBody) > evidenceInputLimit+8192 || json.Unmarshal([]byte(captureBody), &evidence) != nil || evidence.ID != id || evidence.ProjectID != project || len(evidence.Capture.Review.Findings) > 100 {
		return evidence, state, adjudicationStorageError()
	}
	if !revision.Valid {
		state = CLIAdjudication{EvidenceID: id, Findings: make([]CLIFindingDecision, len(evidence.Capture.Review.Findings))}
		for i := range state.Findings {
			state.Findings[i] = CLIFindingDecision{Finding: int64(i), Disposition: "unadjudicated"}
		}
		return evidence, state, nil
	}
	// At most 100 entries, each with two 2048-byte text fields; JSON escaping
	// can expand each byte sixfold. This also bounds corrupted stored records.
	if !stateBody.Valid || len(stateBody.String) > 3<<20 || json.Unmarshal([]byte(stateBody.String), &state) != nil || state.EvidenceID != id || state.Revision != revision.Int64 || state.Revision < 1 || len(state.Findings) != len(evidence.Capture.Review.Findings) || state.Finalized != (state.FrozenCounts != nil) {
		return evidence, state, adjudicationStorageError()
	}
	for i, finding := range state.Findings {
		if finding.Finding != int64(i) || finding.Disposition != "unadjudicated" && finding.Disposition != "accepted" && finding.Disposition != "rejected" || !safeText(finding.Reason, 2048) || !safeText(finding.ProofNote, 2048) {
			return evidence, state, adjudicationStorageError()
		}
	}
	return evidence, state, nil
}

func (e *Engine) ReadCLIAdjudication(ctx context.Context, scope core.Scope, evidenceID string) (CLIAdjudication, error) {
	if !identifier.MatchString(evidenceID) {
		return CLIAdjudication{}, invalid()
	}
	var state CLIAdjudication
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		_, state, err = loadCLIAdjudication(ctx, tx, scope.ProjectID, evidenceID)
		return err
	})
	if err != nil {
		return CLIAdjudication{}, err
	}
	return state, nil
}

func validAdjudicationRevision(expected *int64) bool { return expected != nil && *expected >= 0 }

func adjudicationMatches(state CLIAdjudication, expected int64) bool {
	return state.Revision == expected && state.Revision < math.MaxInt64
}

// persistCLIAdjudication atomically changes the disposition view, audit and
// event, plus an installed native browser projection. Mutate creates the receipt;
// projection failure rolls back all changes without altering captured evidence.
func (e *Engine) persistCLIAdjudication(ctx context.Context, tx *sql.Tx, scope core.Scope, state CLIAdjudication, kind string, finding int64, request any, at string) (any, error) {
	prior := state.Revision
	state.Revision++
	body, err := json.Marshal(state)
	if err != nil {
		return nil, adjudicationStorageError()
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO review_adjudication_state(evidence_id,project_id,revision,body_json) VALUES(?,?,?,?) ON CONFLICT(evidence_id) DO UPDATE SET revision=excluded.revision,body_json=excluded.body_json WHERE review_adjudication_state.project_id=excluded.project_id AND review_adjudication_state.revision=?`, state.EvidenceID, scope.ProjectID, state.Revision, string(body), prior)
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, conflict()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_adjudication_changes(evidence_id,revision,project_id,actor_id,kind,finding_index,input_json,created_at) VALUES(?,?,?,?,?,?,?,?)`, state.EvidenceID, state.Revision, scope.ProjectID, scope.PrincipalID, kind, finding, encoded(request), at); err != nil {
		return nil, err
	}
	if err = core.AppendEvent(ctx, tx, scope, "review.evidence."+kind, state.EvidenceID, state.Revision, struct {
		Finding int64 `json:"finding"`
	}{finding}); err != nil {
		return nil, err
	}
	if e.publishAdjudication != nil {
		if err = e.publishAdjudication(ctx, tx, scope, state); err != nil {
			return nil, err
		}
	}
	return Receipt{ID: state.EvidenceID, State: kind, Revision: state.Revision}, nil
}

func (e *Engine) DecideCLIFinding(ctx context.Context, scope core.Scope, key, evidenceID string, request CLIDecisionRequest) (json.RawMessage, error) {
	if !identifier.MatchString(evidenceID) || !validAdjudicationRevision(request.ExpectedRevision) || request.Finding < 0 || request.Finding >= 100 || request.Disposition != "accepted" && request.Disposition != "rejected" || !safeText(request.Reason, 2048) || strings.TrimSpace(request.Reason) == "" {
		return nil, invalid()
	}
	// Copy pointer values so the callback and receipt fingerprint use one input.
	expected := *request.ExpectedRevision
	request.ExpectedRevision = &expected
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.evidence.decide", Target: evidenceID, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		_, state, err := loadCLIAdjudication(ctx, tx, scope.ProjectID, evidenceID)
		if err != nil {
			return nil, err
		}
		if !adjudicationMatches(state, expected) {
			return nil, conflict()
		}
		if request.Finding >= int64(len(state.Findings)) {
			return nil, invalid()
		}
		f := &state.Findings[request.Finding]
		if request.Disposition == "rejected" && f.FixSHA != "" {
			return nil, conflict()
		}
		at := stamp()
		f.Disposition, f.Reason, f.ActorID, f.DecidedAt = request.Disposition, request.Reason, scope.PrincipalID, at
		return e.persistCLIAdjudication(ctx, tx, scope, state, "adjudicated", request.Finding, request, at)
	})
}

func (e *Engine) FinalizeCLI(ctx context.Context, scope core.Scope, key, evidenceID string, request CLIFinalizeRequest) (json.RawMessage, error) {
	if !identifier.MatchString(evidenceID) || !validAdjudicationRevision(request.ExpectedRevision) {
		return nil, invalid()
	}
	expected := *request.ExpectedRevision
	request.ExpectedRevision = &expected
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.evidence.finalize", Target: evidenceID, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		evidence, state, err := loadCLIAdjudication(ctx, tx, scope.ProjectID, evidenceID)
		if err != nil {
			return nil, err
		}
		if !adjudicationMatches(state, expected) || state.Finalized || !evidence.Capture.Review.Complete || evidence.Capture.Review.Outcome != "completed" {
			return nil, conflict()
		}
		a, err := readAttempt(ctx, tx, scope.ProjectID, evidence.AttemptID)
		if err != nil {
			return nil, err
		}
		if a.State != "completed" {
			return nil, conflict()
		}
		counts := CLICounts{Found: int64(len(state.Findings))}
		for _, f := range state.Findings {
			switch f.Disposition {
			case "accepted":
				counts.Accepted++
			case "rejected":
			default:
				return nil, conflict()
			}
		}
		state.Finalized, state.FrozenCounts = true, &counts
		return e.persistCLIAdjudication(ctx, tx, scope, state, "finalized", -1, request, stamp())
	})
}

func (e *Engine) RecordCLIFix(ctx context.Context, scope core.Scope, key, evidenceID string, request CLIFixRequest) (json.RawMessage, error) {
	if !identifier.MatchString(evidenceID) || !validAdjudicationRevision(request.ExpectedRevision) || request.Finding < 0 || request.Finding >= 100 || request.ExpectedSHA == nil || *request.ExpectedSHA != "" && !sha.MatchString(*request.ExpectedSHA) || !sha.MatchString(request.FixSHA) || !safeText(request.ProofNote, 2048) || strings.TrimSpace(request.ProofNote) == "" {
		return nil, invalid()
	}
	expected, expectedSHA := *request.ExpectedRevision, *request.ExpectedSHA
	request.ExpectedRevision, request.ExpectedSHA = &expected, &expectedSHA
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.evidence.fix", Target: evidenceID, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		_, state, err := loadCLIAdjudication(ctx, tx, scope.ProjectID, evidenceID)
		if err != nil {
			return nil, err
		}
		if !adjudicationMatches(state, expected) {
			return nil, conflict()
		}
		if request.Finding >= int64(len(state.Findings)) {
			return nil, invalid()
		}
		f := &state.Findings[request.Finding]
		if f.Disposition != "accepted" || f.FixSHA != expectedSHA {
			return nil, conflict()
		}
		at := stamp()
		f.FixSHA, f.ProofNote, f.FixedAt = request.FixSHA, request.ProofNote, at
		return e.persistCLIAdjudication(ctx, tx, scope, state, "fix_recorded", request.Finding, request, at)
	})
}
