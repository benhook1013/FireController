package reviewexec

import (
	"net/http"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
)

func (e *Engine) readRoutedFindingsHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	query, err := core.QueryKeys(r, "validation_failed", "view", "pull_number", "limit", "cursor")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var pull int64
	if raw, present := query["pull_number"]; present {
		pull, err = strconv.ParseInt(raw[0], 10, 64)
		if err != nil || !validRoutePull(pull) || strconv.FormatInt(pull, 10) != raw[0] || query.Has("view") {
			core.WriteError(w, invalid())
			return
		}
	} else if query.Has("view") && query.Get("view") != "attention" {
		core.WriteError(w, invalid())
		return
	}
	limit := 20
	if query.Has("limit") {
		limit, err = core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
	}
	value, err := e.ReadRoutedFindings(r.Context(), scope, pull, limit, query.Get("cursor"))
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}
