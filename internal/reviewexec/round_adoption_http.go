package reviewexec

import (
	"bytes"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/roundadoptionmodel"
	"github.com/benhook1013/FireController/internal/strictjson"
	"net/http"
)

func decodeRoundAdoption(raw json.RawMessage) (roundadoptionmodel.Prepared, error) {
	var p roundadoptionmodel.Prepared
	if len(raw) > 16384 || strictjson.Decode(raw, &p) != nil {
		return p, invalid()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return p, invalid()
	}
	_, flag := fields["maximum_unbounded"]
	_, obligations := fields["obligations"]
	expected := 14
	if flag {
		expected++
	}
	if obligations {
		expected++
	}
	if (p.Version != 1 && p.Version != 2) || p.Version == 1 && (flag || obligations) || len(fields) != expected {
		return p, invalid()
	}
	for _, name := range []string{"version", "repository_id", "expected_revision", "pull_number", "channel", "source", "source_key", "source_sha256", "minimum_remaining", "maximum_remaining", "initial_dry", "taper", "tapered", "closed"} {
		if _, ok := fields[name]; !ok {
			return p, invalid()
		}
	}
	for _, v := range fields {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return p, invalid()
		}
	}
	if obligations {
		var ref map[string]json.RawMessage
		if json.Unmarshal(fields["obligations"], &ref) != nil || len(ref) != 3 {
			return p, invalid()
		}
		for _, name := range []string{"namespace", "snapshot_sha256", "revision"} {
			if len(ref[name]) == 0 || bytes.Equal(bytes.TrimSpace(ref[name]), []byte("null")) {
				return p, invalid()
			}
		}
	}
	return p, nil
}
func (e *Engine) previewRoundAdoptionHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var raw json.RawMessage
	if err = core.ReadJSON(w, r, &raw); err != nil {
		core.WriteError(w, err)
		return
	}
	p, err := decodeRoundAdoption(raw)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	value, err := e.PreviewRoundAdoption(r.Context(), scope, p)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, value)
}
func (e *Engine) applyRoundAdoptionHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var wire struct {
		PreviewSHA256 *string         `json:"preview_sha256"`
		Control       json.RawMessage `json:"control"`
	}
	if err = core.ReadJSON(w, r, &wire); err != nil {
		core.WriteError(w, err)
		return
	}
	if wire.PreviewSHA256 == nil {
		core.WriteError(w, invalid())
		return
	}
	p, err := decodeRoundAdoption(wire.Control)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	value, err := e.ApplyRoundAdoption(r.Context(), scope, key, roundadoptionmodel.ApplyRequest{PreviewSHA256: *wire.PreviewSHA256, Control: p})
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, value)
}
