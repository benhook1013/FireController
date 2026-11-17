package projectmap

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type updateRequest struct {
	ExpectedRevision *int64          `json:"expected_revision"`
	Changes          json.RawMessage `json:"changes"`
}

// normalizeChanges validates only supplied fields. Kind-specific combinations
// and retained phase identities are checked after merging inside the transaction.
func normalizeChanges(data []byte) (map[string]json.RawMessage, error) {
	if len(data) > maxEditableBytes || strictjson.Validate(data) != nil {
		return nil, invalid()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) == 0 {
		return nil, invalid()
	}
	for name, raw := range fields {
		if name == "kind" || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, invalid()
		}
	}
	candidate := make(map[string]json.RawMessage, len(fields)+2)
	for name, raw := range fields {
		candidate[name] = raw
	}
	candidate["kind"] = json.RawMessage(`"workstream"`)
	if _, ok := candidate["title"]; !ok {
		candidate["title"] = json.RawMessage(`"partial update"`)
	}
	trigger := candidate["trigger"]
	delete(candidate, "trigger")
	encoded, _ := json.Marshal(candidate)
	var request editableRequest
	if strictjson.Decode(encoded, &request) != nil {
		return nil, invalid()
	}
	value, err := request.value()
	if err != nil {
		return nil, err
	}
	var triggerValue string
	if trigger != nil && (json.Unmarshal(trigger, &triggerValue) != nil || !text(triggerValue, 8<<10)) {
		return nil, invalid()
	}
	normalized, _ := json.Marshal(value)
	var all map[string]json.RawMessage
	_ = json.Unmarshal(normalized, &all)
	// Zero position is omitted by full snapshots, but remains an explicit change.
	all["position"], _ = json.Marshal(value.Position)
	for name := range fields {
		if name == "trigger" {
			fields[name], _ = json.Marshal(triggerValue)
		} else {
			fields[name] = all[name]
		}
	}
	return fields, nil
}

func updateEntry(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "write")
		if !ok {
			return
		}
		id, ok := selectedEntry(w, r)
		if !ok {
			return
		}
		var request updateRequest
		if err := core.ReadJSON(w, r, &request); err != nil {
			core.WriteError(w, err)
			return
		}
		if !validGuard(request.ExpectedRevision, 1) {
			core.WriteError(w, invalid())
			return
		}
		changes, err := normalizeChanges(request.Changes)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		normalized := struct {
			ExpectedRevision int64                      `json:"expected_revision"`
			Changes          map[string]json.RawMessage `json:"changes"`
		}{*request.ExpectedRevision, changes}
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "map.entries.update", Target: id, Key: key, Request: normalized}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			current, err := readEntry(ctx, tx, scope.ProjectID, id)
			if err != nil {
				return nil, err
			}
			value := editable(current)
			// Decode may reuse slice backing arrays; keep the current snapshot immutable
			// until validation and equality checks complete.
			value.Phases = append([]Phase{}, current.Phases...)
			value.Links = append([]Link{}, current.Links...)
			encoded, _ := json.Marshal(changes)
			if strictjson.Decode(encoded, &value) != nil {
				return nil, invalid()
			}
			changed, err := admitEntryReplacement(current, normalized.ExpectedRevision, value)
			if err != nil {
				return nil, err
			}
			if !changed {
				return Receipt{id, current.Revision, "unchanged"}, nil
			}
			next := entry(value, scope.ProjectID, id, current.Revision+1, current.CreatedAt, stamp())
			if err := writeEntry(ctx, tx, scope, next, false); err != nil {
				return nil, err
			}
			return Receipt{id, next.Revision, "updated"}, nil
		})
		respond(w, json.RawMessage(result), err, http.StatusOK)
	}
}
