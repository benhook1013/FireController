package mcpadapter

import (
	"encoding/json"
	"strings"

	"github.com/benhook1013/FireController/internal/strictjson"
)

func reviewTool(name string) bool {
	return name == "review_queue" || name == "review_pull" || name == "review_runs" || name == "review_run"
}

func reviewArguments(name string, data json.RawMessage, fields map[string]json.RawMessage) (arguments, error) {
	var args arguments
	type pullInput struct {
		Number   int64  `json:"pull_number"`
		Snapshot string `json:"snapshot,omitempty"`
	}
	type runsInput struct {
		Number   int64  `json:"pull_number"`
		Snapshot string `json:"snapshot,omitempty"`
		Limit    *int   `json:"limit,omitempty"`
		Cursor   string `json:"cursor,omitempty"`
	}
	type runInput struct {
		ID       string `json:"run_id"`
		Snapshot string `json:"snapshot,omitempty"`
	}
	var limit *int
	switch name {
	case "review_queue":
		var input pageInput
		if strictjson.Decode(data, &input) != nil {
			return args, invalidArguments()
		}
		args.limit, limit, args.cursor = 5, input.Limit, input.Cursor
	case "review_pull":
		var input pullInput
		if strictjson.Decode(data, &input) != nil {
			return args, invalidArguments()
		}
		args.pullNumber, args.snapshot = input.Number, input.Snapshot
	case "review_runs":
		var input runsInput
		if strictjson.Decode(data, &input) != nil {
			return args, invalidArguments()
		}
		args.pullNumber, args.snapshot, args.limit, limit, args.cursor = input.Number, input.Snapshot, 5, input.Limit, input.Cursor
	case "review_run":
		var input runInput
		if strictjson.Decode(data, &input) != nil || !validReviewID(input.ID, "review_run_") {
			return args, invalidArguments()
		}
		args.id, args.snapshot = input.ID, input.Snapshot
	}
	if name == "review_pull" || name == "review_runs" {
		if args.pullNumber < 1 || args.pullNumber > 2147483647 {
			return args, invalidArguments()
		}
	}
	if _, exists := fields["snapshot"]; exists && !validReviewID(args.snapshot, "review_snapshot_") {
		return args, invalidArguments()
	}
	if _, exists := fields["cursor"]; exists && !validEventCursor(args.cursor) {
		return args, invalidArguments()
	}
	if limit != nil {
		if *limit < 1 || *limit > 100 {
			return args, invalidArguments()
		}
		args.limit = *limit
	}
	return args, nil
}

func validReviewID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+32 {
		return false
	}
	for _, c := range value[len(prefix):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func reviewDefinitions() []definition {
	limit := map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 5, "description": "Items in this explicit page; defaults to 5, with no automatic paging."}
	cursor := map[string]any{"type": "string", "minLength": 1, "maxLength": 4096, "description": "Signed continuation cursor from this scoped review page."}
	snapshot := map[string]any{"type": "string", "pattern": "^review_snapshot_[a-f0-9]{32}$", "description": "Optional retained snapshot for consistent historical drill-down; omission selects current."}
	number := map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}
	schema := func(properties map[string]any, required ...string) map[string]any {
		value := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			value["required"] = required
		}
		return value
	}
	return append([]definition{
		{"review_queue", "Read one compact stored review queue page, including retained merged history. No provider refresh, review execution or readiness verdict.", schema(map[string]any{"limit": limit, "cursor": cursor})},
		{"review_pull", "Read one stored PR summary and attempts, optionally from a retained snapshot. Historical observations do not establish current-head coverage or merge authorization.", schema(map[string]any{"pull_number": number, "snapshot": snapshot}, "pull_number")},
		{"review_runs", "Read one newest-first stored run-summary page for an explicit PR and optional snapshot; full findings are not loaded.", schema(map[string]any{"pull_number": number, "snapshot": snapshot, "limit": limit, "cursor": cursor}, "pull_number")},
		{"review_run", "Read explicit stored run findings, decisions, fix proof and source coverage limitations, without provider work or a readiness verdict.", schema(map[string]any{"run_id": map[string]any{"type": "string", "pattern": "^review_run_[a-f0-9]{32}$"}, "snapshot": snapshot}, "run_id")},
	}, reviewControlDefinitions()...)
}
