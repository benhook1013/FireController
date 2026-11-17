package reviewexec

import (
	"github.com/benhook1013/FireController/internal/core"
	"net/http"
	"strconv"
)

func (e *Engine) readSourceReadinessHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	raw := r.PathValue("pull_number")
	number, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || strconv.FormatInt(number, 10) != raw || number < 1 || number > 2147483647 {
		core.WriteError(w, invalid())
		return
	}
	value, err := e.ReadSourceReadiness(r.Context(), scope, number)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}
