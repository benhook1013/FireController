package reviewexec

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func (e *Engine) requestCheckpointPublicationHTTP(w http.ResponseWriter, r *http.Request) {
	e.mutateCheckpointHTTP(w, r, false)
}

func (e *Engine) recoverCheckpointPublicationHTTP(w http.ResponseWriter, r *http.Request) {
	e.mutateCheckpointHTTP(w, r, true)
}

func (e *Engine) mutateCheckpointHTTP(w http.ResponseWriter, r *http.Request, recovery bool) {
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
	var request CheckpointPublicationRequest
	var recoverRequest CheckpointPublicationRecovery
	if recovery {
		wire := struct {
			ExpectedRevision *int64 `json:"expected_revision"`
			RetryUnposted    *bool  `json:"retry_unposted,omitempty"`
		}{RetryUnposted: new(bool)}
		if err = core.ReadJSON(w, r, &wire); err != nil {
			core.WriteError(w, err)
			return
		}
		if wire.ExpectedRevision == nil || wire.RetryUnposted == nil {
			core.WriteError(w, invalid())
			return
		}
		recoverRequest = CheckpointPublicationRecovery{ExpectedRevision: wire.ExpectedRevision, RetryUnposted: *wire.RetryUnposted}
	} else {
		if err = core.ReadJSON(w, r, &request); err != nil {
			core.WriteError(w, err)
			return
		}
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if err = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wait + 5*time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		core.WriteError(w, core.Fail("internal_error", "The checkpoint response deadline could not be established; no publication action was accepted.", 500))
		return
	}
	w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(int(wait/time.Second)))
	var receipt json.RawMessage
	if recovery {
		receipt, err = e.RecoverCheckpointPublication(r.Context(), scope, key, r.PathValue("publication_id"), recoverRequest)
	} else {
		receipt, err = e.RequestCheckpointPublication(r.Context(), scope, key, request)
	}
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var queued Receipt
	if json.Unmarshal(receipt, &queued) != nil || !validPublicationID(queued.ID) || queued.State != "queued" || queued.Revision < 1 {
		writeCheckpointWaitError(w, checkpointStorage())
		return
	}
	// Even wait=0 reads the accepted action once. Replaying a completed key
	// returns useful terminal facts rather than a stale pending receipt.
	current, terminal, err := e.WaitCheckpointPublication(r.Context(), scope, queued.ID, int(wait/time.Second))
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "overloaded" {
			current, err = e.ReadCheckpointPublication(r.Context(), scope, queued.ID)
			terminal = !checkpointActive(current.State)
		}
		if err != nil {
			writeCheckpointWaitError(w, err)
			return
		}
	}
	if terminal {
		core.WriteJSON(w, http.StatusOK, current)
		return
	}
	core.WriteJSON(w, http.StatusAccepted, receipt)
}

func (e *Engine) readCheckpointPublicationHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	current, err := e.ReadCheckpointPublication(r.Context(), scope, r.PathValue("publication_id"))
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, current)
}

func writeCheckpointWaitError(w http.ResponseWriter, err error) {
	failure := core.Error{Code: "internal_error", Message: "The accepted checkpoint result is unavailable; retain the same request and key for recovery.", Status: 500}
	var original *core.Error
	if errors.As(err, &original) {
		failure = *original
		failure.Message = "The accepted checkpoint result is unavailable; retain the same request and key for recovery."
	}
	failure.Details = map[string]any{"outcome_unknown": true}
	core.WriteError(w, &failure)
}
