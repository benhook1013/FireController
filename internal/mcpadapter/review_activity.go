package mcpadapter

import (
	"bytes"
	"encoding/json"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func reviewActivityArguments(raw json.RawMessage) (arguments, error) {
	var args arguments
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if len(raw) > 4096 || strictjson.Decode(raw, &args.reviewActivity) != nil {
		return args, invalidArguments()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return args, invalidArguments()
	}
	for key, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return args, invalidArguments()
		}
		if (key == "hours" && args.reviewActivity.Hours == 0) || (key == "pull_number" && args.reviewActivity.PullNumber == 0) || (key == "since" && args.reviewActivity.Since == "") || (key == "until" && args.reviewActivity.Until == "") || (key == "timezone" && args.reviewActivity.Timezone == "") {
			return args, invalidArguments()
		}
	}
	if client.ValidateReviewActivityOptions(args.reviewActivity) != nil {
		return args, invalidArguments()
	}
	return args, nil
}

func reviewActivityDefinition() definition {
	return definition{"review_activity_read", "Read completed Hosted/CLI review activity and separate attempt diagnostics for this project or one PR, from current retained source only. One provider-free read: no enrollment, configuration, quota check, history drain or round credit. Use hours (default 24, 1–168) OR paired since/until timestamps spanning at most 7 days; UTC by default or an IANA timezone for offset-labelled hourly buckets. Partial buckets are clipped to the selected elapsed window. Coverage limits and undated records remain explicit. See docs/review-activity.md.", map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"hours":       map[string]any{"type": "integer", "minimum": 1, "maximum": 168},
		"since":       map[string]any{"type": "string", "minLength": 1, "maxLength": 64},
		"until":       map[string]any{"type": "string", "minLength": 1, "maxLength": 64},
		"timezone":    map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
		"pull_number": map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647},
	}}}
}
