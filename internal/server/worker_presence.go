package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

func (a *api) workerPresence(w http.ResponseWriter, r *http.Request) {
	invalid := core.Fail("validation_failed", "Use one worker_ids list of 1-100 distinct worker identities.", 400)
	if len(r.URL.RawQuery) > 8192 {
		core.WriteError(w, invalid)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 1 || len(query["worker_ids"]) != 1 {
		core.WriteError(w, invalid)
		return
	}
	ids := strings.Split(query.Get("worker_ids"), ",")
	if err = core.ValidateWorkerPresenceIDs(ids); err != nil {
		core.WriteError(w, err)
		return
	}
	scope, err := a.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	result, err := a.store.ReadWorkerPresence(r.Context(), scope, ids)
	complete(w, result, err)
}
