package server

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func (a *api) messageEvents(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	scope, err := a.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	invalid := core.Fail("validation_failed", "Use cursor, limit and optional wait_ms 0-4000 once each for the selected worker's message feed.", 400)
	if len(r.URL.RawQuery) > 16<<10 {
		core.WriteError(w, invalid)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		core.WriteError(w, invalid)
		return
	}
	for name, values := range query {
		if len(values) != 1 || (name != "cursor" && name != "limit" && name != "wait_ms") {
			core.WriteError(w, invalid)
			return
		}
	}
	limit, err := core.PageLimit(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	wait := 0
	if values, present := query["wait_ms"]; present {
		wait, err = strconv.Atoi(values[0])
		if err != nil || wait < 0 || wait > 4000 {
			core.WriteError(w, invalid)
			return
		}
	}
	page, err := a.store.MessageEvents(ctx, scope, query.Get("cursor"), limit, time.Duration(wait)*time.Millisecond)
	complete(w, page, err)
}
