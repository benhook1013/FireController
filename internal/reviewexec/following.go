package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

type FollowingRequest struct {
	ExpectedRevision *int64 `json:"expected_revision"`
	Following        *bool  `json:"following"`
}

func (e *Engine) followingTracked(ctx context.Context, tx *sql.Tx, project string, number int64) (bool, error) {
	c, _, err := readConfiguration(ctx, tx, project)
	var failure *core.Error
	if errors.As(err, &failure) && failure.Code == "reviews_unconfigured" {
		return false, nil
	}
	if err != nil || !slices.Contains(c.Queue, number) {
		return false, err
	}
	var retired bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_retired WHERE project_id=? AND number=?)`, project, number).Scan(&retired)
	return !retired, err
}

func (e *Engine) Following(ctx context.Context, scope core.Scope, number int64) (reviews.Following, error) {
	var value reviews.Following
	if scope.WorkerID == "" || !validRoutePull(number) {
		return value, invalid()
	}
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		value, err = reviews.ReadFollowing(ctx, tx, scope.ProjectID, scope.WorkerID, number)
		if err == nil {
			value.Tracked, err = e.followingTracked(ctx, tx, scope.ProjectID, number)
		}
		return err
	})
	return value, err
}

func (e *Engine) ConfigureFollowing(ctx context.Context, scope core.Scope, key string, number int64, input FollowingRequest) (json.RawMessage, error) {
	if scope.WorkerID == "" || !validRoutePull(number) || input.ExpectedRevision == nil || *input.ExpectedRevision < 0 || *input.ExpectedRevision == math.MaxInt64 || input.Following == nil {
		return nil, invalid()
	}
	request := struct {
		WorkerID string           `json:"worker_id"`
		Input    FollowingRequest `json:"input"`
	}{scope.WorkerID, input}
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.following", Target: strconv.FormatInt(number, 10), Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		if err := core.CheckActiveWorker(ctx, tx, scope.ProjectID, scope.WorkerID); err != nil {
			return nil, err
		}
		value, err := reviews.ReadFollowing(ctx, tx, scope.ProjectID, scope.WorkerID, number)
		if err != nil {
			return nil, err
		}
		if value.Revision != *input.ExpectedRevision {
			return nil, &core.Error{Code: "revision_conflict", Message: "The PR following preference changed; read its current revision.", Status: 409, Details: map[string]int64{"current_revision": value.Revision}}
		}
		value.Tracked, err = e.followingTracked(ctx, tx, scope.ProjectID, number)
		if err != nil {
			return nil, err
		}
		if *input.Following && !value.Tracked {
			return nil, core.Fail("review_not_configured", "This PR is not enrolled for observation. Following does not add it; use the existing queue enrollment separately.", 409)
		}
		mode := "muted"
		if *input.Following {
			mode = "follow"
		}
		if value.Mode == mode {
			return value, nil
		}
		value.Mode, value.Revision = mode, value.Revision+1
		_, err = tx.ExecContext(ctx, `INSERT INTO review_pr_followers(project_id,pull_number,worker_id,mode,revision,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(project_id,pull_number,worker_id) DO UPDATE SET mode=excluded.mode,revision=excluded.revision,updated_at=excluded.updated_at`, value.ProjectID, number, value.WorkerID, mode, value.Revision, stamp())
		return value, err
	})
}

func (e *Engine) registerFollowingHTTP(mux *http.ServeMux) {
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		mux.HandleFunc(method+" /api/v1/projects/{project_id}/reviews/pulls/{pull_number}/following", func(w http.ResponseWriter, r *http.Request) {
			permission := "read"
			if r.Method == http.MethodPut {
				permission = "write"
			}
			scope, err := e.store.Scope(r, permission)
			if err != nil {
				core.WriteError(w, err)
				return
			}
			number, err := strconv.ParseInt(r.PathValue("pull_number"), 10, 64)
			if err != nil || strconv.FormatInt(number, 10) != r.PathValue("pull_number") || scope.WorkerID == "" || !validRoutePull(number) {
				core.WriteError(w, invalid())
				return
			}
			if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
				core.WriteError(w, err)
				return
			}
			if r.Method == http.MethodGet {
				value, err := e.Following(r.Context(), scope, number)
				if err != nil {
					core.WriteError(w, err)
					return
				}
				core.WriteJSON(w, 200, value)
				return
			}
			var input FollowingRequest
			if err = core.ReadJSON(w, r, &input); err != nil {
				core.WriteError(w, err)
				return
			}
			key, err := core.IdempotencyKey(r, "idempotency_key_required")
			if err != nil {
				core.WriteError(w, err)
				return
			}
			value, err := e.ConfigureFollowing(r.Context(), scope, key, number, input)
			if err != nil {
				core.WriteError(w, err)
				return
			}
			core.WriteJSON(w, 200, json.RawMessage(value))
		})
	}
}

// SeedNotificationFollowers runs once after selection/scheduling schemas exist.
// Only recorded manual request workers are recoverable; principal identities
// and automatic project authorizers never supply inferred participants.
func SeedNotificationFollowers(ctx context.Context, store *core.Store) error {
	return store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var seeded string
		err := tx.QueryRowContext(ctx, `SELECT CASE WHEN typeof(value)='text' AND value='1' THEN value END FROM metadata WHERE key='review_notifications:manual_seed_v1'`).Scan(&seeded)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return core.Fail("schema_invalid", "The PR notification seed marker is invalid.", 500)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO review_pr_followers(project_id,pull_number,worker_id,mode,revision,updated_at)
 SELECT DISTINCT o.project_id,o.pull_number,o.worker_id,'auto',1,? FROM review_operations o JOIN workers w ON w.project_id=o.project_id AND w.id=o.worker_id
 WHERE o.worker_id<>'' AND NOT EXISTS(SELECT 1 FROM worker_retirements r WHERE r.project_id=o.project_id AND r.worker_id=o.worker_id)
 AND NOT EXISTS(SELECT 1 FROM review_selections s JOIN review_automatic_selections a ON a.project_id=s.project_id AND a.selection_id=s.id WHERE s.project_id=o.project_id AND s.operation_id=o.id)
 ON CONFLICT(project_id,pull_number,worker_id) DO NOTHING`, stamp())
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('review_notifications:manual_seed_v1','1')`)
		return err
	})
}

func (e *Engine) followExplicitWork(ctx context.Context, tx *sql.Tx, scope core.Scope, number int64) error {
	if e.publishQueue == nil {
		return nil
	} // Standalone execution journals have no review workspace.
	return reviews.AutoFollow(ctx, tx, scope, number)
}
