package reviewexec

import (
	"github.com/benhook1013/FireController/internal/core"
	"net/http"
)

func (e *Engine) readLaneDiagnosticHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	out, err := e.ReadLaneDiagnostic(r.Context(), scope, r.PathValue("channel"))
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, out)
}
