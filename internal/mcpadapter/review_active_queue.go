package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func reviewActiveQueueArguments(data json.RawMessage) (client.PageOptions, bool, error) {
	out := client.PageOptions{Limit: 5}
	var in struct {
		Active *bool   `json:"active,omitempty"`
		Limit  *int    `json:"limit,omitempty"`
		Cursor *string `json:"cursor,omitempty"`
	}
	var fields map[string]json.RawMessage
	if len(data) > 16<<10 || strictjson.Decode(data, &in) != nil || json.Unmarshal(data, &fields) != nil || fields == nil {
		return out, false, invalidArguments()
	}
	for _, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return out, false, invalidArguments()
		}
	}
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > 100 {
			return out, false, invalidArguments()
		}
		out.Limit = *in.Limit
	}
	if in.Cursor != nil {
		if !validEventCursor(*in.Cursor) {
			return out, false, invalidArguments()
		}
		out.Cursor = *in.Cursor
	}
	return out, in.Active != nil && *in.Active, nil
}
func reviewActiveQueueSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"active": map[string]any{"type": "boolean", "default": false, "description": "Select only open, nonretired PRs before paging; compact scalar state without histories or readiness claims."}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 5}, "cursor": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}}, "additionalProperties": false}
}
func runReviewActiveQueue(ctx context.Context, service *client.Client, options client.PageOptions) (any, error) {
	return service.ReviewActiveQueue(ctx, options)
}
