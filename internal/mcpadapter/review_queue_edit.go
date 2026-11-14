package mcpadapter

import (
	"encoding/json"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// This is the canonical project-management queue-edit document, independent of
// native review request policy and scheduling configuration.
func queueEditArguments(raw json.RawMessage) (arguments, error) {
	var in struct {
		Document json.RawMessage `json:"document"`
		Key      string          `json:"idempotency_key"`
	}
	if strictjson.Decode(raw, &in) != nil || client.ValidateReviewQueueEdit(in.Key, in.Document) != nil {
		return arguments{}, invalidArguments()
	}
	return arguments{key: in.Key, queueEdit: in.Document}, nil
}

func queueEditDefinition() definition {
	return definition{"review_queue_edit", "Apply the existing native queue edit document for selected metadata, explicit defunct/superseded retirement, reactivation or relative ordering. Requires project management or instance administration. Requires expected_revision, repository_id and source inside document plus an explicit idempotency key. Preserves evidence and execution policy; performs no provider work.", map[string]any{"type": "object", "additionalProperties": false, "required": []string{"document", "idempotency_key"}, "properties": map[string]any{"document": map[string]any{"type": "object", "description": "Strict native queue edit document, at most 256 KiB; up to 100 metadata/retirement entries plus up to 100 order entries. Validated by the typed client before service I/O."}, "idempotency_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}}}}
}
