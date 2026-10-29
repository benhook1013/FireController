package reviewexec

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

// DeploymentDrain is process-local operator state, separate from project policy.
// ActiveDispatches includes preparation and retention, not remote Hosted waits.
type DeploymentDrain struct {
	Draining         bool `json:"draining"`
	ActiveDispatches int  `json:"active_dispatches"`
}

func (e *Engine) beginDispatchWork() bool {
	e.deploymentMu.Lock()
	defer e.deploymentMu.Unlock()
	if e.deploymentDraining {
		return false
	}
	e.deploymentActive++
	return true
}
func (e *Engine) endDispatchWork() {
	e.deploymentMu.Lock()
	defer e.deploymentMu.Unlock()
	e.deploymentActive--
}
func (e *Engine) deploymentDrain(action string) DeploymentDrain {
	e.deploymentMu.Lock()
	defer e.deploymentMu.Unlock()
	switch action {
	case "begin":
		e.deploymentDraining = true
	case "cancel":
		e.deploymentDraining = false
	}
	return DeploymentDrain{e.deploymentDraining, e.deploymentActive}
}

func (e *Engine) deploymentDrainHTTP(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := e.store.AdminScope(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
			core.WriteError(w, err)
			return
		}
		if action == "begin" {
			var body struct{}
			if err = core.ReadJSON(w, r, &body); err != nil {
				core.WriteError(w, err)
				return
			}
		}
		var value DeploymentDrain
		err = e.store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			value = e.deploymentDrain(action)
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		core.WriteJSON(w, http.StatusOK, value)
	}
}
