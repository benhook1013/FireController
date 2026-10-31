package mcpadapter

import (
	"bytes"
	"encoding/json"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func ciWatchArguments(raw json.RawMessage) (arguments, error) {
	var args arguments
	var in struct {
		Targets []json.RawMessage `json:"targets"`
		Seconds *int              `json:"wait_seconds,omitempty"`
	}
	if len(raw) > 16<<10 || strictjson.Decode(raw, &in) != nil || len(in.Targets) < 1 || len(in.Targets) > client.CIWatchMaxTargets {
		return args, invalidArguments()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields["wait_seconds"] != nil && in.Seconds == nil {
		return args, invalidArguments()
	}
	args.reviewSourceWait = client.CIWatchDefaultSeconds
	if in.Seconds != nil {
		args.reviewSourceWait = *in.Seconds
	}
	if client.ValidateReviewOperationWait(args.reviewSourceWait) != nil {
		return args, invalidArguments()
	}
	for _, raw := range in.Targets {
		var target client.CIWatchTarget
		var fields map[string]json.RawMessage
		if strictjson.Decode(raw, &target) != nil || json.Unmarshal(raw, &fields) != nil || bytes.Equal(bytes.TrimSpace(fields["base_sha"]), []byte("null")) || fields["base_sha"] != nil && target.BaseSHA == "" {
			return args, invalidArguments()
		}
		args.ciWatchTargets = append(args.ciWatchTargets, target)
	}
	if client.ValidateCIWatchTargets(args.ciWatchTargets) != nil {
		return args, invalidArguments()
	}
	return args, nil
}

func ciWatchDefinition() definition {
	sha := func() map[string]any { return map[string]any{"type": "string", "pattern": "^[a-f0-9]{40}$"} }
	return definition{"wait_for_ci", "Wait quietly for fresh required CI on up to 20 PRs pinned to exact head commits and optional exact base commits. Checks the repository default merge branch requirements without enrollment. Returns when any target passes, fails, changes source, closes, or has verified no required checks; otherwise times out. Inspect per-check context, status and reason; omit completed targets from the next explicit wait. No merge permission, review, mutation, or automatic reconnect.", map[string]any{"type": "object", "additionalProperties": false, "required": []string{"targets"}, "properties": map[string]any{
		"targets":      map[string]any{"type": "array", "minItems": 1, "maxItems": client.CIWatchMaxTargets, "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"pull_number", "head_sha"}, "properties": map[string]any{"pull_number": map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}, "head_sha": sha(), "base_sha": sha()}}},
		"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": maxToolWaitSeconds, "default": client.CIWatchDefaultSeconds, "description": "Finite quiet result wait; zero returns available results immediately without waiting."},
	}}}
}
