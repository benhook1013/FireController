package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"math"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type ownerManagementInvocation struct {
	name, key, id string
	policy        client.ConfigureReviewPolicyRequest
	execution     client.ConfigureReviewExecutionRequest
	settlement    client.SettleReviewAttemptRequest
	worker        client.CreateWorkerRequest
	grant         client.GrantProjectCredentialRequest
	update        client.UpdateProjectCredentialRequest
	control       client.ReviewOperationControl
}

func ownerManagementTool(name string) bool {
	switch name {
	case "review_operation_control", "review_policy_read", "review_policy_configure", "review_execution_configure", "review_attempt_settle", "worker_create", "project_credential_grant", "project_credential_revoke", "project_credential_update":
		return true
	}
	return false
}
func ownerManagementWrite(name string) bool {
	return ownerManagementTool(name) && name != "review_policy_read"
}
func ownerManagementDestructive(name string) bool {
	return ownerManagementWrite(name) && name != "worker_create" && name != "project_credential_grant"
}
func ownerDecode(raw json.RawMessage, out any, required ...string) bool {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if len(raw) > 32<<10 || strictjson.Decode(raw, out) != nil {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	for _, v := range fields {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return false
		}
	}
	for _, k := range required {
		if _, ok := fields[k]; !ok {
			return false
		}
	}
	return true
}
func ownerManagementArguments(name string, raw json.RawMessage) (arguments, error) {
	var args arguments
	in := &ownerManagementInvocation{name: name}
	if name == "review_policy_read" {
		if !ownerDecode(raw, &struct{}{}) {
			return args, invalidArguments()
		}
		args.ownerManagement = in
		return args, nil
	}
	if len(raw) > 32<<10 || strictjson.Validate(raw) != nil {
		return args, invalidArguments()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return args, invalidArguments()
	}
	if json.Unmarshal(fields["idempotency_key"], &in.key) != nil || !validKey(in.key) {
		return args, invalidArguments()
	}
	delete(fields, "idempotency_key")
	idField := ""
	switch name {
	case "review_operation_control":
		idField = "operation_id"
	case "review_attempt_settle":
		idField = "attempt_id"
	case "project_credential_revoke", "project_credential_update":
		idField = "credential_id"
	}
	if idField != "" {
		if json.Unmarshal(fields[idField], &in.id) != nil || !validID(in.id) {
			return args, invalidArguments()
		}
		delete(fields, idField)
	}
	body, _ := json.Marshal(fields)
	valid := false
	switch name {
	case "review_operation_control":
		valid = ownerDecode(body, &in.control, "expected_revision", "action") && client.ValidateReviewOperationControl(in.id, in.control) == nil
	case "review_policy_configure":
		valid = ownerDecode(body, &in.policy, "expected_revision", "required_channels", "max_review_files") && client.ValidateConfigureReviewPolicy(in.policy) == nil
	case "review_execution_configure":
		valid = ownerDecode(body, &in.execution, "expected_revision", "capability_id", "mode", "queue", "channels", "paused") && client.ValidateConfigureReviewExecution(in.execution) == nil
	case "review_attempt_settle":
		valid = ownerDecode(body, &in.settlement, "expected_revision", "expected_repository_id", "expected_head_sha", "expected_external_id", "external_action_stopped", "reason", "evidence_reference") && client.ValidateSettleReviewAttempt(in.id, in.settlement) == nil
	case "worker_create":
		valid = ownerDecode(body, &in.worker, "name") && client.ValidateCreateWorker(in.worker) == nil
	case "project_credential_grant":
		valid = ownerDecode(body, &in.grant, "token_sha256", "permissions") && client.ValidateGrantProjectCredential(in.grant) == nil
	case "project_credential_revoke":
		valid = ownerDecode(body, &struct{}{})
	case "project_credential_update":
		valid = ownerDecode(body, &in.update, "expected_permissions", "permissions") && client.ValidateUpdateProjectCredential(in.id, in.update) == nil
	}
	if !valid {
		return args, invalidArguments()
	}
	args.ownerManagement = in
	return args, nil
}
func executeOwnerManagement(ctx context.Context, c *client.Client, in *ownerManagementInvocation) (any, error) {
	switch in.name {
	case "review_operation_control":
		return c.ControlReviewOperation(ctx, in.key, in.id, in.control)
	case "review_policy_read":
		return c.ReviewPolicy(ctx)
	case "review_policy_configure":
		return c.ConfigureReviewPolicy(ctx, in.key, in.policy)
	case "review_execution_configure":
		return c.ConfigureReviewExecution(ctx, in.key, in.execution)
	case "review_attempt_settle":
		return c.SettleReviewAttempt(ctx, in.key, in.id, in.settlement)
	case "worker_create":
		return c.CreateWorker(ctx, in.key, in.worker)
	case "project_credential_grant":
		return c.GrantProjectCredential(ctx, in.key, in.grant)
	case "project_credential_revoke":
		return c.RevokeProjectCredential(ctx, in.key, in.id)
	case "project_credential_update":
		return c.UpdateProjectCredential(ctx, in.key, in.id, in.update)
	}
	return nil, invalidArguments()
}
func ownerManagementDefinitions() []definition {
	text := func(max int) map[string]any { return map[string]any{"type": "string", "maxLength": max} }
	integer := func(min, max int64) map[string]any {
		return map[string]any{"type": "integer", "minimum": min, "maximum": max}
	}
	strEnum := func(values ...string) map[string]any { return map[string]any{"type": "string", "enum": values} }
	channels := map[string]any{"type": "array", "minItems": 1, "maxItems": 2, "uniqueItems": true, "items": strEnum("cli", "hosted")}
	permissions := map[string]any{"type": "array", "minItems": 1, "maxItems": 3, "uniqueItems": true, "items": strEnum("read", "write", "manage")}
	key := text(128)
	key["minLength"] = 1
	revision := integer(0, math.MaxInt64-1)
	sha := text(40)
	sha["pattern"] = "^[a-f0-9]{40}$"
	var defs []definition
	add := func(name, desc string, props map[string]any, required ...string) {
		defs = append(defs, definition{name, desc, map[string]any{"type": "object", "additionalProperties": false, "properties": props, "required": required}})
	}
	operationID := text(49)
	operationID["pattern"] = "^review_operation_[a-f0-9]{32}$"
	add("review_operation_control", "Project writer: pause, resume or cancel only a queued undispatched operation using its current revision and an explicit key. Pause preserves the retry deadline; resume retries this same operation when due. Cancel withdraws intent, never an issued provider request. Posting/active/uncertain work refuses. No changes to grants, taper, findings or project pause.", map[string]any{"operation_id": operationID, "expected_revision": integer(1, math.MaxInt64-1), "action": strEnum("pause", "resume", "cancel"), "idempotency_key": key}, "operation_id", "expected_revision", "action", "idempotency_key")
	add("review_policy_read", "Read this project's current configured review requirements, policy revision and attribution. No source refresh or provider action.", map[string]any{})
	add("review_policy_configure", "Project manager: replace current review requirements with exact expected_revision and explicit key. max_review_files zero means no project ceiling; provider limits remain separate. Does not change grants, taper or request reviews.", map[string]any{"idempotency_key": key, "expected_revision": revision, "required_channels": channels, "max_review_files": integer(0, 100000)}, "idempotency_key", "expected_revision", "required_channels", "max_review_files")
	add("review_execution_configure", "Project manager: full guarded replacement of existing capability selection, mode, ordered scheduling queue, channels and pause. Read current configuration and preserve fields outside the intended change. Preserve the current capability_id; capability installation and initial binding are instance-owned. Existing controls/history remain; this requests no review.", map[string]any{"idempotency_key": key, "expected_revision": revision, "capability_id": text(128), "mode": strEnum("independent", "stacked"), "queue": map[string]any{"type": "array", "maxItems": 100, "uniqueItems": true, "items": integer(1, 2147483647)}, "channels": channels, "paused": map[string]any{"type": "boolean"}}, "idempotency_key", "expected_revision", "capability_id", "mode", "queue", "channels", "paused")
	add("review_attempt_settle", "Project manager: settle eligible attempts with a verified external stop (external_action_stopped:true), or explicitly abandon a confirmed issued pending Hosted request without claiming it stopped (false). The server checks eligibility. Read review_operation_read with its default compact view for attempt_id and settlement_target; use attempt_revision as expected_revision. These guard facts prove neither eligibility nor termination. Preserve exact guards and explicit reason/evidence. Local settlement does not stop a provider, invent evidence, automatically retry, award completion credit or request another review. Preserve original history and trigger identity for late-result pickup.", map[string]any{"idempotency_key": key, "attempt_id": text(128), "expected_revision": integer(1, math.MaxInt64-1), "expected_repository_id": integer(1, math.MaxInt64), "expected_head_sha": sha, "expected_external_id": text(128), "external_action_stopped": map[string]any{"type": "boolean", "description": "Required explicit choice: true asserts an independently verified external stop; false requests operator abandonment only for a confirmed issued pending Hosted request and makes no stop claim."}, "reason": text(1024), "evidence_reference": text(1024)}, "idempotency_key", "attempt_id", "expected_revision", "expected_repository_id", "expected_head_sha", "expected_external_id", "external_action_stopped", "reason", "evidence_reference")
	add("worker_create", "Project writer: create one named worker in this bound project. Returns its ID; does not switch the acting worker, create credentials or start an agent.", map[string]any{"idempotency_key": key, "name": text(200)}, "idempotency_key", "name")
	digest := text(64)
	digest["pattern"] = "^[a-f0-9]{64}$"
	add("project_credential_grant", "Project manager: grant this project to a distinct non-admin credential using only its lowercase SHA-256 digest. Raw tokens never enter tool arguments/output; generate/store credentials privately through onboarding. manage implies read/write. Explicit key; no other-project or instance authority.", map[string]any{"idempotency_key": key, "token_sha256": digest, "permissions": permissions}, "idempotency_key", "token_sha256", "permissions")
	add("project_credential_revoke", "Project manager: revoke a known credential's access to this project only. Preserves its other-project grants; no token or acting identity change. Explicit key, original ID retained after unknown outcome.", map[string]any{"idempotency_key": key, "credential_id": text(128)}, "idempotency_key", "credential_id")
	add("project_credential_update", "Project manager: replace one existing non-admin credential's permissions in this project under an exact expected_permissions guard. Preserves credential, principal, token and worker binding; changes no other project. manage implies read/write. No hidden role or identity switching.", map[string]any{"idempotency_key": key, "credential_id": text(128), "expected_permissions": permissions, "permissions": permissions}, "idempotency_key", "credential_id", "expected_permissions", "permissions")
	return defs
}
