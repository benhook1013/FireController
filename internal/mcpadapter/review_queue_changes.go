package mcpadapter

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func queueChangeArguments(name string, raw json.RawMessage) (arguments, error) {
	var args arguments
	action := strings.TrimPrefix(name, "review_queue_")
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return args, invalidArguments()
	}
	for field := range fields {
		allowed := field == "expected_configuration_revision" || field == "idempotency_key"
		if action == "reorder" {
			allowed = allowed || field == "order"
		} else {
			allowed = allowed || field == "pull_number"
			if action == "remove" {
				allowed = allowed || field == "reason"
			} else {
				allowed = allowed || field == "before" || field == "after" || field == "position"
			}
		}
		if !allowed {
			return args, invalidArguments()
		}
	}
	var in struct {
		ExpectedConfigurationRevision int64   `json:"expected_configuration_revision"`
		PullNumber                    int64   `json:"pull_number"`
		Before                        *int64  `json:"before,omitempty"`
		After                         *int64  `json:"after,omitempty"`
		Position                      string  `json:"position,omitempty"`
		Reason                        string  `json:"reason,omitempty"`
		Order                         []int64 `json:"order,omitempty"`
		Key                           string  `json:"idempotency_key"`
	}
	if strictjson.Decode(raw, &in) != nil || !validKey(in.Key) {
		return args, invalidArguments()
	}
	args.queueChange = client.ReviewQueueChange{ExpectedConfigurationRevision: in.ExpectedConfigurationRevision, PullNumber: in.PullNumber, Before: in.Before, After: in.After, Position: in.Position, Reason: in.Reason, Order: in.Order}
	if client.ValidateReviewQueueChange(action, args.queueChange) != nil {
		return args, diagnostic("invalid_arguments", "Supply the current expected_configuration_revision and explicit key. Add and restore take pull_number and at most one destination. Move needs one destination. Remove takes pull_number and reason superseded or defunct. Reorder takes order with 2–100 unique enrolled PR numbers.", "Read review_execution_read for the current order and revision. No request was sent.")
	}
	args.key = in.Key
	return args, nil
}
func queueChangeDefinitions() []definition {
	var out []definition
	for _, action := range []string{"add", "move", "remove", "restore", "reorder"} {

		properties := map[string]any{
			"expected_configuration_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64 - 1)},
			"pull_number":                     map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647},
			"idempotency_key":                 map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
		}
		required := []string{"expected_configuration_revision", "pull_number", "idempotency_key"}
		description := "Project-writer queue " + action + " for one PR. Preserves unrelated configuration and existing review controls; adding installs absent product defaults and updates website enrollment/order atomically. Local provider-free action, no review request. Use before, after or position (first/last); add without a destination appends, move requires a destination. Retain exact key/arguments after unknown outcome."
		if action == "remove" {
			properties["reason"] = map[string]any{"type": "string", "enum": []string{"superseded", "defunct"}}
			required = append(required, "reason")
			description = "Project-writer removal of one enrolled defunct or superseded PR from scheduling and the visible queue atomically. Requires current expected_configuration_revision, reason superseded/defunct and explicit key; accepts no ordering destination. Ordinary merged PRs remain completed history; an actually absorbed or superseded merged carrier may be deliberately retired only with reason=superseded, preserving all history. Defunct merged PRs and pending operations (with operation_id) are refused. Preserves allowances, taper, capabilities and pause; never cancels work or requests a review. Removing the final item leaves an empty configured queue. Retain exact arguments/key after unknown outcome."
		} else if action == "reorder" {
			delete(properties, "pull_number")
			properties["order"] = map[string]any{"type": "array", "minItems": 2, "maxItems": 100, "uniqueItems": true, "items": map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}}
			required = []string{"expected_configuration_revision", "order", "idempotency_key"}
			description = "Project-writer relative reorder of 2–100 unique enrolled PR numbers. Replaces only the slots occupied by selected numbers in scheduling and Delivery; unnamed slots stay unchanged. Requires current expected_configuration_revision and explicit key; accepts no pull_number, destination or reason. Preserves all settings, review controls and history. No-op retains configuration revision. Local provider-free write, no review request. Retain exact arguments/key after unknown outcome."
		} else {
			properties["before"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}
			properties["after"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}
			properties["position"] = map[string]any{"type": "string", "enum": []string{"first", "last"}}
			if action == "restore" {
				description = "Project-writer restore of one explicitly retired PR to scheduling and Delivery in one transaction. Takes current expected_configuration_revision, pull_number, an explicit key and at most one before, after or position (first/last). Without a destination the PR stays in its configured slot if present, otherwise it is appended. Preserves retained history, controls and unrelated settings. Local provider-free write. Retain exact arguments and key after an unknown outcome."
			}
		}
		out = append(out, definition{"review_queue_" + action, description, map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}})
	}
	return out
}
