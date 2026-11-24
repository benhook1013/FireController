package reviewexec

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"slices"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

type queuePriorityChange struct {
	ExpectedRevision *int64  `json:"expected_revision"`
	PullNumbers      []int64 `json:"pull_numbers"`
	Priority         *bool   `json:"priority"`
}

func (e *Engine) queuePriorityHTTP(w http.ResponseWriter, r *http.Request) {
	permission := "read"
	if r.Method == http.MethodPost {
		permission = "write"
	}
	scope, err := e.store.Scope(r, permission)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		var result reviews.QueuePriority
		err := e.store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			result, err = reviews.ReadQueuePriority(ctx, tx, scope.ProjectID)
			return err
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, result)
		return
	}
	var input queuePriorityChange
	if err = core.ReadJSON(w, r, &input); err != nil {
		core.WriteError(w, err)
		return
	}
	if input.ExpectedRevision == nil || *input.ExpectedRevision < 0 || input.Priority == nil ||
		len(input.PullNumbers) == 0 || !reviews.ValidPriorityPulls(input.PullNumbers) {
		core.WriteError(w, invalid())
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	out, err := e.store.Mutate(r.Context(), scope, core.Mutation{
		Operation: "reviews.queue_priority", Target: scope.ProjectID, Key: key, Request: input,
	}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		current, err := reviews.ReadQueuePriority(ctx, tx, scope.ProjectID)
		if err != nil {
			return nil, err
		}
		if current.Revision != *input.ExpectedRevision {
			return nil, conflict()
		}
		configuration, _, err := readConfiguration(ctx, tx, scope.ProjectID)
		if err != nil {
			return nil, err
		}
		selected := make(map[int64]bool, len(current.PriorityPulls))
		for _, pull := range current.PriorityPulls {
			if slices.Contains(configuration.Queue, pull) {
				selected[pull] = true
			}
		}
		for _, pull := range input.PullNumbers {
			if *input.Priority {
				if !slices.Contains(configuration.Queue, pull) {
					return nil, core.Fail("queue_priority_unenrolled", "Enroll the pull request before marking it as priority.", 409)
				}
				selected[pull] = true
			} else {
				delete(selected, pull)
			}
		}
		pulls := make([]int64, 0, len(selected))
		for pull := range selected {
			pulls = append(pulls, pull)
		}
		slices.Sort(pulls)
		if slices.Equal(current.PriorityPulls, pulls) {
			return current, nil
		}
		if current.Revision == math.MaxInt64 {
			return nil, core.Fail("revision_exhausted", "The queue priority revision cannot advance further.", 409)
		}
		current.PriorityPulls = pulls
		current.Revision++
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_queue_priority(project_id,revision,pulls_json) VALUES(?,?,?) ON CONFLICT(project_id) DO UPDATE SET revision=excluded.revision,pulls_json=excluded.pulls_json`, scope.ProjectID, current.Revision, encoded(pulls)); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "review.queue_priority_changed", scope.ProjectID, current.Revision, current); err != nil {
			return nil, err
		}
		return current, nil
	})
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, out)
}
