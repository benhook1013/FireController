package mcpadapter

import (
	"context"
	"encoding/json"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func jobStatusArguments(data json.RawMessage) (arguments, error) {
	var in jobInput
	if len(data) > 16<<10 || strictjson.Decode(data, &in) != nil || !validID(in.JobID) {
		return arguments{}, invalidArguments()
	}
	return arguments{id: in.JobID}, nil
}

func runJobStatus(ctx context.Context, service *client.Client, args arguments) (any, error) {
	return service.JobStatus(ctx, args.id)
}

func jobStatusDefinitions() []definition {
	return []definition{{"job_status_read", "Read compact current job status and bounded progress/next/blocker previews with explicit truncation flags and evidence count. Does not load the brief, checklist, notes or history; use job_read for resume context.", map[string]any{"type": "object", "properties": map[string]any{"job_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}}, "required": []string{"job_id"}, "additionalProperties": false}}}
}
