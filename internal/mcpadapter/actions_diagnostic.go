package mcpadapter

import (
	"bytes"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func actionsDiagnosticArguments(raw json.RawMessage) (arguments, error) {
	var args arguments
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if len(raw) > 4096 || strictjson.Decode(raw, &args.actionsDiagnostic) != nil {
		return args, invalidArguments()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return args, invalidArguments()
	}
	for key, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || key == "age_hours" && args.actionsDiagnostic.AgeHours == 0 || key == "limit" && args.actionsDiagnostic.Limit == 0 {
			return args, invalidArguments()
		}
	}
	if client.ValidateActionsDiagnosticOptions(args.actionsDiagnostic) != nil {
		return args, invalidArguments()
	}
	return args, nil
}
func actionsDiagnosticDefinition() definition {
	return definition{"actions_liveness_read", "Explicit bounded read-only Actions update-age diagnostic for this bound project's configured repository, including obsolete heads. Uses existing source workers and at most 10 GETs in 15 seconds per scan; no automatic retry or recurring scan. Returns compact oldest aged rows with complete run/attempt/head identity and omitted counts. Non-atomic buckets cannot prove no runs exist; age is not death/no-progress, capacity is unknown. No cancellation, restart, review, required-CI verdict or recovery authority. See docs/provider-observations.md.", map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"age_hours": map[string]any{"type": "integer", "minimum": 1, "maximum": 720}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20}}}}
}
