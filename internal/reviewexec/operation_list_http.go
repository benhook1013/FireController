package reviewexec

import (
	"net/http"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
)

func (e *Engine) readOperationsHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	query, err := core.QueryKeys(r, "validation_failed", "pull_number", "channel", "status", "limit", "cursor")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	options := OperationListOptions{Channel: query.Get("channel"), Status: query.Get("status"), Cursor: query.Get("cursor")}
	for _, name := range []string{"pull_number", "channel", "status", "limit", "cursor"} {
		if values, ok := query[name]; ok && values[0] == "" {
			core.WriteError(w, invalid())
			return
		}
	}
	if raw, ok := query["pull_number"]; ok {
		options.PullNumber, err = strconv.ParseInt(raw[0], 10, 64)
		if err != nil || !validRoutePull(options.PullNumber) || strconv.FormatInt(options.PullNumber, 10) != raw[0] {
			core.WriteError(w, invalid())
			return
		}
	}
	if _, ok := query["limit"]; ok {
		options.Limit, err = core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
	}
	value, err := e.ListOperations(r.Context(), scope, options)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}
