package mcpadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func subagentTool(name string) bool {
	return name == "review_subagent_record" || name == "review_subagent_read" || name == "review_subagent_update"
}
func subagentWrite(name string) bool {
	return name == "review_subagent_record" || name == "review_subagent_update"
}

func subagentArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	// Binding selectors and the key are outside the bounded public document.
	if !subagentTool(name) || len(data) > (256<<10)+1024 || strictjson.Validate(data) != nil {
		return args, invalidArguments()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return args, invalidArguments()
	}
	for _, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return args, invalidArguments()
		}
	}
	if name != "review_subagent_record" {
		if strictjson.Decode(fields["run_id"], &args.id) != nil || client.ValidateSubagentRunID(args.id) != nil {
			return args, invalidArguments()
		}
		delete(fields, "run_id")
	}
	if name == "review_subagent_read" {
		if len(fields) != 0 {
			return arguments{}, invalidArguments()
		}
		return args, nil
	}
	if strictjson.Decode(fields["idempotency_key"], &args.key) != nil || !validKey(args.key) {
		return arguments{}, invalidArguments()
	}
	delete(fields, "idempotency_key")
	document, err := json.Marshal(fields)
	if err != nil {
		return arguments{}, invalidArguments()
	}
	if name == "review_subagent_record" {
		args.subagentRecord, err = client.DecodeSubagentRecordDocument(document)
	} else {
		args.subagentUpdate, err = client.DecodeSubagentUpdateDocument(document)
	}
	if err != nil {
		var field *client.Error
		if name == "review_subagent_record" && errors.As(err, &field) && field.Code == "validation_failed" {
			// The canonical document validator owns these field names and
			// requirements; it never includes private request values.
			return arguments{}, diagnostic("invalid_arguments", field.Message, field.Action+" No request was sent.")
		}
		return arguments{}, invalidArguments()
	}
	return args, nil
}

func subagentDefinitions() []definition {
	object := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	text := func(max int) map[string]any {
		return map[string]any{"type": "string", "minLength": 1, "maxLength": max}
	}
	key := text(128)
	key["description"] = "Explicit idempotency key; recover an unknown outcome with this same key and exact request."
	id := map[string]any{"type": "string", "pattern": "^review_run_[a-f0-9]{32}$"}
	sha := map[string]any{"type": "string", "pattern": "^[a-f0-9]{40}$", "minLength": 40, "maxLength": 40}
	decision := map[string]any{"type": "string", "enum": []string{"accepted", "rejected"}}
	finding := object(map[string]any{"title": text(500), "detail": map[string]any{"type": "string", "maxLength": 16384}, "severity": map[string]any{"type": "string", "enum": []string{"info", "low", "medium", "high", "critical", "unknown"}}, "disposition": map[string]any{"type": "string", "enum": []string{"unadjudicated", "accepted", "rejected"}}, "reason": text(2048)}, "title", "detail", "severity")
	index := map[string]any{"type": "integer", "minimum": 0, "maximum": 99}
	decisions := object(map[string]any{"finding": index, "disposition": decision, "reason": text(2048)}, "finding", "disposition", "reason")
	fixes := object(map[string]any{"finding": index, "expected_sha": map[string]any{"type": "string", "pattern": "^(|[a-f0-9]{40})$", "maxLength": 40}, "fix_sha": sha, "proof_note": text(2048)}, "finding", "expected_sha", "fix_sha", "proof_note")
	array := func(item map[string]any) map[string]any {
		return map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "items": item}
	}
	stamp := map[string]any{"type": "string", "format": "date-time", "maxLength": 128}
	reviewer := text(128)
	reviewer["description"] = "Nonblank reviewer label, at most 128 UTF-8 bytes, not characters."
	scope := text(128)
	scope["description"] = "Short nonblank comparison label, at most 128 UTF-8 bytes, not characters. Put longer descriptions and limitations in coverage_limits."
	return []definition{
		{"review_subagent_record", "Record one actual completed subagent review under this worker's project, retaining reviewed head, scope, times and finding text. Reviewer and scope are nonblank labels of at most 128 UTF-8 bytes each; longer limitations belong in coverage_limits. Counts/identities are derived. Optional finalized requires explicit decisions; zero findings is valid. Ordinary writer access, one compact receipt, no provider work or CodeRabbit round credit.", object(map[string]any{"idempotency_key": key, "pull_number": map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}, "head_sha": sha, "reviewer": reviewer, "scope": scope, "started_at": stamp, "finished_at": stamp, "coverage_limits": map[string]any{"type": "array", "maxItems": 16, "items": text(1024)}, "findings": map[string]any{"type": "array", "maxItems": 100, "items": finding}, "finalized": map[string]any{"type": "boolean"}}, "idempotency_key", "pull_number", "head_sha", "reviewer", "scope", "started_at", "finished_at", "findings")},
		{"review_subagent_read", "Read one recorded subagent review, its stable run revision, zero-based finding indexes, current decisions and fix proof. Explicit detail read up to 4 MiB; no provider work or state change.", object(map[string]any{"run_id": id}, "run_id")},
		{"review_subagent_update", "Atomically update explicitly selected subagent finding decisions/fixes and optionally finalize with the current stable run revision and one key. Each reason/proof is explicit; omitted findings stay unchanged and finalized source counts remain frozen. No provider work, extra review, allowance change or automatic retry.", object(map[string]any{"run_id": id, "idempotency_key": key, "expected_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64 - 1)}, "decisions": array(decisions), "fixes": array(fixes), "finalize": map[string]any{"type": "boolean"}}, "run_id", "idempotency_key", "expected_revision")},
	}
}
