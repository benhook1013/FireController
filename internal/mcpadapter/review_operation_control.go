package mcpadapter

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
	"math"
)

func operationControlArguments(raw json.RawMessage) (arguments, error) {
	var in struct {
		ID       string `json:"operation_id"`
		Key      string `json:"idempotency_key"`
		Revision int64  `json:"expected_revision"`
		Action   string `json:"action"`
	}
	var out arguments
	if strictjson.Decode(raw, &in) != nil {
		return out, invalidArguments()
	}
	out.id, out.key = in.ID, in.Key
	out.operationControl = client.ReviewOperationControl{ExpectedRevision: in.Revision, Action: in.Action}
	if !validKey(in.Key) || client.ValidateReviewOperationControl(in.ID, out.operationControl) != nil {
		return arguments{}, invalidArguments()
	}
	return out, nil
}
func operationControlDefinition() definition {
	return definition{"review_operation_control", "Pause, resume or cancel only a queued undispatched operation using its current revision and explicit key. Pause retains the retry deadline; resume rechecks this same request when due. Cancel withdraws intent, never an issued provider request. Posting, active and uncertain work refuses. Preserve the original request/key after unknown output. No changes to grants, taper, findings or project pause.", map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"operation_id": map[string]any{"type": "string", "pattern": "^review_operation_[a-f0-9]{32}$"}, "expected_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64 - 1)}, "action": map[string]any{"type": "string", "enum": []string{"pause", "resume", "cancel"}}, "idempotency_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}}, "required": []string{"operation_id", "expected_revision", "action", "idempotency_key"}}}
}
