package mcpadapter

import (
	"encoding/json"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func reviewControlTool(name string) bool {
	switch name {
	case "review_evidence_read", "review_adjudication_read", "review_finding_decide", "review_finding_fix", "review_source_finalize", "review_execution_read", "review_operation_read", "review_request":
		return true
	}
	return false
}

func reviewControlArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	switch name {
	case "review_execution_read":
		if strictjson.Decode(data, &struct{}{}) != nil {
			return args, invalidArguments()
		}
		return args, nil
	case "review_operation_read":
		var in struct {
			ID string `json:"operation_id"`
		}
		if strictjson.Decode(data, &in) != nil || client.ValidateReviewOperationID(in.ID) != nil {
			return args, invalidArguments()
		}
		args.id = in.ID
		return args, nil
	case "review_request":
		var in struct {
			Key      string `json:"idempotency_key"`
			Revision *int64 `json:"expected_configuration_revision"`
			Number   *int64 `json:"pull_number"`
			Channel  string `json:"channel"`
			Head     string `json:"head_sha"`
		}
		if strictjson.Decode(data, &in) != nil || in.Revision == nil || in.Number == nil || !validKey(in.Key) {
			return args, invalidArguments()
		}
		args.key = in.Key
		args.reviewRequest = client.ReviewRequest{ExpectedConfigurationRevision: *in.Revision, PullNumber: *in.Number, Channel: in.Channel, HeadSHA: in.Head}
		if client.ValidateReviewRequest(args.reviewRequest) != nil {
			return args, invalidArguments()
		}
		return args, nil
	case "review_evidence_read", "review_adjudication_read":
		var in struct {
			ID string `json:"evidence_id"`
		}
		if strictjson.Decode(data, &in) != nil {
			return args, invalidArguments()
		}
		args.id = in.ID
	case "review_finding_decide":
		var in struct {
			ID          string `json:"evidence_id"`
			Key         string `json:"idempotency_key"`
			Revision    *int64 `json:"expected_revision"`
			Finding     *int64 `json:"finding"`
			Disposition string `json:"disposition"`
			Reason      string `json:"reason"`
		}
		if strictjson.Decode(data, &in) != nil || in.Revision == nil || in.Finding == nil {
			return args, invalidArguments()
		}
		args.id, args.key = in.ID, in.Key
		args.reviewDecision = client.ReviewDecisionRequest{ExpectedRevision: *in.Revision, Finding: *in.Finding, Disposition: in.Disposition, Reason: in.Reason}
		if client.ValidateReviewDecisionRequest(args.reviewDecision) != nil {
			return args, invalidArguments()
		}
	case "review_finding_fix":
		var in struct {
			ID          string  `json:"evidence_id"`
			Key         string  `json:"idempotency_key"`
			Revision    *int64  `json:"expected_revision"`
			Finding     *int64  `json:"finding"`
			ExpectedSHA *string `json:"expected_sha"`
			FixSHA      string  `json:"fix_sha"`
			ProofNote   string  `json:"proof_note"`
		}
		if strictjson.Decode(data, &in) != nil || in.Revision == nil || in.Finding == nil || in.ExpectedSHA == nil {
			return args, invalidArguments()
		}
		args.id, args.key = in.ID, in.Key
		args.reviewFix = client.ReviewFixRequest{ExpectedRevision: *in.Revision, Finding: *in.Finding, ExpectedSHA: *in.ExpectedSHA, FixSHA: in.FixSHA, ProofNote: in.ProofNote}
		if client.ValidateReviewFixRequest(args.reviewFix) != nil {
			return args, invalidArguments()
		}
	case "review_source_finalize":
		var in struct {
			ID       string `json:"evidence_id"`
			Key      string `json:"idempotency_key"`
			Revision *int64 `json:"expected_revision"`
		}
		if strictjson.Decode(data, &in) != nil || in.Revision == nil {
			return args, invalidArguments()
		}
		args.id, args.key = in.ID, in.Key
		args.reviewFinalize = client.ReviewFinalizeRequest{ExpectedRevision: *in.Revision}
		if client.ValidateReviewFinalizeRequest(args.reviewFinalize) != nil {
			return args, invalidArguments()
		}
	}
	if client.ValidateReviewEvidenceID(args.id) != nil || mutationTool(name) && !validKey(args.key) {
		return args, invalidArguments()
	}
	return args, nil
}

