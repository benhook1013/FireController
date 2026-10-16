package reviewexec

import (
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

type policyHTTPRequest struct {
	ExpectedRevision *int64    `json:"expected_revision"`
	RequiredChannels *[]string `json:"required_channels"`
	PausedChannels   *[]string `json:"paused_channels"`
	MaxReviewFiles   *int64    `json:"max_review_files"`
}

func (e *Engine) readPolicyHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	record, err := e.ReadPolicy(r.Context(), scope)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, record)
}

func (e *Engine) configurePolicyHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var wire policyHTTPRequest
	if err := core.ReadJSON(w, r, &wire); err != nil {
		core.WriteError(w, err)
		return
	}
	if wire.ExpectedRevision == nil || wire.RequiredChannels == nil || wire.PausedChannels == nil || wire.MaxReviewFiles == nil {
		core.WriteError(w, invalid())
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	input := PolicyConfiguration{ExpectedRevision: wire.ExpectedRevision, RequiredChannels: *wire.RequiredChannels, PausedChannels: *wire.PausedChannels, MaxReviewFiles: *wire.MaxReviewFiles}
	receipt, err := e.ConfigurePolicy(r.Context(), scope, key, input)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, receipt)
}
