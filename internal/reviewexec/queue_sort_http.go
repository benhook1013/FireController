package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"net/http"
)

type queueSortingView struct {
	ProjectID string `json:"project_id"`
	Revision  int64  `json:"revision"`
	Mode      string `json:"mode"`
	Writable  bool   `json:"writable"`
	Reason    string `json:"reason,omitempty"`
}

func (e *Engine) queueSortingHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if r.Method == http.MethodPost {
		scope, err = e.store.AdminScope(r)
	}
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		c, err := e.ReadExecutionConfiguration(r.Context(), scope)
		if err != nil {
			var failure *core.Error
			if errors.As(err, &failure) && failure.Code == "reviews_unconfigured" {
				core.WriteJSON(w, http.StatusOK, queueSortingView{ProjectID: scope.ProjectID, Mode: "auto", Reason: "reviews_unconfigured"})
				return
			}
			core.WriteError(w, err)
			return
		}
		out := queueSortingView{ProjectID: scope.ProjectID, Revision: c.Revision, Mode: queueSortMode(Configuration{QueueSortMode: c.QueueSortMode}), Writable: e.queueSortingWrites}
		if !out.Writable {
			out.Reason = "compatible_backup_and_rollback_readers_required"
		}
		core.WriteJSON(w, 200, out)
		return
	}
	var input struct {
		ExpectedRevision int64  `json:"expected_revision"`
		Mode             string `json:"mode"`
	}
	if err = core.ReadJSON(w, r, &input); err != nil {
		core.WriteError(w, err)
		return
	}
	if input.ExpectedRevision < 1 || (input.Mode != "auto" && input.Mode != "manual") {
		core.WriteError(w, invalid())
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	// Read the exact retained revision, not latest, so uncertain retries produce
	// identical configuration requests and can replay the original receipt.
	var c Configuration
	err = e.store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		c, err = sourceConfiguration(ctx, tx, scope.ProjectID, input.ExpectedRevision)
		return err
	})
	if err != nil {
		core.WriteError(w, err)
		return
	}
	c.ExpectedRevision = &input.ExpectedRevision
	c.QueueSortMode = input.Mode
	out, err := e.Configure(r.Context(), scope, key, c)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, out)
}
