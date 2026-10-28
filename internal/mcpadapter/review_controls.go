package mcpadapter

import (
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func invalidHeadSHA(value string) error {
	if len(value) != 40 || strings.Trim(value, "0123456789abcdef") != "" {
		return diagnostic("invalid_arguments", "head_sha must contain exactly 40 lowercase hexadecimal characters.", "Use the full PR head SHA from Git or the provider; abbreviated SHAs are not accepted. No request was sent.")
	}
	return nil
}

func invalidReviewArguments(err error) error {
	var field *client.Error
	if errors.As(err, &field) && field.Code == "validation_failed" && field.Details != nil {
		return &Diagnostic{Code: "invalid_arguments", Message: field.Message, Action: field.Action, Details: field.Details}
	}
	return invalidArguments()
}

func reviewControlTool(name string) bool {
	switch name {
	case "review_policy_assess", "review_evidence_read", "review_adjudication_read", "review_finding_decide", "review_finding_fix", "review_source_finalize", "review_execution_read", "review_operation_read", "review_request", "review_source_refresh", "review_source_refresh_read", "review_source_policy_assess", "review_source_ci_assess", "review_source_readiness", "review_finding_route", "review_route_read", "review_routes_list", "review_route_decide", "review_route_fix":
		return true
	}
	return false
}

func reviewControlArguments(name string, data json.RawMessage) (arguments, error) {
	var args arguments
	switch name {
	case "review_route_read":
		var in struct {
			ID string `json:"route_id"`
		}
		if strictjson.Decode(data, &in) != nil || client.ValidateReviewRouteID(in.ID) != nil {
			return args, invalidArguments()
		}
		args.id = in.ID
		return args, nil
	case "review_routes_list":
		var in struct {
			Number *int64  `json:"pull_number"`
			Limit  *int    `json:"limit,omitempty"`
			Cursor *string `json:"cursor,omitempty"`
		}
		if strictjson.Decode(data, &in) != nil || in.Number == nil || *in.Number < 1 || *in.Number > 2147483647 {
			return args, invalidArguments()
		}
		args.pullNumber = *in.Number
		args.limit = 5
		if in.Limit != nil {
			if *in.Limit < 1 || *in.Limit > 100 {
				return args, invalidArguments()
			}
			args.limit = *in.Limit
		}
		if in.Cursor != nil {
			if !validEventCursor(*in.Cursor) {
				return args, invalidArguments()
			}
			args.cursor = *in.Cursor
		}
		return args, nil
	case "review_finding_route":
		var in struct {
			ID            string `json:"evidence_id"`
			Key           string `json:"idempotency_key"`
			Revision      *int64 `json:"expected_revision"`
			Finding       *int64 `json:"finding"`
			Target        *int64 `json:"target_pull_number"`
			Reason        string `json:"reason"`
			RouteRevision *int64 `json:"expected_route_revision,omitempty"`
		}
		if strictjson.Decode(data, &in) != nil || in.Revision == nil || in.Finding == nil || in.Target == nil || client.ValidateReviewEvidenceID(in.ID) != nil || !validKey(in.Key) {
			return args, invalidArguments()
		}
		args.id, args.key = in.ID, in.Key
		args.reviewRoute = client.ReviewRouteRequest{ExpectedRevision: *in.Revision, Finding: *in.Finding, TargetPullNumber: *in.Target, Reason: in.Reason}
		if in.RouteRevision != nil {
			args.reviewRoute.ExpectedRouteRevision = *in.RouteRevision
		}
		if client.ValidateReviewRouteRequest(args.reviewRoute) != nil {
			return args, invalidArguments()
		}
		return args, nil
	case "review_route_decide":
		var in struct {
			ID          string `json:"route_id"`
			Key         string `json:"idempotency_key"`
			Revision    *int64 `json:"expected_revision"`
			Disposition string `json:"disposition"`
			Reason      string `json:"reason"`
		}
		if strictjson.Decode(data, &in) != nil || in.Revision == nil || client.ValidateReviewRouteID(in.ID) != nil || !validKey(in.Key) {
			return args, invalidArguments()
		}
		args.id, args.key = in.ID, in.Key
		args.reviewRouteDecision = client.ReviewRouteDecisionRequest{ExpectedRevision: *in.Revision, Disposition: in.Disposition, Reason: in.Reason}
		if client.ValidateReviewRouteDecisionRequest(args.reviewRouteDecision) != nil {
			return args, invalidArguments()
		}
		return args, nil
	case "review_route_fix":
		var in struct {
			ID          string  `json:"route_id"`
			Key         string  `json:"idempotency_key"`
			Revision    *int64  `json:"expected_revision"`
			ExpectedSHA *string `json:"expected_sha"`
			FixSHA      string  `json:"fix_sha"`
			ProofNote   string  `json:"proof_note"`
		}
		if strictjson.Decode(data, &in) != nil || in.Revision == nil || in.ExpectedSHA == nil || client.ValidateReviewRouteID(in.ID) != nil || !validKey(in.Key) {
			return args, invalidArguments()
		}
		args.id, args.key = in.ID, in.Key
		args.reviewRouteFix = client.ReviewRouteFixRequest{ExpectedRevision: *in.Revision, ExpectedSHA: *in.ExpectedSHA, FixSHA: in.FixSHA, ProofNote: in.ProofNote}
		if err := client.ValidateReviewRouteFixRequest(args.reviewRouteFix); err != nil {
			return args, invalidReviewArguments(err)
		}
		return args, nil
	case "review_source_refresh_read":
		var in struct {
			ID string `json:"refresh_id"`
		}
		if strictjson.Decode(data, &in) != nil || client.ValidateReviewSourceRefreshID(in.ID) != nil {
			return args, invalidArguments()
		}
		args.id = in.ID
		return args, nil
	case "review_source_policy_assess", "review_source_ci_assess", "review_source_readiness":
		var in struct {
			Number *int64 `json:"pull_number"`
		}
		if strictjson.Decode(data, &in) != nil || in.Number == nil || *in.Number < 1 || *in.Number > 2147483647 {
			return args, invalidArguments()
		}
		args.pullNumber = *in.Number
		return args, nil
	case "review_source_refresh":
		var in struct {
			Key              string `json:"idempotency_key"`
			Revision         *int64 `json:"expected_configuration_revision"`
			Number           *int64 `json:"pull_number"`
			Head             string `json:"head_sha"`
			Wait             *int   `json:"wait_seconds,omitempty"`
			IncludeCI        *bool  `json:"include_ci,omitempty"`
			IncludeReadiness *bool  `json:"include_readiness,omitempty"`
		}
		if strictjson.Decode(data, &in) != nil || in.Revision == nil || in.Number == nil || !validKey(in.Key) {
			return args, invalidArguments()
		}
		args.key = in.Key
		args.reviewSourceWait = 2
		if in.Wait != nil {
			args.reviewSourceWait = *in.Wait
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || fields["wait_seconds"] != nil && in.Wait == nil || fields["include_ci"] != nil && in.IncludeCI == nil || fields["include_readiness"] != nil && in.IncludeReadiness == nil || client.ValidateReviewSourceWait(args.reviewSourceWait) != nil || args.reviewSourceWait > maxToolWaitSeconds {
			return args, invalidArguments()
		}
		args.reviewSource = client.ReviewSourceRequest{ExpectedConfigurationRevision: *in.Revision, PullNumber: *in.Number, HeadSHA: in.Head, IncludeCI: in.IncludeCI != nil && *in.IncludeCI, IncludeReadiness: in.IncludeReadiness != nil && *in.IncludeReadiness}
		if err := invalidHeadSHA(in.Head); err != nil {
			return args, err
		}
		if client.ValidateReviewSourceRequest(args.reviewSource) != nil {
			return args, invalidArguments()
		}
		return args, nil
	case "review_execution_read":
		if strictjson.Decode(data, &struct{}{}) != nil {
			return args, invalidArguments()
		}
		return args, nil
	case "review_operation_read":
		var in struct {
			ID   string `json:"operation_id"`
			Wait *int   `json:"wait_seconds,omitempty"`
		}
		if strictjson.Decode(data, &in) != nil || client.ValidateReviewOperationID(in.ID) != nil {
			return args, invalidArguments()
		}
		args.id = in.ID
		if in.Wait != nil {
			args.reviewSourceWait = *in.Wait
		}
		if client.ValidateReviewOperationWait(args.reviewSourceWait) != nil || args.reviewSourceWait > maxToolWaitSeconds {
			return args, invalidArguments()
		}
		return args, nil
	case "review_request":
		var in struct {
			Key             string `json:"idempotency_key"`
			Revision        *int64 `json:"expected_configuration_revision"`
			Number          *int64 `json:"pull_number"`
			Channel         string `json:"channel"`
			Head            string `json:"head_sha"`
			Wait            *int   `json:"wait_seconds,omitempty"`
			AllowOutOfOrder *bool  `json:"allow_out_of_order,omitempty"`
		}
		if strictjson.Decode(data, &in) != nil || in.Revision == nil || in.Number == nil || !validKey(in.Key) {
			return args, invalidArguments()
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || fields["allow_out_of_order"] != nil && in.AllowOutOfOrder == nil {
			return args, invalidArguments()
		}
		args.reviewSourceWait = 2
		if in.Wait != nil {
			args.reviewSourceWait = *in.Wait
		}
		if client.ValidateReviewOperationWait(args.reviewSourceWait) != nil || args.reviewSourceWait > maxToolWaitSeconds {
			return args, invalidArguments()
		}
		args.key = in.Key
		args.reviewRequest = client.ReviewRequest{ExpectedConfigurationRevision: *in.Revision, PullNumber: *in.Number, Channel: in.Channel, HeadSHA: in.Head, AllowOutOfOrder: in.AllowOutOfOrder != nil && *in.AllowOutOfOrder}
		if err := invalidHeadSHA(in.Head); err != nil {
			return args, err
		}
		if client.ValidateReviewRequest(args.reviewRequest) != nil {
			return args, invalidArguments()
		}
		return args, nil
	case "review_policy_assess", "review_evidence_read", "review_adjudication_read":
		var in struct {
			ID string `json:"evidence_id"`
		}
		if strictjson.Decode(data, &in) != nil {
			return args, invalidArguments()
		}
		args.id = in.ID
	case "review_finding_decide":
		var in struct {
			ID            string `json:"evidence_id"`
			Key           string `json:"idempotency_key"`
			Revision      *int64 `json:"expected_revision"`
			Finding       *int64 `json:"finding"`
			Disposition   string `json:"disposition"`
			Reason        string `json:"reason"`
			RouteRevision *int64 `json:"expected_route_revision,omitempty"`
		}
		if strictjson.Decode(data, &in) != nil || in.Revision == nil || in.Finding == nil {
			return args, invalidArguments()
		}
		args.id, args.key = in.ID, in.Key
		args.reviewDecision = client.ReviewDecisionRequest{ExpectedRevision: *in.Revision, Finding: *in.Finding, Disposition: in.Disposition, Reason: in.Reason}
		if in.RouteRevision != nil {
			args.reviewDecision.ExpectedRouteRevision = *in.RouteRevision
		}
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
		if err := client.ValidateReviewFixRequest(args.reviewFix); err != nil {
			return args, invalidReviewArguments(err)
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
	routeID := map[string]any{"type": "string", "minLength": 45, "maxLength": 45, "pattern": "^review_route_[a-f0-9]{32}$"}
	pull := map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}
	id := map[string]any{"type": "string", "minLength": 48, "maxLength": 48, "pattern": "^review_evidence_[a-f0-9]{32}$"}
	key := map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "Explicit printable ASCII idempotency key; retain the same key and exact request after an unknown outcome. Never generated or automatically retried."}
	revision := map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64 - 1), "description": "Explicit current adjudication revision; zero selects the initial undecided state."}
	finding := map[string]any{"type": "integer", "minimum": 0, "maximum": 99, "description": "Explicit zero-based finding index from the immutable evidence, from 0 through 99."}
	routeRevision := map[string]any{"type": "integer", "minimum": 0, "maximum": int64(math.MaxInt64 - 1), "description": "Current receiving-route revision when rerouting or replacing a routed source decision. Omit or zero only when no active route exists."}
	text := map[string]any{"type": "string", "minLength": 1, "maxLength": 2048, "description": "Exact nonblank UTF-8 text, at most 2048 bytes; Unicode controls except tab, CR and LF are forbidden."}
	schema := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	return []definition{
		{"review_finding_route", "Route one retained native finding to another configured PR in this project. Explicit source revision/key; rerouting also requires the current receiver revision. Local ledger only, no provider call or message.", schema(map[string]any{"evidence_id": id, "idempotency_key": key, "expected_revision": revision, "finding": finding, "target_pull_number": pull, "reason": text, "expected_route_revision": routeRevision}, "evidence_id", "idempotency_key", "expected_revision", "finding", "target_pull_number", "reason")},
		{"review_route_read", "Explicitly read one route, original captured comparison and finding text, including superseded routes. Local provider-free read.", schema(map[string]any{"route_id": routeID}, "route_id")},
		{"review_routes_list", "Read one compact page of active incoming finding routes for a receiving PR, default five. No finding text, provider refresh or automatic paging.", schema(map[string]any{"pull_number": pull, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 5}, "cursor": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}}, "pull_number")},
		{"review_route_decide", "Record the receiving decision for one active route using an explicit key/revision. Receiver decision is independent of source counts; no provider work or automatic retry.", schema(map[string]any{"route_id": routeID, "idempotency_key": key, "expected_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64 - 1)}, "disposition": map[string]any{"type": "string", "enum": []string{"accepted", "rejected"}}, "reason": text}, "route_id", "idempotency_key", "expected_revision", "disposition", "reason")},
		{"review_route_fix", "Record or correct receiving fix proof with an explicit key, revision and expected current SHA. Retains source counts; no provider work or automatic retry.", schema(map[string]any{"route_id": routeID, "idempotency_key": key, "expected_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64 - 1)}, "expected_sha": map[string]any{"type": "string", "maxLength": 40, "pattern": "^(|[a-f0-9]{40})$"}, "fix_sha": map[string]any{"type": "string", "minLength": 40, "maxLength": 40, "pattern": "^[a-f0-9]{40}$", "description": "Full 40-character lowercase hexadecimal fix commit SHA; abbreviations are rejected."}, "proof_note": text}, "route_id", "idempotency_key", "expected_revision", "expected_sha", "fix_sha", "proof_note")},
		{"review_policy_assess", "Read current project requirements and completed PR/channel review progress for the PR identified by captured evidence; later Git changes preserve progress. No provider refresh, current-source verification or merge-readiness verdict.", schema(map[string]any{"evidence_id": id}, "evidence_id")},
		{"review_evidence_read", "Explicitly read full native immutable review evidence and parsed finding text, bounded to 4 MiB. No raw capture, provider refresh or execution.", schema(map[string]any{"evidence_id": id}, "evidence_id")},
		{"review_adjudication_read", "Explicitly read current native finding decisions and fix proof without finding text, provider refresh or execution.", schema(map[string]any{"evidence_id": id}, "evidence_id")},
		{"review_finding_decide", "Record or correct a native finding decision using an explicit key, revision and zero-based index; retain audit history and return a compact receipt. No provider work or automatic retry.", schema(map[string]any{"evidence_id": id, "idempotency_key": key, "expected_revision": revision, "finding": finding, "disposition": map[string]any{"type": "string", "enum": []string{"accepted", "rejected"}}, "reason": text, "expected_route_revision": routeRevision}, "evidence_id", "idempotency_key", "expected_revision", "finding", "disposition", "reason")},
		{"review_finding_fix", "Record or correct accepted-finding fix proof with an explicit key, revision, zero-based index and expected current fix SHA; retain audit history and return a compact receipt. No provider work or automatic retry.", schema(map[string]any{"evidence_id": id, "idempotency_key": key, "expected_revision": revision, "finding": finding, "expected_sha": map[string]any{"type": "string", "maxLength": 40, "pattern": "^(|[a-f0-9]{40})$", "description": "Required even when empty; empty asserts that no fix SHA is currently recorded."}, "fix_sha": map[string]any{"type": "string", "minLength": 40, "maxLength": 40, "pattern": "^[a-f0-9]{40}$", "description": "Full 40-character lowercase hexadecimal fix commit SHA; abbreviations are rejected."}, "proof_note": text}, "evidence_id", "idempotency_key", "expected_revision", "finding", "expected_sha", "fix_sha", "proof_note")},
		{"review_source_finalize", "Finalize eligible native source counts and atomically queue the GitHub summary; no separate publish call or wait for fixes. Return a compact receipt without waiting for provider I/O or requesting another review.", schema(map[string]any{"evidence_id": id, "idempotency_key": key, "expected_revision": revision}, "evidence_id", "idempotency_key", "expected_revision")},
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
		{"review_source_refresh", "Collect source and optionally required CI and readiness (review-thread and merge-state) facts for one configured PR and expected head. Wait up to wait_seconds for a completed or held result; pending work or a zero wait returns a compact receipt. No review post, allowance query or execution. Retain the exact idempotency key and arguments after an unknown outcome; no automatic retry or polling.", schema(map[string]any{
			"idempotency_key":                 map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"expected_configuration_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64)},
			"pull_number":                     map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647},
			"head_sha":                        map[string]any{"type": "string", "minLength": 40, "maxLength": 40, "pattern": "^[a-f0-9]{40}$"},
			"include_readiness":               map[string]any{"type": "boolean", "default": false, "description": "Also collect selected-base required CI, review-thread resolution and merge-state facts for local readiness assessment; optional workflow health is not assessed."},
			"include_ci":                      map[string]any{"type": "boolean", "default": false, "description": "Collect required checks for the selected base branch. not_required means no required checks; optional workflows may still run or fail and are not assessed. Ordinary reads remain provider-free."},
			"wait_seconds":                    map[string]any{"type": "integer", "minimum": 0, "maximum": maxToolWaitSeconds, "default": 2, "description": "Wait for terminal facts up to this many seconds; zero requests an immediate receipt. Extends this source-refresh call budget only."},
		}, "idempotency_key", "expected_configuration_revision", "pull_number", "head_sha")},
		{"review_source_refresh_read", "Read one explicit retained source-refresh operation. Local provider-free read; no automatic polling or source refresh.", schema(map[string]any{"refresh_id": map[string]any{"type": "string", "minLength": 47, "maxLength": 47, "pattern": "^review_refresh_[a-f0-9]{32}$"}}, "refresh_id")},
		{"review_source_policy_assess", "Read current project review policy as of a retained source observation for one PR. Local provider-free read; no current remote-source verification, CI or merge-readiness verdict.", schema(map[string]any{"pull_number": map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}}, "pull_number")},
		{"review_source_readiness", "Read one retained as-of readiness assessment using current local policy. Required CI covers the selected base branch, not optional workflow health; GitHub merge-state gates remain separate. Provider-free, compact selected-PR read; no refresh, polling or merge authorization.", schema(map[string]any{"pull_number": map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}}, "pull_number")},
		{"review_source_ci_assess", "Read retained required-CI assessment for the selected base branch of one PR. not_required means no required checks; optional workflows may still run or fail and are not assessed. Local provider-free read; reports observation age and applicability, never merge readiness.", schema(map[string]any{"pull_number": map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}}, "pull_number")},
		{"review_execution_read", "Read stored project review execution configuration. No provider calls, execution, readiness verdict or automatic polling.", schema(map[string]any{})},
		operationStatusDefinition(),
		{"review_request", "Enqueue the selected external provider review, which can consume included allowance asynchronously. Waits briefly for a terminal result; pending work returns a compact queued receipt, which does not mean started or completed. Retain the exact idempotency key and arguments for uncertain-outcome recovery; no automatic retry, generated key or operation polling.", schema(map[string]any{
			"idempotency_key":                 map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"expected_configuration_revision": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(math.MaxInt64)},
			"pull_number":                     map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647},
			"channel":                         map[string]any{"type": "string", "enum": []string{"cli", "hosted"}},
			"head_sha":                        map[string]any{"type": "string", "pattern": "^[a-f0-9]{40}$"},
			"allow_out_of_order":              map[string]any{"type": "boolean", "default": false, "description": "Explicitly bypass queue order for this exact target only; comparison and all target admission guards still apply."},
			"wait_seconds":                    map[string]any{"type": "integer", "minimum": 0, "maximum": maxToolWaitSeconds, "default": 2, "description": "Wait quietly for a terminal result; zero detaches immediately. Changing this preference never starts another review."},
		}, "idempotency_key", "expected_configuration_revision", "pull_number", "channel", "head_sha")},
	}
}
