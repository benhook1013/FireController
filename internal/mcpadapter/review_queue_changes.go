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
	var in struct {
		ExpectedConfigurationRevision int64  `json:"expected_configuration_revision"`
		PullNumber                    int64  `json:"pull_number"`
		Before                        *int64 `json:"before,omitempty"`
		After                         *int64 `json:"after,omitempty"`
		Position                      string `json:"position,omitempty"`
		Key                           string `json:"idempotency_key"`
	}
	if strictjson.Decode(raw, &in) != nil || !validKey(in.Key) {
		return args, invalidArguments()
	}
	args.queueChange = client.ReviewQueueChange{ExpectedConfigurationRevision: in.ExpectedConfigurationRevision, PullNumber: in.PullNumber, Before: in.Before, After: in.After, Position: in.Position}
	if client.ValidateReviewQueueChange(strings.TrimPrefix(name, "review_queue_"), args.queueChange) != nil {
		return args, diagnostic("invalid_arguments", "Supply the current expected_configuration_revision, pull_number, explicit key and at most one of before, after or position. Move requires a destination.", "Read review_execution_read for current order/revision. Add without a destination appends. No request was sent.")
	}
	args.key = in.Key
	return args, nil
}
func queueChangeDefinitions() []definition {
	var out []definition
	for _, action := range []string{"add", "move"} {
		out = append(out, definition{"review_queue_" + action, "Project-writer queue " + action + " for one PR. Preserves unrelated configuration and existing review controls; adding installs absent product defaults and updates website enrollment/order atomically. Local provider-free action, no review request. Use before, after or position (first/last); add without a destination appends, move requires a destination. Retain exact key/arguments after unknown outcome.", map[string]any{"type": "object", "additionalProperties": false, "required": []string{"expected_configuration_revision", "pull_number", "idempotency_key"}, "properties": map[string]any{
			"expected_configuration_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64 - 1)},
			"pull_number":                     map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647},
			"before":                          map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647},
			"after":                           map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647},
			"position":                        map[string]any{"type": "string", "enum": []string{"first", "last"}},
			"idempotency_key":                 map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
		}}})
	}
	return out
}
