package reviewexec

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

const pendingEvidenceHeader = "X-FC-Review-Blockers"
const pendingEvidenceLimit = 5

// PendingEvidence lists current local obligations, not the historical cause of
// the held operation. Its references never constitute new review permission.
type PendingEvidence struct {
	EvaluatedAt string                `json:"evaluated_at"`
	Items       []PendingEvidenceItem `json:"items"`
	HasMore     bool                  `json:"has_more"`
}
type PendingEvidenceItem struct {
	OperationID      string `json:"operation_id"`
	EvidenceID       string `json:"evidence_id,omitempty"`
	Finding          *int64 `json:"finding,omitempty"`
	RouteID          string `json:"route_id,omitempty"`
	TargetPullNumber int64  `json:"target_pull_number,omitempty"`
	Reason           string `json:"reason"`
}

// Explicit response expansion keeps full-operation reads compatible with
// existing clients. Ordinary reads and non-evidence holds remain unchanged.
func (e *Engine) pendingEvidenceForResponse(r *http.Request, scope core.Scope, id, state, reason string) (*PendingEvidence, error) {
	if r.Header.Get(pendingEvidenceHeader) != "true" || state != "held" || reason != "review_evidence_pending" {
		return nil, nil
	}
	return e.readPendingEvidence(r.Context(), scope, id)
}

func (e *Engine) readPendingEvidence(ctx context.Context, scope core.Scope, id string) (*PendingEvidence, error) {
	var result *PendingEvidence
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		op, err := readOperationBase(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return err
		}
		if op.State != "held" || op.Reason != "review_evidence_pending" {
			return nil
		}
		result = &PendingEvidence{EvaluatedAt: stamp(), Items: []PendingEvidenceItem{}}
		var dirty bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_comparison_proofs WHERE project_id=? AND pull_number=? AND channel=? AND state='dirty')`, op.ProjectID, op.PullNumber, op.Channel).Scan(&dirty); err != nil {
			return err
		}
		if dirty {
			return adjudicationStorageError()
		}
		rows, err := tx.QueryContext(ctx, `SELECT operation_id,evidence_id FROM review_comparison_proofs WHERE project_id=? AND pull_number=? AND channel=? AND state='pending' ORDER BY operation_id LIMIT 6`, op.ProjectID, op.PullNumber, op.Channel)
		if err != nil {
			return err
		}
		type ref struct{ operation, evidence string }
		refs := []ref{}
		for rows.Next() {
			var x ref
			if err = rows.Scan(&x.operation, &x.evidence); err != nil {
				break
			}
			refs = append(refs, x)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		add := func(item PendingEvidenceItem) { result.Items = append(result.Items, item) }
		for _, x := range refs {
			if !hostedObservationID(x.operation, "review_operation_") {
				return adjudicationStorageError()
			}
			item := PendingEvidenceItem{OperationID: x.operation, EvidenceID: x.evidence}
			if x.evidence == "" {
				item.Reason = "capture_missing"
				add(item)
			} else {
				evidence, state, err := loadReviewAdjudication(ctx, tx, op.ProjectID, x.evidence)
				if err != nil {
					return err
				}
				if evidence.OperationID != x.operation || evidence.Anchor.PullNumber != op.PullNumber {
					return adjudicationStorageError()
				}
				ready, err := coverageProof(ctx, tx, evidence, state, op.Channel)
				if err != nil {
					return err
				}
				if ready {
					return adjudicationStorageError()
				}
				before := len(result.Items)
				if !evidence.Capture.Review.Complete || evidence.Capture.Review.Outcome != "completed" {
					item.Reason = "capture_incomplete"
					add(item)
				} else {
					undecided := false
					for _, finding := range state.Findings {
						v := item
						index := finding.Finding
						v.Finding = &index
						switch finding.Disposition {
						case "unadjudicated":
							v.Reason = "finding_undecided"
							undecided = true
						case "accepted":
							if finding.FixSHA == "" {
								v.Reason = "fix_pending"
							}
						case "routed":
							route, err := projectedFindingRoute(ctx, tx, evidence, finding)
							if err != nil {
								return err
							}
							resolved, err := receivingRouteResolved(route)
							if err != nil {
								return err
							}
							if !resolved {
								v.RouteID = route.ID
								v.TargetPullNumber = route.TargetPullNumber
								v.Reason = "route_undecided"
								if route.Disposition == "accepted" {
									v.Reason = "route_fix_pending"
								}
							}
						}
						if v.Reason != "" {
							add(v)
						}
						if len(result.Items) > pendingEvidenceLimit {
							break
						}
					}
					if !state.Finalized && !undecided && len(result.Items) <= pendingEvidenceLimit {
						v := item
						v.Reason = "finalization_pending"
						add(v)
					}
				}
				if len(result.Items) == before {
					return adjudicationStorageError()
				}
			}
			if len(result.Items) > pendingEvidenceLimit {
				result.HasMore = true
				result.Items = result.Items[:pendingEvidenceLimit]
				break
			}
		}
		return nil
	})
	return result, err
}
