package mcpadapter

import (
	"bytes"
	"encoding/json"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const externalHostedSettlementTool = "review_external_hosted_settle"

func externalHostedSettlementArguments(data json.RawMessage) (arguments, error) {
	var in struct {
		PullNumber int64           `json:"pull_number"`
		Key        string          `json:"idempotency_key"`
		CountRound json.RawMessage `json:"count_round"`
		Runs       json.RawMessage `json:"runs"`
	}
	if len(data) > client.ExternalHostedSettlementMaxBytes+1024 || strictjson.Decode(data, &in) != nil || in.PullNumber < 1 || in.PullNumber > 2147483647 || !validKey(in.Key) {
		return arguments{}, invalidArguments()
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(struct {
		CountRound json.RawMessage `json:"count_round"`
		Runs       json.RawMessage `json:"runs"`
	}{in.CountRound, in.Runs}); err != nil {
		return arguments{}, invalidArguments()
	}
	value, err := client.DecodeExternalHostedSettlementRequest(body.Bytes())
	if err != nil {
		return arguments{}, invalidReviewArguments(err)
	}
	return arguments{pullNumber: in.PullNumber, key: in.Key, externalHostedSettlement: value}, nil
}

func externalHostedSettlementDefinition() definition {
	object := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	boundedText := func(maximum int) map[string]any {
		return map[string]any{"type": "string", "minLength": 1, "maxLength": maximum}
	}
	finding := object(map[string]any{
		"finding_id":  boundedText(256),
		"disposition": map[string]any{"type": "string", "enum": []string{"accepted", "rejected"}},
		"reason":      boundedText(2048),
		"fix_sha":     map[string]any{"type": "string", "pattern": "^[a-f0-9]{40}$"},
		"proof_note":  boundedText(2048),
	}, "finding_id", "disposition", "reason")
	run := object(map[string]any{
		"run_id":   map[string]any{"type": "string", "pattern": "^review_run_[a-f0-9]{32}$"},
		"findings": map[string]any{"type": "array", "maxItems": 100, "items": finding},
	}, "run_id", "findings")
	return definition{
		externalHostedSettlementTool,
		"Settle retained CodeRabbit Hosted results started directly on GitHub for one PR. Read review_runs and review_run for stable run_id and source_finding_id values. Submit 1–10 unique runs with every finding explicitly decided, at most 100 total, and [] for a zero-finding run. Reasons and proof are nonblank, at most 2048 UTF-8 bytes. Accepted findings require the full lowercase fix_sha and proof_note. Omit both fields for rejected findings. Set count_round:true to count the batch as one Hosted round or false to settle without round credit. Use the same key, inputs and bound connection after an unknown outcome.",
		object(map[string]any{
			"pull_number":     map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647},
			"idempotency_key": boundedText(128),
			"count_round":     map[string]any{"type": "boolean"},
			"runs":            map[string]any{"type": "array", "minItems": 1, "maxItems": 10, "items": run},
		}, "pull_number", "idempotency_key", "count_round", "runs"),
	}
}
