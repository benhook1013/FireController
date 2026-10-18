package reviewexec

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func (e *Engine) enqueueSourceRefreshHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Prefer")
	scope, err := e.store.Scope(r, "write")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	wait, err := sourceRefreshPreference(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var wire struct {
		ExpectedConfigurationRevision *int64  `json:"expected_configuration_revision"`
		PullNumber                    *int64  `json:"pull_number"`
		HeadSHA                       *string `json:"head_sha"`
		IncludeCI                     *bool   `json:"include_ci,omitempty"`
		IncludeReadiness              *bool   `json:"include_readiness,omitempty"`
	}
	wire.IncludeReadiness = new(bool)
	wire.IncludeCI = new(bool) // Omission selects source-only; explicit null is invalid.
	if err = core.ReadJSON(w, r, &wire); err != nil {
		core.WriteError(w, err)
		return
	}
	if wire.ExpectedConfigurationRevision == nil || wire.PullNumber == nil || wire.HeadSHA == nil {
		core.WriteError(w, invalid())
		return
	}
	if wire.IncludeCI == nil || wire.IncludeReadiness == nil {
		core.WriteError(w, invalid())
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	request := SourceRefreshRequest{ExpectedConfigurationRevision: *wire.ExpectedConfigurationRevision, PullNumber: *wire.PullNumber, HeadSHA: *wire.HeadSHA, IncludeCI: *wire.IncludeCI, IncludeReadiness: *wire.IncludeReadiness}
	if !validRefreshRequest(request) {
		core.WriteError(w, invalid())
		return
	}
	if err = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wait + 5*time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		core.WriteError(w, core.Fail("internal_error", "The source refresh response deadline could not be established; no refresh was accepted.", 500))
		return
	}
	w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(int(wait/time.Second)))
	receipt, err := e.EnqueueSourceRefresh(r.Context(), scope, key, request)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if wait > 0 {
		var queued Receipt
		if json.Unmarshal(receipt, &queued) != nil {
			writeSourceWaitError(w, refreshStorageError())
			return
		}
		current, err := e.WaitSourceRefresh(r.Context(), scope, queued.ID, wait)
		if err != nil {
			if detached, detachErr := e.acceptedWaitDetached(r.Context(), scope, err); detached {
				core.WriteJSON(w, http.StatusAccepted, receipt)
				return
			} else {
				err = detachErr
			}
			var failure *core.Error
			capacity := false
			if errors.As(err, &failure) && failure.Code == "overloaded" {
				if details, ok := failure.Details.(map[string]string); ok {
					capacity = details["reason"] == "source_wait_capacity"
				}
			}
			if capacity {
				current, err = e.ReadSourceRefresh(r.Context(), scope, queued.ID)
			}
			if err != nil {
				writeSourceWaitError(w, err)
				return
			}
		}
		if current.State == "completed" || current.State == "held" {
			core.WriteJSON(w, http.StatusOK, current)
			return
		}
	}
	core.WriteJSON(w, http.StatusAccepted, receipt)
}
func (e *Engine) readSourceRefreshHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	value, err := e.ReadSourceRefresh(r.Context(), scope, r.PathValue("refresh_id"))
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}
func (e *Engine) readSourcePolicyAssessmentHTTP(w http.ResponseWriter, r *http.Request) {
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
	value, err := e.ReadSourcePolicyAssessment(r.Context(), scope, number)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}

func sourceRefreshPreference(r *http.Request) (time.Duration, error) {
	values, present := r.Header["Prefer"]
	if !present {
		return 2 * time.Second, nil
	}
	if len(values) != 1 || !strings.HasPrefix(values[0], "wait=") {
		return 0, invalid()
	}
	raw := strings.TrimPrefix(values[0], "wait=")
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 || seconds > 60 || strconv.Itoa(seconds) != raw {
		return 0, invalid()
	}
	return time.Duration(seconds) * time.Second, nil
}

// The durable action has already committed. Preserve authorization failures
// without exposing its identity, and make lost-result recovery explicit.
func writeSourceWaitError(w http.ResponseWriter, err error) {
	failure := core.Error{Code: "internal_error", Message: "The accepted source refresh result could not be read; retain the same request and key for recovery.", Status: 500}
	var original *core.Error
	if errors.As(err, &original) {
		failure = *original
		failure.Message = "The accepted source refresh result is unavailable; retain the same request and key for recovery."
	}
	failure.Details = map[string]any{"outcome_unknown": true}
	core.WriteError(w, &failure)
}

func (e *Engine) readSourceCIAssessmentHTTP(w http.ResponseWriter, r *http.Request) {
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
	value, err := e.ReadSourceCIAssessment(r.Context(), scope, number)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}
