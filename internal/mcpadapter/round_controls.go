package mcpadapter

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
	"math"
)

func roundControlArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	var in struct {
		PullNumber       *int64 `json:"pull_number"`
		Channel          string `json:"channel"`
		Key              string `json:"idempotency_key,omitempty"`
		ExpectedRevision *int64 `json:"expected_revision,omitempty"`
		Action           string `json:"action,omitempty"`
		Minimum          *int64 `json:"minimum,omitempty"`
		Maximum          *int64 `json:"maximum,omitempty"`
	}
	if strictjson.Decode(data, &in) != nil || in.PullNumber == nil || *in.PullNumber < 1 || *in.PullNumber > 2147483647 || (in.Channel != "hosted" && in.Channel != "cli") {
		return args, invalidArguments()
	}
	args.pullNumber, args.roundChannel = *in.PullNumber, in.Channel
	if name == "review_rounds_read" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || len(fields) != 2 {
			return arguments{}, invalidArguments()
		}
		return args, nil
	}
	args.key = in.Key
	args.roundControl = client.ConfigureReviewRoundControlRequest{ExpectedRevision: in.ExpectedRevision, Action: in.Action, Minimum: in.Minimum, Maximum: in.Maximum}
	if !validKey(in.Key) || client.ValidateReviewRoundControlRequest(args.roundControl) != nil {
		return arguments{}, invalidArguments()
	}
	return args, nil
}

func roundControlDefinitions() []definition {
	pull := map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}
	channel := map[string]any{"type": "string", "enum": []string{"hosted", "cli"}}
	schema := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	return []definition{
		{"review_rounds_read", "Read one PR/channel's local additional-round allowance, dry streak and explicit closure. No provider work or merge-readiness verdict.", schema(map[string]any{"pull_number": pull, "channel": channel}, "pull_number", "channel")},
		{"review_rounds_configure", "Administrator only: grant additional rounds or close one PR/channel, using its current revision and an explicit retained idempotency key. Grant requires minimum and maximum; close omits both. No provider dispatch or automatic retry.", schema(map[string]any{"pull_number": pull, "channel": channel, "expected_revision": map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64 - 1)}, "idempotency_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "action": map[string]any{"type": "string", "enum": []string{"grant", "close"}}, "minimum": map[string]any{"type": "integer", "minimum": 0, "maximum": 1000}, "maximum": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}}, "pull_number", "channel", "expected_revision", "idempotency_key", "action")},
	}
}