func reviewControlDefinitions() []definition {
	id := map[string]any{"type": "string", "minLength": 48, "maxLength": 48, "pattern": "^review_evidence_[a-f0-9]{32}$"}
	key := map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "Explicit printable ASCII idempotency key; retain the same key and exact request after an unknown outcome. Never generated or automatically retried."}
	revision := map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64 - 1), "description": "Explicit current adjudication revision; zero selects the initial undecided state."}
	finding := map[string]any{"type": "integer", "minimum": 0, "maximum": 99, "description": "Explicit zero-based finding index from the immutable evidence, from 0 through 99."}
	text := map[string]any{"type": "string", "minLength": 1, "maxLength": 2048, "description": "Exact nonblank UTF-8 text, at most 2048 bytes; Unicode controls except tab, CR and LF are forbidden."}
	schema := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	return []definition{
		{"review_evidence_read", "Explicitly read full native immutable review evidence and parsed finding text, bounded to 4 MiB. No raw capture, provider refresh or execution.", schema(map[string]any{"evidence_id": id}, "evidence_id")},
		{"review_adjudication_read", "Explicitly read current native finding decisions and fix proof without finding text, provider refresh or execution.", schema(map[string]any{"evidence_id": id}, "evidence_id")},
		{"review_finding_decide", "Record or correct a native finding decision using an explicit key, revision and zero-based index; retain audit history and return a compact receipt. No provider work or automatic retry.", schema(map[string]any{"evidence_id": id, "idempotency_key": key, "expected_revision": revision, "finding": finding, "disposition": map[string]any{"type": "string", "enum": []string{"accepted", "rejected"}}, "reason": text}, "evidence_id", "idempotency_key", "expected_revision", "finding", "disposition", "reason")},
		{"review_finding_fix", "Record or correct accepted-finding fix proof with an explicit key, revision, zero-based index and expected current fix SHA; retain audit history and return a compact receipt. No provider work or automatic retry.", schema(map[string]any{"evidence_id": id, "idempotency_key": key, "expected_revision": revision, "finding": finding, "expected_sha": map[string]any{"type": "string", "maxLength": 40, "pattern": "^(|[a-f0-9]{40})$", "description": "Required even when empty; empty asserts that no fix SHA is currently recorded."}, "fix_sha": map[string]any{"type": "string", "minLength": 40, "maxLength": 40, "pattern": "^[a-f0-9]{40}$"}, "proof_note": text}, "evidence_id", "idempotency_key", "expected_revision", "finding", "expected_sha", "fix_sha", "proof_note")},
		{"review_source_finalize", "Finalize eligible native source counts with an explicit key and revision; return a compact receipt. No provider work, review execution or automatic retry.", schema(map[string]any{"evidence_id": id, "idempotency_key": key, "expected_revision": revision}, "evidence_id", "idempotency_key", "expected_revision")},
	}
}

func reviewExecutionDefinitions() []definition {
	schema := func(properties map[string]any, required ...string) map[string]any {
		result := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			result["required"] = required
		}
		return result
	}
	return []definition{
		{"review_execution_read", "Read stored project review execution configuration. No provider calls, execution, readiness verdict or automatic polling.", schema(map[string]any{})},
		{"review_operation_read", "Read one explicit local review operation status. No provider refresh, execution or automatic polling.", schema(map[string]any{"operation_id": map[string]any{"type": "string", "pattern": "^review_operation_[a-f0-9]{32}$"}}, "operation_id")},
		{"review_request", "Enqueue the selected external provider review, which can consume included allowance asynchronously. The compact queued receipt does not mean started or completed. Retain the exact idempotency key and arguments for uncertain-outcome recovery; no automatic retry, generated key or operation polling.", schema(map[string]any{
			"idempotency_key":                 map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"expected_configuration_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64)},
			"pull_number":                     map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647},
			"channel":                         map[string]any{"type": "string", "enum": []string{"cli", "hosted"}},
			"head_sha":                        map[string]any{"type": "string", "pattern": "^[a-f0-9]{40}$"},
		}, "idempotency_key", "expected_configuration_revision", "pull_number", "channel", "head_sha")},
	}
}
