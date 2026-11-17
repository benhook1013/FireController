package mcpadapter

import (
	"encoding/json"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func followingArguments(name string, raw json.RawMessage) (arguments, error) {
	var args arguments
	var in struct {
		Number    int64  `json:"pull_number"`
		Revision  *int64 `json:"expected_revision,omitempty"`
		Following *bool  `json:"following,omitempty"`
		Key       string `json:"idempotency_key,omitempty"`
	}
	if strictjson.Decode(raw, &in) != nil || in.Number < 1 || in.Number > 2147483647 {
		return args, invalidArguments()
	}
	if name == "review_following_read" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || len(fields) != 1 {
			return args, invalidArguments()
		}
	} else {
		if in.Revision == nil || *in.Revision < 0 || *in.Revision == math.MaxInt64 || in.Following == nil || !validKey(in.Key) {
			return args, invalidArguments()
		}
		args.following = client.ConfigureReviewFollowingRequest{ExpectedRevision: *in.Revision, Following: *in.Following}
		args.key = in.Key
	}
	args.pullNumber = in.Number
	return args, nil
}
func followingDefinitions() []definition {
	number := map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}
	return []definition{
		{"review_following_read", "Read this bound worker's PR notification preference and whether the PR is enrolled. Ordinary explicit work follows automatically; this read changes nothing.", map[string]any{"type": "object", "additionalProperties": false, "required": []string{"pull_number"}, "properties": map[string]any{"pull_number": number}}},
		{"review_following_update", "Follow or unfollow this bound worker's durable merged/closed PR inbox notices. Require current expected_revision (zero if none) and explicit key; following=false persists against later automatic following. Does not enroll PRs, assign ownership, request reviews or alter CI/merge permissions. Preserve exact arguments/key after unknown outcome. Existing terminal history is not replayed.", map[string]any{"type": "object", "additionalProperties": false, "required": []string{"pull_number", "expected_revision", "following", "idempotency_key"}, "properties": map[string]any{"pull_number": number, "expected_revision": map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64 - 1)}, "following": map[string]any{"type": "boolean"}, "idempotency_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}}}},
	}
}
