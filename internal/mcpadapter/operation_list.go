package mcpadapter

import (
	"encoding/json"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func operationListDefinition() definition {
	return definition{"review_operations_list", "Read one compact newest-first page of retained project operations, default five pending. Optional all includes terminal and held history for lost-receipt recovery. Mutable states are evaluated on each page; no historical snapshot, provider work, automatic pagination or review retry. Select review_operation_read for detail.", map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"pull_number": map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647},
		"channel":     map[string]any{"type": "string", "enum": []string{"cli", "hosted"}},
		"status":      map[string]any{"type": "string", "enum": []string{"pending", "all"}, "default": "pending"},
		"limit":       map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 5},
		"cursor":      map[string]any{"type": "string", "minLength": 1, "maxLength": 4096},
	}}}
}

func operationListArguments(data json.RawMessage) (arguments, error) {
	var args arguments
	var in struct {
		PullNumber *int64  `json:"pull_number,omitempty"`
		Channel    *string `json:"channel,omitempty"`
		Status     *string `json:"status,omitempty"`
		Limit      *int    `json:"limit,omitempty"`
		Cursor     *string `json:"cursor,omitempty"`
	}
	if strictjson.Decode(data, &in) != nil {
		return args, invalidArguments()
	}
	o := client.ReviewOperationsOptions{PageOptions: client.PageOptions{Limit: 5}, Status: "pending"}
	if in.PullNumber != nil {
		if *in.PullNumber < 1 {
			return args, invalidArguments()
		}
		o.PullNumber = *in.PullNumber
	}
	if in.Channel != nil {
		if *in.Channel == "" {
			return args, invalidArguments()
		}
		o.Channel = *in.Channel
	}
	if in.Status != nil {
		if *in.Status == "" {
			return args, invalidArguments()
		}
		o.Status = *in.Status
	}
	if in.Limit != nil {
		if *in.Limit < 1 {
			return args, invalidArguments()
		}
		o.Limit = *in.Limit
	}
	if in.Cursor != nil {
		if *in.Cursor == "" {
			return args, invalidArguments()
		}
		o.Cursor = *in.Cursor
	}
	if client.ValidateReviewOperationsOptions(o) != nil {
		return args, invalidArguments()
	}
	args.reviewOperations = o
	return args, nil
}
