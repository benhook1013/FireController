package reviewexec

import (
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

func (e *Engine) readPolicyAssessmentHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	value, err := e.ReadPolicyAssessment(r.Context(), scope, r.PathValue("evidence_id"))
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}
