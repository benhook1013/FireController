package historycollect

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func (e *Engine) Register(mux *http.ServeMux) {
	base := "/api/v1/projects/{project_id}/history/collections"
	mux.HandleFunc("POST "+base, e.enqueueHTTP)
	mux.HandleFunc("GET "+base+"/{collection_id}", e.readHTTP)
	mux.HandleFunc("POST "+base+"/{collection_id}/resume", e.changeHTTP("resume"))
	mux.HandleFunc("POST "+base+"/{collection_id}/cancel", e.changeHTTP("cancel"))
	mux.HandleFunc("GET "+base+"/{collection_id}/pages", e.pagesHTTP)
	mux.HandleFunc("GET "+base+"/{collection_id}/pages/{page_id}", e.captureHTTP)
}

func (e *Engine) pagesHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	query, err := core.QueryKeys(r, "validation_failed", "limit", "cursor")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	limit := 5
	if value, present := query["limit"]; present {
		limit, err = strconv.Atoi(value[0])
		if err != nil || strconv.Itoa(limit) != value[0] {
			core.WriteError(w, invalid())
			return
		}
	}
	value, err := e.Pages(r.Context(), scope, r.PathValue("collection_id"), limit, query.Get("cursor"))
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}

func (e *Engine) captureHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	value, err := e.Capture(r.Context(), scope, r.PathValue("collection_id"), r.PathValue("page_id"))
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}

func responseDeadline(w http.ResponseWriter, seconds int) error {
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Duration(seconds+5) * time.Second))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return core.Fail("internal_error", "The response deadline could not be established; no action was accepted.", 500)
	}
	return nil
}

func (e *Engine) enqueueHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Prefer")
	scope, err := e.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	seconds, err := core.WaitPreference(r, 2)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var selection Selection
	if err := core.ReadJSON(w, r, &selection); err != nil {
		core.WriteError(w, err)
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if err := responseDeadline(w, seconds); err != nil {
		core.WriteError(w, err)
		return
	}
	w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(seconds))
	receipt, err := e.Enqueue(r.Context(), scope, key, selection)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var accepted Receipt
	if json.Unmarshal(receipt, &accepted) != nil {
		acceptedWaitError(w, "", storageError())
		return
	}
	if seconds > 0 {
		operation, err := e.Wait(r.Context(), scope, accepted.ID, time.Duration(seconds)*time.Second)
		if err != nil {
			if detached, detachErr := e.acceptedWaitDetached(r.Context(), scope, accepted.ID, err); detached {
				core.WriteJSON(w, http.StatusAccepted, receipt)
				return
			} else {
				err = detachErr
			}
		}
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "overloaded" {
			if details, ok := failure.Details.(map[string]string); ok && details["reason"] == "collection_wait_capacity" {
				operation, err = e.Read(r.Context(), scope, accepted.ID)
			}
		}
		if err != nil {
			acceptedWaitError(w, accepted.ID, err)
			return
		}
		if operation.State == "completed" || operation.State == "paused" || operation.State == "canceled" {
			core.WriteJSON(w, http.StatusOK, operation)
			return
		}
	}
	core.WriteJSON(w, http.StatusAccepted, receipt)
}

func acceptedWaitError(w http.ResponseWriter, id string, err error) {
	failure := core.Error{Code: "internal_error", Status: 500}
	var original *core.Error
	if errors.As(err, &original) {
		failure = *original
	}
	failure.Message = "The accepted collection result is unavailable; retry the same request and key."
	failure.Details = map[string]any{"outcome_unknown": true, "operation_id": id}
	core.WriteError(w, &failure)
}

func (e *Engine) readHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Prefer")
	scope, err := e.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	seconds, err := core.WaitPreference(r, 0)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if err := responseDeadline(w, seconds); err != nil {
		core.WriteError(w, err)
		return
	}
	value, err := e.Wait(r.Context(), scope, r.PathValue("collection_id"), time.Duration(seconds)*time.Second)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(seconds))
	core.WriteJSON(w, http.StatusOK, value)
}

func (e *Engine) changeHTTP(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := e.store.AdminScope(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
			core.WriteError(w, err)
			return
		}
		var request struct {
			ExpectedRevision *int64 `json:"expected_revision"`
		}
		if err := core.ReadJSON(w, r, &request); err != nil {
			core.WriteError(w, err)
			return
		}
		if request.ExpectedRevision == nil {
			core.WriteError(w, invalid())
			return
		}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		value, err := e.Change(r.Context(), scope, r.PathValue("collection_id"), key, action, *request.ExpectedRevision)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, value)
	}
}
