package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

const externalSettlementMaxRuns = 10
const externalSettlementMaxFindings = 100

var externalSettlementRunID = regexp.MustCompile("^review_run_[0-9a-f]{32}$")

type ExternalHostedSettlement struct {
	CountRound *bool                       `json:"count_round"`
	Runs       []ExternalHostedSelectedRun `json:"runs"`
}

type ExternalHostedSelectedRun struct {
	RunID    string                   `json:"run_id"`
	Findings []ExternalHostedDecision `json:"findings"`
}

type ExternalHostedDecision struct {
	FindingID   string `json:"finding_id"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
	FixSHA      string `json:"fix_sha,omitempty"`
	ProofNote   string `json:"proof_note,omitempty"`
}

func (r ExternalHostedSettlement) valid() bool {
	if r.CountRound == nil || len(r.Runs) == 0 || len(r.Runs) > externalSettlementMaxRuns {
		return false
	}
	runs := map[string]bool{}
	total := 0
	for _, run := range r.Runs {
		total += len(run.Findings)
		if !externalSettlementRunID.MatchString(run.RunID) || runs[run.RunID] || run.Findings == nil || total > externalSettlementMaxFindings {
			return false
		}
		runs[run.RunID] = true
		findings := map[string]bool{}
		for _, f := range run.Findings {
			if findings[f.FindingID] || !safeText(f.FindingID, 256) || strings.TrimSpace(f.FindingID) == "" ||
				!validFindingDecisionAction(FindingDecisionRequest{Disposition: f.Disposition, Reason: f.Reason}) {
				return false
			}
			findings[f.FindingID] = true
			if f.Disposition == "accepted" {
				if !sha.MatchString(f.FixSHA) || !safeText(f.ProofNote, 2048) || strings.TrimSpace(f.ProofNote) == "" {
					return false
				}
			} else if f.FixSHA != "" || f.ProofNote != "" {
				return false
			}
		}
	}
	return true
}

func (e *Engine) SettleExternalHosted(ctx context.Context, scope core.Scope, key string, pull int64, request ExternalHostedSettlement) (json.RawMessage, error) {
	if pull < 1 || pull > 2147483647 || !request.valid() {
		return nil, invalid()
	}
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.external_hosted.settle", Target: strconv.FormatInt(pull, 10), Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		var repo int64
		if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&repo); err != nil {
			return nil, err
		}
		target := lifecycleTarget{project: scope.ProjectID, repository: repo, number: pull}
		var live bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_execution_reservations WHERE repository_id=? AND pull_number=? AND action_class='hosted')`, repo, pull).Scan(&live); err != nil {
			return nil, err
		}
		if live {
			return nil, core.Fail("review_busy", "Wait for the active Hosted review to settle before settling external results.", 409)
		}
		id, at := core.NewID("review_hosted_batch"), stamp()
		var accepted int64
		ids := make([]string, 0, len(request.Runs))
		for _, selected := range request.Runs {
			detail, err := reviews.ExternalHostedSettlementRun(ctx, tx, scope.ProjectID, repo, pull, selected.RunID)
			if err != nil {
				return nil, err
			}
			if err = requireExternalHostedIdentity(ctx, tx, target, detail.Run); err != nil {
				return nil, err
			}
			input, err := externalHostedDecisions(scope.PrincipalID, at, detail, selected.Findings)
			if err != nil {
				return nil, err
			}
			if err = reviews.FinalizeExternalHostedRun(ctx, tx, scope, input); err != nil {
				return nil, err
			}
			ids = append(ids, selected.RunID)
			accepted += input.FrozenCounts.Accepted
		}
		if err := holdQueuedExternalHosted(ctx, tx, scope.ProjectID, pull, false); err != nil {
			return nil, err
		}
		if *request.CountRound {
			if err := creditExternalHostedRound(ctx, tx, scope.ProjectID, pull, id, ids, accepted, at); err != nil {
				return nil, err
			}
		}
		receipt := map[string]any{"id": id, "state": "external_hosted_settled", "runs": len(ids), "round_counted": *request.CountRound}
		if err := core.AppendEvent(ctx, tx, scope, "reviews.external_hosted_settled", id, 1, map[string]any{"pull_number": pull, "run_ids": ids, "round_counted": *request.CountRound}); err != nil {
			return nil, err
		}
		return receipt, nil
	})
}

