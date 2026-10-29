package mcpadapter

import (
	"encoding/json"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func reviewBatchTool(name string) bool {
	return name == "review_findings_decide" || name == "review_findings_fix"
}
func reviewBatchArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	var wire struct {
		ID       string          `json:"evidence_id"`
		Key      string          `json:"idempotency_key"`
		Revision json.RawMessage `json:"expected_revision"`
		Items    json.RawMessage `json:"items"`
	}
	if !reviewBatchTool(name) || len(data) > client.ReviewBatchMaxBytes+1024 || strictjson.Decode(data, &wire) != nil || client.ValidateReviewEvidenceID(wire.ID) != nil || !validKey(wire.Key) || len(wire.Revision) == 0 || len(wire.Items) == 0 {
		return args, invalidArguments()
	}
	body, _ := json.Marshal(struct {
		Revision json.RawMessage `json:"expected_revision"`
		Items    json.RawMessage `json:"items"`
	}{wire.Revision, wire.Items})
	var err error
	if name == "review_findings_decide" {
		args.reviewDecisionBatch, err = client.DecodeReviewDecisionBatchRequest(body)
	} else {
		args.reviewFixBatch, err = client.DecodeReviewFixBatchRequest(body)
	}
	if err != nil {
		return arguments{}, invalidReviewArguments(err)
	}
	args.id, args.key = wire.ID, wire.Key
	return args, nil
}

const expectedFixSHADescription = "The fix_sha already stored on this record; empty for the first fix. This compares the current stored value, not the PR head or original reviewed SHA."

func reviewBatchDefinitions() []definition {
	object := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	id := map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
	revision := map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64 - 1)}
	finding := map[string]any{"type": "integer", "minimum": 0, "maximum": 99, "description": "Explicit zero-based finding index; no duplicates within a batch."}
	text := map[string]any{"type": "string", "minLength": 1, "maxLength": 2048, "description": "Explicit nonblank text, at most 2048 UTF-8 bytes."}
	decision := object(map[string]any{"finding": finding, "disposition": map[string]any{"type": "string", "enum": []string{"accepted", "rejected"}}, "reason": text, "expected_route_revision": revision}, "finding", "disposition", "reason")
	fix := object(map[string]any{"finding": finding, "expected_sha": map[string]any{"type": "string", "pattern": "^(|[a-f0-9]{40})$", "maxLength": 40, "description": expectedFixSHADescription}, "fix_sha": map[string]any{"type": "string", "pattern": "^[a-f0-9]{40}$", "minLength": 40, "maxLength": 40, "description": "Full 40-character lowercase hexadecimal fix commit SHA; abbreviations are rejected."}, "proof_note": text}, "finding", "expected_sha", "fix_sha", "proof_note")
	fields := func(item map[string]any) map[string]any {
		return object(map[string]any{"evidence_id": id, "idempotency_key": id, "expected_revision": revision, "items": map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "items": item}}, "evidence_id", "idempotency_key", "expected_revision", "items")
	}
	return []definition{
		{"review_findings_decide", "Atomically decide 1–100 explicitly selected unique native findings with one revision and idempotency key. Every disposition/reason is explicit; routed source corrections retain route guards. One compact receipt, no implicit finalization, provider work or retry.", fields(decision)},
		{"review_findings_fix", "Atomically record fix proof for 1–100 explicitly selected unique accepted native findings with one revision/key and expected_sha equal to each finding's currently stored fix_sha (empty for the first fix). fix_sha names the new fix commit. One compact receipt; retains source counts and receiving-route authority. No implicit finalization, provider work or retry.", fields(fix)},
	}
}
