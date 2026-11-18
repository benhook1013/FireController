package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
	"net/http"
	"strings"
	"time"
)

// Operational proof is authenticated verification of an external/settings fix,
// never a surrogate Git commit. SHA fixes retain their existing contract.
type OperationalFixRequest struct {
	ExpectedRevision *int64  `json:"expected_revision"`
	Finding          int64   `json:"finding"`
	ExpectedDigest   *string `json:"expected_digest"`
	ProofNote        string  `json:"proof_note"`
}

func operationalDigest(note string) string {
	sum := sha256.Sum256([]byte(note))
	return hex.EncodeToString(sum[:])
}

func validOperationalDecision(f FindingDecision) bool {
	p := f.OperationalProof
	if p == nil {
		return true
	}
	at, err := time.Parse(time.RFC3339Nano, p.RecordedAt)
	return f.Disposition == "accepted" && f.RouteID == "" &&
		f.FixSHA == "" && f.ProofNote == "" && f.FixedAt == "" &&
		safeText(p.Note, 2048) && strings.TrimSpace(p.Note) != "" &&
		p.Digest == operationalDigest(p.Note) &&
		identifier.MatchString(p.ActorID) && err == nil && !at.IsZero()
}

func (e *Engine) RecordOperationalFix(ctx context.Context, scope core.Scope, key, id string, request OperationalFixRequest) (json.RawMessage, error) {
	if !identifier.MatchString(id) || !validAdjudicationRevision(request.ExpectedRevision) ||
		request.Finding < 0 || request.Finding >= 100 || request.ExpectedDigest == nil ||
		(*request.ExpectedDigest != "" && !validOperationalDigest(*request.ExpectedDigest)) ||
		!safeText(request.ProofNote, 2048) || strings.TrimSpace(request.ProofNote) == "" {
		return nil, invalid()
	}
	revision, digest := *request.ExpectedRevision, *request.ExpectedDigest
	request.ExpectedRevision, request.ExpectedDigest = &revision, &digest
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.evidence.operational_fix", Target: id, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		_, state, err := loadReviewAdjudication(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return nil, err
		}
		if !adjudicationMatches(state, revision) {
			return nil, adjudicationRevisionConflict(state.Revision)
		}
		if request.Finding >= int64(len(state.Findings)) {
			return nil, invalid()
		}
		f := &state.Findings[request.Finding]
		if f.Disposition != "accepted" || f.FixSHA != "" {
			return nil, conflict()
		}
		current := ""
		if f.OperationalProof != nil {
			current = f.OperationalProof.Digest
		}
		if current != digest {
			return nil, conflict()
		}
		at := stamp()
		f.OperationalProof = &reviews.OperationalProof{Digest: operationalDigest(request.ProofNote), Note: request.ProofNote, ActorID: scope.PrincipalID, RecordedAt: at}
		return e.persistReviewAdjudication(ctx, tx, scope, state, "operational_fix_recorded", request.Finding, request, at)
	})
}

func validOperationalDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (e *Engine) operationalFixEvidenceHTTP(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := e.evidenceHTTPScope(w, r, "write")
	if !ok {
		return
	}
	var wire struct {
		ExpectedRevision *int64  `json:"expected_revision"`
		Finding          *int64  `json:"finding"`
		ExpectedDigest   *string `json:"expected_digest"`
		ProofNote        *string `json:"proof_note"`
	}
	if err := core.ReadJSON(w, r, &wire); err != nil {
		core.WriteError(w, err)
		return
	}
	if wire.ExpectedRevision == nil || wire.Finding == nil || wire.ExpectedDigest == nil || wire.ProofNote == nil {
		core.WriteError(w, invalid())
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	receipt, err := e.RecordOperationalFix(r.Context(), scope, key, id, OperationalFixRequest{ExpectedRevision: wire.ExpectedRevision, Finding: *wire.Finding, ExpectedDigest: wire.ExpectedDigest, ProofNote: *wire.ProofNote})
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, receipt)
}
