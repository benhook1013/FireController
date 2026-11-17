package mcpadapter

import (
	"encoding/json"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func checkpointTool(name string) bool {
	return name == "review_checkpoint_publish" || name == "review_checkpoint_read" || name == "review_checkpoint_recover"
}
func checkpointWrite(name string) bool {
	return name == "review_checkpoint_publish" || name == "review_checkpoint_recover"
}
func checkpointArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	if name == "review_checkpoint_read" {
		var in struct {
			ID string `json:"publication_id"`
		}
		if strictjson.Decode(data, &in) != nil || client.ValidateReviewCheckpointID(in.ID) != nil {
			return args, invalidArguments()
		}
		args.id = in.ID
		return args, nil
	}
	args.reviewSourceWait = 2
	if name == "review_checkpoint_publish" {
		var in struct {
			Key      string `json:"idempotency_key"`
			ID       string `json:"evidence_id"`
			Revision *int64 `json:"expected_revision"`
			Wait     *int   `json:"wait_seconds,omitempty"`
		}
		if strictjson.Decode(data, &in) != nil || in.Revision == nil || !validKey(in.Key) {
			return args, invalidArguments()
		}
		args.key = in.Key
		args.reviewCheckpoint = client.ReviewCheckpointRequest{EvidenceID: in.ID, ExpectedRevision: *in.Revision}
		if in.Wait != nil {
			args.reviewSourceWait = *in.Wait
		}
		if client.ValidateReviewCheckpointRequest(args.reviewCheckpoint) != nil || client.ValidateReviewSourceWait(args.reviewSourceWait) != nil || args.reviewSourceWait > maxToolWaitSeconds {
			return args, invalidArguments()
		}
		return args, nil
	}
	var in struct {
		Key      string `json:"idempotency_key"`
		ID       string `json:"publication_id"`
		Revision *int64 `json:"expected_revision"`
		Retry    bool   `json:"retry_unposted,omitempty"`
		Wait     *int   `json:"wait_seconds,omitempty"`
	}
	if strictjson.Decode(data, &in) != nil || in.Revision == nil || !validKey(in.Key) || client.ValidateReviewCheckpointID(in.ID) != nil {
		return args, invalidArguments()
	}
	args.key, args.id = in.Key, in.ID
	args.reviewCheckpointRecovery = client.ReviewCheckpointRecovery{ExpectedRevision: *in.Revision, RetryUnposted: in.Retry}
	if in.Wait != nil {
		args.reviewSourceWait = *in.Wait
	}
	if client.ValidateReviewCheckpointRecovery(args.reviewCheckpointRecovery) != nil || client.ValidateReviewSourceWait(args.reviewSourceWait) != nil || args.reviewSourceWait > maxToolWaitSeconds {
		return args, invalidArguments()
	}
	return args, nil
}
func checkpointDefinitions() []definition {
	schema := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	key := map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "Explicit idempotency key; recover unknown outcomes with this same key and exact request. No automatic retry."}
	id := map[string]any{"type": "string", "pattern": "^review_publication_[a-f0-9]{32}$"}
	wait := map[string]any{"type": "integer", "minimum": 0, "maximum": maxToolWaitSeconds, "default": 2, "description": "Explicit bounded result wait; zero returns a receipt if pending. No polling."}
	return []definition{
		{"review_checkpoint_publish", "Explicitly publish frozen historical native source counts to GitHub; no review request or merge action. Keep the exact key after an unknown outcome.", schema(map[string]any{"idempotency_key": key, "evidence_id": map[string]any{"type": "string", "pattern": "^review_evidence_[a-f0-9]{32}$"}, "expected_revision": map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64)}, "wait_seconds": wait}, "idempotency_key", "evidence_id", "expected_revision")},
		{"review_checkpoint_read", "Read one compact stored public checkpoint operation; no provider calls or implicit recovery.", schema(map[string]any{"publication_id": id}, "publication_id")},
		{"review_checkpoint_recover", "Explicitly recover a checkpoint using bounded provider observation; retry_unposted only permits a positively proven unposted action. Uncertain posts are never blindly retried.", schema(map[string]any{"idempotency_key": key, "publication_id": id, "expected_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64 - 1)}, "retry_unposted": map[string]any{"type": "boolean", "default": false}, "wait_seconds": wait}, "idempotency_key", "publication_id", "expected_revision")},
	}
}
