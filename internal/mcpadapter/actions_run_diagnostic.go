package mcpadapter

import (
	"bytes"
	"encoding/json"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func actionsRunDiagnosticArguments(raw json.RawMessage) (arguments, error) {
	var args arguments
	if len(raw) == 0 || len(raw) > 4096 || strictjson.Decode(raw, &args.actionsRunDiagnostic) != nil {
		return args, invalidArguments()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return args, invalidArguments()
	}
	for key, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || key == "limit" && args.actionsRunDiagnostic.Limit == 0 {
			return args, invalidArguments()
		}
	}
	if client.ValidateActionsRunDiagnosticOptions(args.actionsRunDiagnostic) != nil {
		return args, invalidArguments()
	}
	return args, nil
}
func actionsRunDiagnosticDefinition() definition {
	return definition{"actions_run_read", "Read jobs for an exact Actions run attempt. This read-only diagnostic covers the bound project repository, including obsolete heads. Requires expected run_attempt; a rerun or changed identity/status refuses rather than silently switching. Shares existing source workers and the one-shot diagnostic permit; at most 10 GETs in 15 seconds, compact counts/rows with omissions. Non-atomic observed job states are not a process heartbeat or proof of death, capacity, required CI or recovery authority. No logs, runner names, recurring scan, automatic retry, cancellation or restart. See docs/provider-observations.md.", map[string]any{"type": "object", "additionalProperties": false, "required": []string{"run_id", "run_attempt"}, "properties": map[string]any{"run_id": map[string]any{"type": "integer", "minimum": 1}, "run_attempt": map[string]any{"type": "integer", "minimum": 1}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20}}}}
}
