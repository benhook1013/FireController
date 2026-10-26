package mcpadapter

import (
	"encoding/json"
	"math"

	"github.com/benhook1013/FireController/internal/strictjson"
)

func jobRevisionArguments(data json.RawMessage) (arguments, error) {
	var in struct {
		JobID    string `json:"job_id"`
		Revision *int64 `json:"revision"`
	}
	if strictjson.Decode(data, &in) != nil || !validID(in.JobID) || in.Revision == nil || *in.Revision < 1 {
		return arguments{}, invalidArguments()
	}
	return arguments{id: in.JobID, jobRevision: *in.Revision}, nil
}

func jobRevisionDefinition() definition {
	return definition{"job_revision_read", "Read exactly one retained job revision, including its original full standing brief and historical state. Required job_id and revision. No pagination, current notes/context, provider work or writes. A missing retained revision returns not_found; it never falls back to the current job or recreates unimported source history.", map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"job_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64)}}, "required": []string{"job_id", "revision"}}}
}
