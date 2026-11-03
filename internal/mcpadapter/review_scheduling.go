package mcpadapter

import (
	"bytes"
	"encoding/json"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func reviewSchedulingWrite(name string) bool {
	return name == "review_scheduling_configure" || name == "review_pull_scheduling_update"
}

func reviewSchedulingTool(name string) bool {
	return reviewSchedulingWrite(name) || name == "review_scheduling_read" || name == "review_pull_scheduling_read"
}

func reviewSchedulingArguments(name string, raw json.RawMessage) (arguments, error) {
	var args arguments
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(raw) > 4096 || len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return args, invalidArguments()
	}
	if name == "review_scheduling_read" {
		var empty struct{}
		if strictjson.Decode(raw, &empty) != nil {
			return args, invalidArguments()
		}
		return args, nil
	}
	if name == "review_pull_scheduling_read" {
		var wire struct {
			PullNumber int64 `json:"pull_number"`
		}
		if strictjson.Decode(raw, &wire) != nil || wire.PullNumber < 1 || wire.PullNumber > 2147483647 {
			return args, invalidArguments()
		}
		args.pullNumber = wire.PullNumber
		return args, nil
	}
	if name == "review_scheduling_configure" {
		var wire struct {
			Revision *int64 `json:"expected_revision"`
			Enabled  *bool  `json:"automatic_hosted"`
			Key      string `json:"idempotency_key"`
		}
		if strictjson.Decode(raw, &wire) != nil || wire.Revision == nil || wire.Enabled == nil || !client.ValidMutationKey(wire.Key) {
			return args, invalidArguments()
		}
		args.key = wire.Key
		args.reviewScheduling = client.ConfigureReviewSchedulingRequest{ExpectedRevision: *wire.Revision, AutomaticHosted: *wire.Enabled}
		if client.ValidateConfigureReviewScheduling(args.reviewScheduling) != nil {
			return arguments{}, invalidArguments()
		}
		return args, nil
	}
	var wire struct {
		PullNumber int64   `json:"pull_number"`
		Revision   *int64  `json:"expected_revision"`
		Paused     *bool   `json:"paused"`
		Reason     *string `json:"reason,omitempty"`
		Key        string  `json:"idempotency_key"`
	}
	if name != "review_pull_scheduling_update" || strictjson.Decode(raw, &wire) != nil || wire.Revision == nil || wire.Paused == nil || !client.ValidMutationKey(wire.Key) {
		return args, invalidArguments()
	}
	var present map[string]json.RawMessage
	if json.Unmarshal(raw, &present) != nil || present["reason"] != nil && wire.Reason == nil {
		return args, invalidArguments()
	}
	args.key, args.pullNumber = wire.Key, wire.PullNumber
	args.reviewPullScheduling = client.ConfigureReviewPullSchedulingRequest{ExpectedRevision: *wire.Revision, Paused: *wire.Paused}
	if wire.Reason != nil {
		args.reviewPullScheduling.Reason = *wire.Reason
	}
	if client.ValidateConfigureReviewPullScheduling(args.pullNumber, args.reviewPullScheduling) != nil {
		return arguments{}, invalidArguments()
	}
	return args, nil
}

func reviewSchedulingDefinitions() []definition {
	object := func(properties map[string]any, required ...string) map[string]any {
		if required == nil {
			required = []string{}
		}
		return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
	}
	revision := map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64 - 1)}
	pull := map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}
	key := map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "Explicit original idempotency key; retain with the exact request after unknown outcome."}
	return []definition{
		{"review_scheduling_read", "Read this project's automatic Hosted setting and current queued PR pauses in one provider-free call. No settings, review or allowance changes. See docs/review-scheduling.md.", object(map[string]any{})},
		{"review_scheduling_configure", "Project manager: explicitly enable or disable automatic future Hosted requests under existing source, obligation, round and provider guards. Defaults off; CLI stays explicitly requested; accepted requests can automatically wait through quota exhaustion without selecting additional PRs or rounds. Enabling retains this connection's current credential and active bound worker; withdrawn authority stops new automatic actions. This setting can authorize asynchronous provider work. Current revision and explicit key required; no automatic mutation retry. See docs/review-scheduling.md.", object(map[string]any{"expected_revision": revision, "automatic_hosted": map[string]any{"type": "boolean"}, "idempotency_key": key}, "expected_revision", "automatic_hosted", "idempotency_key")},
		{"review_pull_scheduling_read", "Read one PR's temporary scheduling pause before or after an intent exists. Does not change Draft, grant, taper, closure or active provider work.", object(map[string]any{"pull_number": pull}, "pull_number")},
		{"review_pull_scheduling_update", "Writer: explicitly pause or resume new and undispatched reviews for one PR, including automatic Hosted selection. Does not cancel already-issued reviews or stop collecting results, change round credit or clear a separate operation pause. Retain current revision/key; reason is optional plain text, cleared on resume. See docs/review-scheduling.md.", object(map[string]any{"pull_number": pull, "expected_revision": revision, "paused": map[string]any{"type": "boolean"}, "reason": map[string]any{"type": "string", "maxLength": 256}, "idempotency_key": key}, "pull_number", "expected_revision", "paused", "idempotency_key")},
	}
}
