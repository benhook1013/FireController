package reviewexec

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
	"net/http"
	"time"
)

func (e *Engine) actionsDiagnosticHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if err := operationResponseDeadlineAt(w, started.Add(actionsDiagnosticBudget+5*time.Second)); err != nil {
		core.WriteError(w, err)
		return
	}
	lifetime, cleanup := e.resultWaitContext(r.Context())
	defer cleanup()
	ctx, cancel := context.WithDeadline(lifetime, started.Add(actionsDiagnosticBudget))
	defer cancel()
	r = r.WithContext(ctx)
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var input struct {
		AgeHours *int `json:"age_hours"`
		Limit    *int `json:"limit"`
	}
	finished := make(chan struct{})
	stop := context.AfterFunc(r.Context(), func() { defer close(finished); _ = http.NewResponseController(w).SetReadDeadline(time.Now()) })
	err = core.ReadJSONLimit(w, r, &input, 4096)
	if !stop() {
		<-finished
	}
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if input.AgeHours == nil || input.Limit == nil {
		core.WriteError(w, invalid())
		return
	}
	result, err := e.ObserveActionsDiagnostic(r.Context(), scope, *input.AgeHours, *input.Limit)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, result)
}
