package mcpadapter

import (
	"encoding/json"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func reviewRunArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	if name == "review_selection_read" {
		var in struct {
			ID   string `json:"selection_id"`
			Wait *int   `json:"wait_seconds,omitempty"`
		}
		if strictjson.Decode(data, &in) != nil || client.ValidateReviewRunSelectionID(in.ID) != nil {
			return args, invalidArguments()
		}
		args.id = in.ID
		if in.Wait != nil {
			args.reviewSourceWait = *in.Wait
		}
	} else {
		var in struct {
			Channel       string `json:"channel"`
			Pull          *int64 `json:"expected_pull_number,omitempty"`
			Configuration *int64 `json:"expected_configuration_revision,omitempty"`
			Key           string `json:"idempotency_key"`
			Wait          *int   `json:"wait_seconds,omitempty"`
		}
		if strictjson.Decode(data, &in) != nil || !validKey(in.Key) {
			return args, invalidArguments()
		}
		args.reviewRun = client.ReviewRunRequest{Channel: in.Channel, ExpectedPullNumber: in.Pull, ExpectedConfigurationRevision: in.Configuration}
		if client.ValidateReviewRunRequest(args.reviewRun) != nil {
			return args, invalidArguments()
		}
		args.key, args.reviewSourceWait = in.Key, 2
		if in.Wait != nil {
			args.reviewSourceWait = *in.Wait
		}
	}
	if client.ValidateReviewOperationWait(args.reviewSourceWait) != nil || args.reviewSourceWait > maxToolWaitSeconds {
		return arguments{}, invalidArguments()
	}
	return args, nil
}

func reviewRunDefinitions() []definition {
	schema := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	wait := map[string]any{"type": "integer", "minimum": 0, "maximum": maxToolWaitSeconds}
	return []definition{
		{"review_start", "Select this channel's earliest unfinished review target and request at most one review. Can consume included provider allowance. Expected PR is an assertion, never a forced target. Waits briefly for the result; pending work returns its original compact selection receipt. Retain the exact key and arguments for uncertain-output recovery; no automatic next review or retry.", schema(map[string]any{
			"channel":                         map[string]any{"type": "string", "enum": []string{"cli", "hosted"}},
			"expected_pull_number":            map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647},
			"expected_configuration_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64 - 1)},
			"idempotency_key":                 map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"wait_seconds":                    wait,
		}, "channel", "idempotency_key")},
		{"review_selection_read", "Read one retained selection and its linked review operation, with optional bounded waiting. No new provider work, retry or acknowledgment.", schema(map[string]any{
			"selection_id": map[string]any{"type": "string", "pattern": "^review_selection_[a-f0-9]{32}$", "maxLength": 128},
			"wait_seconds": wait,
		}, "selection_id")},
	}
}
