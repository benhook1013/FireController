package reviewexec

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
)

func roundHTTPSelection(r *http.Request) (int64, string, error) {
	raw, channel := r.PathValue("pull_number"), r.PathValue("channel")
	number, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || number < 1 || number > 2147483647 || strconv.FormatInt(number, 10) != raw || !validChannel(channel) {
		return 0, "", invalid()
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		return 0, "", err
	}
	return number, channel, nil
}

func (e *Engine) readRoundControlHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	number, channel, err := roundHTTPSelection(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	value, err := e.ReadRoundControl(r.Context(), scope, number, channel)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}

func (e *Engine) configureRoundControlHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "write")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	number, channel, err := roundHTTPSelection(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var wire struct {
		ExpectedRevision *int64          `json:"expected_revision"`
		Action           string          `json:"action"`
		Minimum          json.RawMessage `json:"minimum,omitempty"`
		Maximum          json.RawMessage `json:"maximum,omitempty"`
	}
	if err := core.ReadJSON(w, r, &wire); err != nil {
		core.WriteError(w, err)
		return
	}
	request := RoundControlRequest{ExpectedRevision: wire.ExpectedRevision, Action: wire.Action}
	for _, field := range []struct {
		raw    json.RawMessage
		target **int64
	}{{wire.Minimum, &request.Minimum}, {wire.Maximum, &request.Maximum}} {
		if field.raw == nil {
			continue
		}
		var number *int64
		if json.Unmarshal(field.raw, &number) != nil || number == nil {
			core.WriteError(w, invalid())
			return
		}
		*field.target = number
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	value, err := e.ConfigureRoundControl(r.Context(), scope, key, number, channel, request)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}
