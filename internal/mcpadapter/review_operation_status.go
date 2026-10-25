package mcpadapter

import (
	"context"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type operationStatusInput struct {
	ID          string
	WaitSeconds int
	Diagnostics bool
}

func operationStatusArguments(raw json.RawMessage) (*operationStatusInput, error) {
	var in struct {
		ID          string `json:"operation_id"`
		Wait        *int   `json:"wait_seconds,omitempty"`
		Diagnostics *bool  `json:"diagnostics,omitempty"`
	}
	if strictjson.Decode(raw, &in) != nil || client.ValidateReviewOperationID(in.ID) != nil {
		return nil, invalidArguments()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields["wait_seconds"] != nil && in.Wait == nil || fields["diagnostics"] != nil && in.Diagnostics == nil {
		return nil, invalidArguments()
	}
	out := &operationStatusInput{ID: in.ID}
	if in.Wait != nil {
		out.WaitSeconds = *in.Wait
	}
	if in.Diagnostics != nil {
		out.Diagnostics = *in.Diagnostics
	}
	if client.ValidateReviewOperationWait(out.WaitSeconds) != nil || out.WaitSeconds > maxToolWaitSeconds {
		return nil, invalidArguments()
	}
	return out, nil
}
func executeOperationStatus(ctx context.Context, service *client.Client, in *operationStatusInput) (any, error) {
	if in.Diagnostics {
		operation, err := service.WaitReviewOperation(ctx, in.ID, in.WaitSeconds)
		return client.AgentReviewOperation(operation), err
	}
	status, err := service.WaitReviewOperationStatus(ctx, in.ID, in.WaitSeconds)
	return client.AgentReviewOperationStatus(status), err
}
func operationStatusDefinition() definition {
	return definition{"review_operation_read", "Read compact retained operation state, cause and next action; optionally wait without provider polling. wait_for_observation means the issued Hosted trigger remains with scheduled observation: do other work and check later, without reposting or immediate polling. Uncertain states end the bounded result wait. A posted trigger is not proof of provider execution or completion. Set diagnostics=true for the original full retained operation.", map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"operation_id": map[string]any{"type": "string", "pattern": "^review_operation_[a-f0-9]{32}$"}, "wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": maxToolWaitSeconds, "default": 0}, "diagnostics": map[string]any{"type": "boolean", "default": false}}, "required": []string{"operation_id"}}}
}
