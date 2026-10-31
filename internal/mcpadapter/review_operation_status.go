package mcpadapter

import (
	"context"
	"encoding/json"
	"fmt"
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
		return nil, &Diagnostic{Code: "invalid_arguments", Message: fmt.Sprintf("wait_seconds must be from 0 to %d seconds for MCP and CLI review_operation_read.", maxToolWaitSeconds), Action: fmt.Sprintf("Use wait_seconds from 0 to %d (default 0). No operation request was sent.", maxToolWaitSeconds), Details: map[string]any{"field": "wait_seconds", "minimum": 0, "maximum": maxToolWaitSeconds}}
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
	return definition{"review_operation_read", fmt.Sprintf("Read compact retained operation state, cause and next action; optionally wait without provider polling. MCP wait_seconds accepts 0-%d seconds (default 0), matching the CLI. With evidence, review_finished_at dates review finish and evidence_recorded_at dates controller retention; updated_at remains the operation transition. wait_for_observation means the issued Hosted trigger remains with scheduled observation: do other work and check later, without reposting or immediate polling. Uncertain states end the bounded result wait. A posted trigger is not proof of provider execution or completion. Pending Hosted observer metadata reports actual process-local checks and the next scheduled check; it does not prove provider activity. Unknown after restart carries no check claim. Queued wait metadata retains the same request: retry_not_before is the earliest automatic recheck, not a promised start; paused requires explicit review_operation_control resume. Do not submit a replacement for a queued wait. A later held target_changed is terminal: qualify the current open comparison before a fresh authorized request/key, respecting known cooldowns; queued-only controls cannot rebind it. Set diagnostics=true for the full retained operation, preserving its directly bound evidence handle when present.", maxToolWaitSeconds), map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"operation_id": map[string]any{"type": "string", "pattern": "^review_operation_[a-f0-9]{32}$"}, "wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": maxToolWaitSeconds, "default": 0}, "diagnostics": map[string]any{"type": "boolean", "default": false}}, "required": []string{"operation_id"}}}
}
