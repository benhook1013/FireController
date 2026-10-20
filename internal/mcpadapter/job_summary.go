package mcpadapter

import (
	"encoding/json"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func jobSummaryArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	if name == "job_summary_read" {
		var in jobInput
		if strictjson.Decode(data, &in) != nil || !validID(in.JobID) {
			return args, invalidArguments()
		}
		args.id = in.JobID
		return args, nil
	}
	var in struct {
		ID          string  `json:"job_id"`
		Key         string  `json:"idempotency_key"`
		JobRevision *int64  `json:"expected_job_revision"`
		Revision    *int64  `json:"expected_revision"`
		Title       *string `json:"title"`
		Body        *string `json:"body"`
	}
	if strictjson.Decode(data, &in) != nil || !validID(in.ID) || !validKey(in.Key) || in.JobRevision == nil || in.Revision == nil || in.Title == nil || in.Body == nil {
		return args, invalidArguments()
	}
	args.summary = client.SetJobSummaryRequest{ExpectedJobRevision: *in.JobRevision, ExpectedRevision: *in.Revision, Title: *in.Title, Body: *in.Body}
	if client.ValidateJobSummaryRequest(args.summary) != nil {
		return args, invalidArguments()
	}
	args.id = in.ID
	args.key = in.Key
	return args, nil
}
func jobSummaryDefinitions() []definition {
	id := map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
	schema := func(fields map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": fields, "required": required, "additionalProperties": false}
	}
	return []definition{
		{"job_summary_read", "Read one deliberately authored shareable job summary, private until selected for publication. Missing summary returns revision zero and empty prose; never copies the private brief.", schema(map[string]any{"job_id": id}, "job_id")},
		{"job_summary_set", "Set an explicit summary title/body with job and summary revision guards and an idempotency key. Limits are 240/2000 UTF-8 bytes; both empty clears. Does not edit the private job or publish content.", schema(map[string]any{"job_id": id, "idempotency_key": id, "expected_job_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64)}, "expected_revision": map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64 - 1)}, "title": map[string]any{"type": "string", "maxLength": 240}, "body": map[string]any{"type": "string", "maxLength": 2000}}, "job_id", "idempotency_key", "expected_job_revision", "expected_revision", "title", "body")},
	}
}
