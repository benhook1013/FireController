package mcpadapter

import (
	"bytes"
	"encoding/json"

	"github.com/benhook1013/FireController/internal/strictjson"
)

func jobContextArguments(data json.RawMessage) (arguments, error) {
	var args arguments
	if len(data) > 8<<10 || strictjson.Validate(data) != nil {
		return args, invalidArguments()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return args, invalidArguments()
	}
	for _, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return args, invalidArguments()
		}
	}
	var in struct {
		JobID string `json:"job_id"`
		Limit *int   `json:"limit,omitempty"`
	}
	if strictjson.Decode(data, &in) != nil || !validID(in.JobID) {
		return args, invalidArguments()
	}
	args.id = in.JobID
	args.limit = 20
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > 100 {
			return args, invalidArguments()
		}
		args.limit = *in.Limit
	}
	return args, nil
}
func jobContextDefinition() definition {
	return definition{"job_context", "Read one explicit job's complete brief, latest checkpoint and checklist plus the bound worker's focus, pause and pending coordination in one authorized snapshot. Notes/messages are previews even when short; complete/omitted state exactly what remains to read. Default 20, maximum 100 per pending page. Follow ordinary cursors or selected full reads explicitly; no automatic pagination, provider calls, acknowledgement or state change.", map[string]any{"type": "object", "properties": map[string]any{"job_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}, "required": []string{"job_id"}, "additionalProperties": false}}
}
