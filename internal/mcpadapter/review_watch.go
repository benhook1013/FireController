package mcpadapter

import (
	"bytes"
	"encoding/json"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func reviewWatchArguments(data json.RawMessage) (arguments, error) {
	var a arguments
	var in struct {
		Pull    *int64  `json:"pull_number,omitempty"`
		Channel *string `json:"channel,omitempty"`
		Limit   *int    `json:"limit,omitempty"`
		After   *string `json:"after,omitempty"`
		Wait    *int    `json:"wait_seconds,omitempty"`
	}
	if len(data) == 0 {
		data = []byte("{}")
	}
	if len(data) > 8<<10 || strictjson.Decode(data, &in) != nil {
		return a, invalidArguments()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return a, invalidArguments()
	}
	for _, v := range fields {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return a, invalidArguments()
		}
	}
	a.reviewWatch.Limit = 20
	a.reviewSourceWait = client.ReviewWatchDefaultSeconds
	if in.Pull != nil {
		if *in.Pull < 1 {
			return a, invalidArguments()
		}
		a.reviewWatch.PullNumber = *in.Pull
	}
	if in.Channel != nil {
		if *in.Channel == "" {
			return a, invalidArguments()
		}
		a.reviewWatch.Channel = *in.Channel
	}
	if in.Limit != nil {
		a.reviewWatch.Limit = *in.Limit
	}
	if in.After != nil {
		if *in.After == "" {
			return a, invalidArguments()
		}
		a.reviewWatch.After = *in.After
	}
	if in.Wait != nil {
		a.reviewSourceWait = *in.Wait
	}
	if client.ValidateReviewWatch(a.reviewWatch, a.reviewSourceWait) != nil {
		return a, invalidArguments()
	}
	return a, nil
}
func reviewWatchDefinition() definition {
	return definition{"wait_for_reviews", "Read a bounded initial attention snapshot and event_cursor; with after, wait quietly for native review request/lifecycle changes in this project, optionally one PR/channel. Includes terminal holds/failures even after they leave attention. Retain the same filters and event_cursor; snapshot.cursor is only pagination. Default 300s, maximum 900s, zero reads immediately. No provider calls, reviews, inbox messages or ownership inference. Requires matching service/client release; background and idle-chat delivery depend on host. See docs/review-operations.md.", map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"pull_number": map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}, "channel": map[string]any{"type": "string", "enum": []string{"cli", "hosted"}}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 20}, "after": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}, "wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 900, "default": 300},
	}}}
}
