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
		if strictjson.Decode(raw, &target) != nil || json.Unmarshal(raw, &fields) != nil || bytes.Equal(bytes.TrimSpace(fields["base_sha"]), []byte("null")) || fields["base_sha"] != nil && target.BaseSHA == "" || fields["after_observation_key"] != nil && (bytes.Equal(bytes.TrimSpace(fields["after_observation_key"]), []byte("null")) || target.AfterObservationKey == "") {
			return args, invalidArguments()
		}
		if fields["failure_workflow_ids"] != nil && len(target.FailureWorkflowIDs) == 0 {
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
	return definition{"wait_for_ci", "Wait quietly for fresh required CI on up to 20 PRs pinned to exact head commits and optional exact base commits. Checks the repository default merge branch requirements without enrollment. Returns when any target passes, fails, changes source, closes, or has verified no required checks; otherwise times out. The outer state is the wake condition, not batch success: all_targets_passed is true only when every target passed. Inspect targets and per-check context, status and reason; omit completed targets unless continuing to observe them. Reuse a passed or failed observation_key as that target's after_observation_key with the original source pins to wait quietly for changed terminal evidence; unchanged verdicts retain their real facts. Pending checks alone do not wake this terminal wait. Optional failure_workflow_ids return early attention for a failed job in those explicit workflows while gate status stays pending; they do not become required checks. No merge permission, review or mutation.", map[string]any{"type": "object", "additionalProperties": false, "required": []string{"targets"}, "properties": map[string]any{
		"targets":      map[string]any{"type": "array", "minItems": 1, "maxItems": client.CIWatchMaxTargets, "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"pull_number", "head_sha"}, "properties": map[string]any{"pull_number": map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}, "head_sha": sha(), "base_sha": sha(), "failure_workflow_ids": map[string]any{"type": "array", "minItems": 1, "maxItems": 16, "uniqueItems": true, "items": map[string]any{"type": "integer", "minimum": 1}, "description": "Optional stable workflow IDs for early failed-job attention. Exact current runs are observed without making these workflows required CI or allowing unrelated preview failures to wake this watch."}, "after_observation_key": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$", "description": "Optional observation_key from a known passed or failed result, or selected workflow attention. Wait quietly while the same verdict remains; retain the original source pins. Changed terminal proof and attention still return with current facts. Pending checks alone do not wake this terminal wait."}}}},
		"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": maxToolWaitSeconds, "default": client.CIWatchDefaultSeconds, "description": "Finite quiet result wait. Zero does not wait for collection or reuse cached observations; it normally returns not_observed pending targets. Only a fresh observation completed during this call can appear. Use a positive wait to allow fresh CI facts."},
	}}}
}
