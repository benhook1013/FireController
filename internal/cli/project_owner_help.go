package cli

func ownerManagementHelp(operation string) (operationHelpResponse, bool) {
	out := operationHelpResponse{Usage: "<bound launcher> " + operation, Required: []string{}, Optional: []string{}, Semantics: []string{"Uses only the selected project and acting worker. Management capability never grants permission to merge or request unauthorized provider work.", "For mutations, retain the exact request, binding and explicit key after an unknown outcome; no automatic retry."}, Documentation: "docs/review-execution.md"}
	switch operation {
	case "reviews operation-control":
		out.Usage += " OPERATION_ID --action pause|resume|cancel --expect-revision N --key KEY"
		out.Required = []string{"OPERATION_ID", "--action pause|resume|cancel", "--expect-revision N", "--key KEY"}
		out.Example = "<bound launcher> reviews operation-control review_operation_0123456789abcdef0123456789abcdef --action pause --expect-revision 3 --key pause-request"
		out.Semantics = []string{"Project writer: control only a queued, undispatched operation. Read reviews operation for current revision and wait metadata. Pause retains the automatic retry deadline; resume allows the same operation to retry when due. Cancel withdraws only this undispatched intent, never a provider request.", "No change to grants, taper, findings or project pause. Preserve exact request and key after unknown outcome. Posting, active or uncertain provider work cannot be controlled here. Pause/resume cannot edit head or out-of-order intent. An authorized comparison/order change requires successful queued-only cancellation, then a fresh current-head/configuration request/key after any known cooldown; its timer is not transferred. The out-of-order flag bypasses predecessor dependencies, not earlier queued review priority. A held target_changed is terminal; qualify the intended current open head and use a fresh authorized request/key, respecting known cooldowns."}

	case "credentials help":
		out.Optional = []string{"credentials grant --help", "credentials update --help", "credentials revoke --help"}
		out.Semantics = []string{"Project-manager access changes use this bound project. Grant accepts only a digest or local private credential-file reference; update preserves an existing binding under an exact permission-set guard. No raw token arguments or instance administration."}
		out.Documentation = "docs/local-api.md"
	case "reviews policy":
		out.Documentation = "docs/review-controls.md"
		out.Semantics = []string{"Read the current configured project review requirements and policy revision. No provider work or source refresh."}
	case "reviews policy-configure":
		out.Documentation = "docs/review-controls.md"
		out.Usage += " --file POLICY_JSON --key KEY"
		out.Required = []string{"--file (owner-private absolute JSON path)", "--key KEY"}
		out.FileExample = map[string]any{"expected_revision": 0, "required_channels": []string{"cli", "hosted"}, "max_review_files": 0}
		out.Semantics = append(out.Semantics, "Project manager; full policy replacement. max_review_files zero disables the optional project ceiling; provider restrictions remain independent. For initial configuration use expected_revision zero; reviews policy returns reviews_policy_unconfigured until configured. For replacement, read reviews policy for the current revision. Optional round_defaults provides hosted/cli objects with explicit minimum/maximum for newly initialized PR controls only. Omission uses product defaults; existing controls and taper are unchanged. See docs/review-round-controls.md. No review request.")
	case "reviews execution-configure":
		out.Usage += " --file CONFIG_JSON --key KEY"
		out.Required = []string{"--file (owner-private absolute JSON path)", "--key KEY"}
		out.FileExample = map[string]any{"expected_revision": 1, "capability_id": "configured-capability", "mode": "stacked", "queue": []int{42}, "channels": []string{"cli", "hosted"}, "paused": false}
		out.Semantics = append(out.Semantics, "Project manager; full execution replacement. Read reviews execution and preserve unrelated fields. queue may be an explicit empty array. Preserve the current capability_id; capability installation and initial binding are instance-owned. Existing limits/history remain; this requests no review.")
	case "reviews attempt-settle":
		out.Usage += " ATTEMPT_ID --file SETTLEMENT_JSON --key KEY"
		out.Required = []string{"ATTEMPT_ID", "--file (owner-private absolute JSON path)", "--key KEY"}
		out.FileExample = map[string]any{"expected_revision": 4, "expected_repository_id": 123, "expected_head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "expected_external_id": "123456789", "external_action_stopped": false, "reason": "Explicitly abandon this confirmed issued pending Hosted request without claiming it stopped.", "evidence_reference": "private-operator-decision-reference"}
		out.Semantics = append(out.Semantics, "Project manager; eligible issued Hosted attempts and retained partial CLI results only. Read reviews operation OPERATION_ID for attempt_id and settlement_target; use its attempt_revision, not the operation revision, for expected_revision. Those guard facts do not prove eligibility or termination. external_action_stopped:true asserts independently verified external termination; false explicitly abandons only a confirmed issued pending Hosted request without claiming termination. The server checks eligibility. Local settlement does not stop a provider, automatically retry or award completion credit. Preserve original history and trigger identity for late-result pickup.")
		out.Documentation = "docs/hosted-settlement.md"
	case "workers retirement", "workers retire":
		out.Usage += " WORKER_ID"
		out.Required = []string{"WORKER_ID"}
		out.Semantics = []string{"Retirement is one-way and preserves historical worker references. Unfinished assigned jobs, pending direct/assigned-job notes and unacknowledged messages block retirement. Reassign or resolve work and explicitly acknowledge messages first; nothing is moved or acknowledged automatically."}
		if operation == "workers retire" {
			out.Usage += " --key KEY"
			out.Required = append(out.Required, "--key KEY")
			out.Semantics = append(out.Semantics, "Project writer; atomic blocker checks run again at retirement. Retain the exact request/key after an uncertain result; no automatic retry.")
		} else {
			out.Semantics = append(out.Semantics, "Read-only preview reports total blocker counts and up to 20 IDs per category. It is not a reservation or a guarantee a later retirement will succeed.")
		}
		out.Documentation = "docs/worker-retirement.md"
	case "workers create":
		out.Usage += " --name TEXT --key KEY"
		out.Required = []string{"--name TEXT (1-200 UTF-8 bytes, no surrounding whitespace)", "--key KEY"}
		out.Semantics = append(out.Semantics, "Project writer; create one named worker. Does not change the acting worker, create credentials or start an agent.")
		out.Documentation = "docs/local-api.md"
	case "credentials grant":
		out.Usage += " (--token-sha256 HASH | --credential-file PATH) --permission read [--permission write|manage] --key KEY"
		out.Required = []string{"exactly one --token-sha256 (64 lowercase hex) or --credential-file (existing private absolute path)", "--permission (repeat for unique read/write/manage)", "--key KEY"}
		out.Semantics = append(out.Semantics, "Project manager; create one distinct non-admin project credential grant. Only a SHA-256 digest is sent. An explicitly selected owner-private credential file is hashed locally; use a high-entropy token of 32-256 printable nonspace ASCII bytes after surrounding whitespace is trimmed. No raw token is accepted as an argument or printed. manage implies read/write. Existing credentials use update instead.")
		out.Documentation = "docs/local-api.md"
	case "credentials revoke":
		out.Usage += " CREDENTIAL_ID --key KEY"
		out.Required = []string{"CREDENTIAL_ID", "--key KEY"}
		out.Semantics = append(out.Semantics, "Project manager; revoke only this project's grant. Other-project grants remain. No credential secret is read or printed.")
		out.Documentation = "docs/local-api.md"
	case "credentials update":
		out.Usage += " CREDENTIAL_ID --file PERMISSIONS_JSON --key KEY"
		out.Required = []string{"CREDENTIAL_ID", "--file (owner-private absolute JSON path)", "--key KEY"}
		out.FileExample = map[string]any{"expected_permissions": []string{"read", "write"}, "permissions": []string{"read", "write", "manage"}}
		out.Semantics = append(out.Semantics, "Project manager; exact permission-set guard on one active non-admin credential's grant. Preserves credential, token, principal and worker binding. Changes no other project. manage implies read/write.")
		out.Documentation = "docs/local-api.md"
	default:
		return operationHelpResponse{}, false
	}
	return out, true
}
