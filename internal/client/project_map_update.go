package client

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// ProjectMapChanges contains only selected edits. Nil omits a field; explicit
// empty strings/arrays and false/zero values are retained through their pointers.
// Arrays replace their selected field, preserving the server's phase-ID rules.
type ProjectMapChanges struct {
	Title       *string            `json:"title,omitempty"`
	Description *string            `json:"description,omitempty"`
	State       *string            `json:"state,omitempty"`
	Where       *string            `json:"where,omitempty"`
	Next        *string            `json:"next,omitempty"`
	Trigger     *string            `json:"trigger,omitempty"`
	Phases      *[]ProjectMapPhase `json:"phases,omitempty"`
	Links       *[]ProjectMapLink  `json:"links,omitempty"`
	Archived    *bool              `json:"archived,omitempty"`
	Position    *int64             `json:"position,omitempty"`
}

type UpdateProjectMapEntryRequest struct {
	ExpectedRevision int64             `json:"expected_revision"`
	Changes          ProjectMapChanges `json:"changes"`
}

// DecodeProjectMapChanges rejects nulls, misspellings and duplicates before
// connection setup. The server checks kind-specific validity after atomic merge.
func DecodeProjectMapChanges(data []byte) (ProjectMapChanges, error) {
	var changes ProjectMapChanges
	if len(data) > 64<<10 || strictjson.Decode(data, &changes) != nil {
		return ProjectMapChanges{}, mutationValidation()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) == 0 {
		return ProjectMapChanges{}, mutationValidation()
	}
	for _, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return ProjectMapChanges{}, mutationValidation()
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
	delete(candidate, "trigger")
	encoded, _ := json.Marshal(candidate)
	normalized, err := DecodeProjectMapEditable(encoded)
	if err != nil || changes.Trigger != nil && !noteText(*changes.Trigger, 8<<10, false, true) {
		return ProjectMapChanges{}, mutationValidation()
	}
	if changes.Title != nil {
		changes.Title = &normalized.Title
	}
	if changes.Phases != nil {
		changes.Phases = &normalized.Phases
	}
	if changes.Links != nil {
		changes.Links = &normalized.Links
	}
	return changes, nil
}

func ValidateUpdateProjectMapEntryRequest(request UpdateProjectMapEntryRequest) error {
	if request.ExpectedRevision < 1 || request.ExpectedRevision == math.MaxInt64 {
		return fail("invalid_revision", "Supply a writable expected map-entry revision.", "Read the entry and supply its revision explicitly.", 400)
	}
	encoded, err := json.Marshal(request.Changes)
	if err != nil {
		return mutationValidation()
	}
	_, err = DecodeProjectMapChanges(encoded)
	return err
}

func (c *Client) UpdateProjectMapEntry(ctx context.Context, key, id string, request UpdateProjectMapEntryRequest) (ProjectMapReceipt, error) {
	if !validMutationKey(key) || !validID(id) {
		return ProjectMapReceipt{}, mutationValidation()
	}
	if err := ValidateUpdateProjectMapEntryRequest(request); err != nil {
		return ProjectMapReceipt{}, err
	}
	encoded, _ := json.Marshal(request.Changes)
	request.Changes, _ = DecodeProjectMapChanges(encoded)
	var result ProjectMapReceipt
	if err := c.mutate(ctx, http.MethodPatch, c.projectMapPath(id), key, request, http.StatusOK, &result); err != nil {
		return ProjectMapReceipt{}, err
	}
	if result.ID != id || !(result.Outcome == "updated" && result.Revision == request.ExpectedRevision+1 || result.Outcome == "unchanged" && result.Revision == request.ExpectedRevision) {
		return ProjectMapReceipt{}, unknownMutation("invalid_response", "The service returned an invalid map-entry receipt; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}
