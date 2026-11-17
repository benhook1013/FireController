package reviewexec

import (
	"github.com/benhook1013/FireController/internal/core"
	"net/http"
	"strconv"
)

func (e *Engine) selectionDecisionHistoryHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed", "before", "limit"); err != nil {
		core.WriteError(w, err)
		return
	}
	before := int64(0)
	limit := 50
	if raw := r.URL.Query().Get("before"); r.URL.Query().Has("before") {
		before, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || before < 1 || strconv.FormatInt(before, 10) != raw {
			core.WriteError(w, invalid())
			return
		}
	}
	if raw := r.URL.Query().Get("limit"); r.URL.Query().Has("limit") {
		limit, err = strconv.Atoi(raw)
		if err != nil || strconv.Itoa(limit) != raw {
			core.WriteError(w, invalid())
			return
		}
	}
	out, err := e.ReadSelectionDecisionHistory(r.Context(), scope, r.PathValue("channel"), before, limit)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, out)
}
