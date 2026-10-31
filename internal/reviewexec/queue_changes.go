package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/strictjson"
	"math"
	"net/http"
	"slices"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

// QueueChange edits only one existing project's scheduling list. No provider,
// mode, channel, pause or existing per-PR control is chosen by this request.
type QueueChange struct {
	ExpectedConfigurationRevision int64  `json:"expected_configuration_revision"`
	PullNumber                    int64  `json:"pull_number"`
	Before                        *int64 `json:"before,omitempty"`
	After                         *int64 `json:"after,omitempty"`
	Position                      string `json:"position,omitempty"`
	Reason                        string `json:"reason,omitempty"`
}

func validateQueueChange(action string, r QueueChange) error {
	if action != "add" && action != "move" && action != "remove" {
		return invalid()
	}
	if r.ExpectedConfigurationRevision < 1 || r.ExpectedConfigurationRevision == math.MaxInt64 || r.PullNumber < 1 || r.PullNumber > 2147483647 {
		return core.Fail("validation_failed", "Provide a positive expected_configuration_revision and a positive 32-bit pull_number.", 400)
	}
	if action == "remove" {
		if r.Before != nil || r.After != nil || r.Position != "" || r.Reason != "superseded" && r.Reason != "defunct" {
			return core.Fail("validation_failed", "Removal requires reason superseded or defunct and accepts no ordering destination.", 400)
		}
		return nil
	}
	if r.Reason != "" {
		return invalid()
	}
	positions := 0
	for _, anchor := range []*int64{r.Before, r.After} {
		if anchor != nil {
			positions++
			if *anchor < 1 || *anchor > 2147483647 || *anchor == r.PullNumber {
				return core.Fail("validation_failed", "before/after must name another positive 32-bit PR number.", 400)
			}
		}
	}
	if r.Position != "" {
		positions++
		if r.Position != "first" && r.Position != "last" {
			return core.Fail("validation_failed", "position must be first or last.", 400)
		}
	}
	if positions > 1 || action == "move" && positions != 1 {
		return core.Fail("validation_failed", "Choose one of before, after or position; move requires a destination. Add without a destination appends.", 400)
	}
	return nil
}
func changedQueue(queue []int64, action string, r QueueChange) ([]int64, error) {
	index := slices.Index(queue, r.PullNumber)
	if action == "add" && index >= 0 {
		return nil, core.Fail("review_queue_duplicate", "This PR is already enrolled; use queue move to change its position.", 409)
	}
	if (action == "move" || action == "remove") && index < 0 {
		return nil, core.Fail("review_not_configured", "This PR is not enrolled; use queue add first.", 409)
	}
	if action == "add" && len(queue) >= 100 {
		return nil, core.Fail("review_queue_full", "The configured queue already contains 100 PRs; retain completed history and deliberately update scheduling before adding another.", 409)
	}
	out := slices.Clone(queue)
	if index >= 0 {
		out = slices.Delete(out, index, index+1)
	}
	if action == "remove" {
		return out, nil
	}
	destination := len(out)
	if r.Position == "first" {
		destination = 0
	}
	for _, a := range []struct {
		n     *int64
		after bool
	}{{r.Before, false}, {r.After, true}} {
		if a.n != nil {
			destination = slices.Index(out, *a.n)
			if destination < 0 {
				return nil, core.Fail("review_queue_anchor_missing", "The destination PR is not in the configured queue; read its current order.", 409)
			}
			if a.after {
				destination++
			}
		}
	}
	return slices.Insert(out, destination, r.PullNumber), nil
}
func (e *Engine) ChangeQueue(ctx context.Context, scope core.Scope, key, action string, r QueueChange) (json.RawMessage, error) {
	if err := validateQueueChange(action, r); err != nil {
		return nil, err
	}
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.queue." + action, Target: "configuration", Key: key, Request: r}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		allowed, err := actorAllowed(ctx, tx, scope.ProjectID, scope.PrincipalID, scope.WorkerID)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, core.Fail("forbidden", "Project write access is required to change its queue.", 403)
		}
		previous, revision, err := readConfiguration(ctx, tx, scope.ProjectID)
		if err != nil {
			return nil, err
		}
		if revision != r.ExpectedConfigurationRevision {
			return nil, configurationRevisionConflict(revision)
		}
		if action == "remove" {
			var operation string
			err := tx.QueryRowContext(ctx, `SELECT id FROM review_operations WHERE project_id=? AND pull_number=? AND state NOT IN ('completed','held','failed','cancelled','rate_limited') ORDER BY created_at,id LIMIT 1`, scope.ProjectID, r.PullNumber).Scan(&operation)
			if err == nil {
				return nil, &core.Error{Code: "review_queue_operation_pending", Message: "This PR has a pending review operation; removal does not cancel it.", Status: 409, Details: map[string]string{"operation_id": operation}}
			}
			if err != sql.ErrNoRows {
				return nil, err
			}
		}
		queue, err := changedQueue(previous.Queue, action, r)
		if err != nil {
			return nil, err
		}
		if slices.Equal(queue, previous.Queue) {
			return Receipt{ID: scope.ProjectID, State: "unchanged", Revision: revision}, nil
		}
		next := previous
		next.Queue = queue
		next.ExpectedRevision = &revision
		// Planning preserves a configured capability even when temporarily unavailable.
		if err := validateConfigurationShape(next); err != nil {
			return nil, err
		}
		receipt, err := e.persistConfiguration(ctx, tx, scope, previous, next, revision)
		if err == nil && action == "remove" {
			err = reviews.RetireExecutionPull(ctx, tx, scope, r.PullNumber, r.Reason)
		}
		if err == nil && action == "add" {
			receipt.ReviewSize = reviews.CurrentReviewSize(ctx, tx, scope.ProjectID, r.PullNumber)
		}
		return receipt, err
	})
}
func (e *Engine) changeQueueHTTP(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := e.store.Scope(r, "write")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
			core.WriteError(w, err)
			return
		}
		var raw json.RawMessage
		if err = core.ReadJSON(w, r, &raw); err != nil {
			core.WriteError(w, err)
			return
		}
		var request QueueChange
		var fields map[string]json.RawMessage
		if strictjson.Decode(raw, &request) != nil || json.Unmarshal(raw, &fields) != nil || fields == nil {
			core.WriteError(w, invalid())
			return
		}
		for _, value := range fields {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				core.WriteError(w, invalid())
				return
			}
		}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		receipt, err := e.ChangeQueue(r.Context(), scope, key, action, request)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusCreated, receipt)
	}
}