func requireExternalHostedIdentity(ctx context.Context, tx *sql.Tx, target lifecycleTarget, run reviews.RunSummary) error {
	kind, value, _ := strings.Cut(run.SourceRunID, ":")
	source := reviewprovider.ExternalHostedRun{SourceID: run.SourceRunID, HeadSHA: run.HeadSHA}
	if kind == "publication" {
		source.Inventory.Publication.PublicationID = value
	} else {
		source.ProviderRunID = value
	}
	captures, uncertain, err := externalNativeCaptures(ctx, tx, target, source)
	if err != nil {
		return err
	}
	if uncertain || len(captures) != 0 {
		return core.Fail("external_hosted_native_identity", "This result may already belong to a Controller review. Check the retained native capture before settling it.", 409)
	}
	return nil
}

func externalHostedDecisions(actor, at string, detail reviews.RunDetailResponse, decisions []ExternalHostedDecision) (reviews.NativeAdjudication, error) {
	counts := reviews.NativeCounts{Found: detail.Run.Found}
	input := reviews.NativeAdjudication{SourceRunID: detail.Run.SourceRunID, Finalized: true, FrozenCounts: &counts}
	refused := core.Fail("external_hosted_decisions_incomplete", "Supply a decision for every retained finding in each selected run.", 409)
	if len(decisions) != len(detail.Findings) {
		return input, refused
	}
	byID := make(map[string]ExternalHostedDecision, len(decisions))
	for _, decision := range decisions {
		byID[decision.FindingID] = decision
	}
	for _, finding := range detail.Findings {
		decision, ok := byID[finding.SourceFindingID]
		if !ok {
			return input, refused
		}
		update := reviews.NativeFindingUpdate{SourceFindingID: finding.SourceFindingID, Disposition: decision.Disposition, DecisionReason: decision.Reason, Actor: actor, DecidedAt: at}
		if decision.Disposition == "accepted" {
			counts.Accepted++
			update.FixSHA, update.ProofNote, update.ResolvedAt = decision.FixSHA, decision.ProofNote, at
		}
		input.Findings = append(input.Findings, update)
	}
	return input, nil
}

func (e *Engine) settleExternalHostedHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "write")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	raw := r.PathValue("pull_number")
	pull, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || pull < 1 || strconv.FormatInt(pull, 10) != raw {
		core.WriteError(w, invalid())
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var request ExternalHostedSettlement
	if err = core.ReadJSONLimit(w, r, &request, reviewBatchMaxBytes); err != nil {
		core.WriteError(w, err)
		return
	}
	receipt, err := e.SettleExternalHosted(r.Context(), scope, key, pull, request)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, receipt)
}

func requireExternalHostedSettled(ctx context.Context, tx *sql.Tx, project string, pull int64, channel string) error {
	if channel != "hosted" {
		return nil
	}
	var repository int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(github_repository_id,0) FROM projects WHERE id=?`, project).Scan(&repository); err != nil {
		return err
	}
	ids, err := reviews.PendingExternalHosted(ctx, tx, project, repository, pull, externalSettlementMaxRuns+1)
	if err != nil || len(ids) == 0 {
		return err
	}
	more := len(ids) > externalSettlementMaxRuns
	if more {
		ids = ids[:externalSettlementMaxRuns]
	}
	return &core.Error{Code: "external_hosted_pending", Status: 409,
		Message: "Settle the pending manual Hosted results before starting another Hosted review for this PR.",
		Details: map[string]any{"pull_number": pull, "run_ids": ids, "has_more": more}}
}

// Retire unposted intent before it can hold priority or consume a round that
// the worker now has a completed result for. An active attempt settles normally.
func holdQueuedExternalHosted(ctx context.Context, tx *sql.Tx, project string, pull int64, notify bool) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM review_operations WHERE project_id=? AND pull_number=? AND channel='hosted' AND state='queued'`, project, pull)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		o, err := readOperation(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if err := holdOperation(ctx, tx, o, "external_hosted_pending", stamp()); err != nil {
			return err
		}
		if notify {
			if err := publishStoppedOperationNotice(ctx, tx, o, "held", "external_hosted_pending", false); err != nil {
				return err
			}
		}
	}
	return nil
}
