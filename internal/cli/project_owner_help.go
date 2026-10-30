package cli

func ownerManagementHelp(operation string) (operationHelpResponse, bool) {
	out := operationHelpResponse{Usage: "<bound launcher> " + operation, Required: []string{}, Optional: []string{}, Semantics: []string{"Uses only the selected project and acting worker. Management capability never grants permission to merge or request unauthorized provider work.", "For mutations, retain the exact request, binding and explicit key after an unknown outcome; no automatic retry."}, Documentation: "docs/review-execution.md"}
	switch operation {
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
		out.FileExample = map[string]any{"expected_revision": 1, "required_channels": []string{"cli", "hosted"}, "max_review_files": 0}
		out.Semantics = append(out.Semantics, "Project manager; full policy replacement. max_review_files zero disables the optional project ceiling; provider restrictions remain independent. Read reviews policy for current revision. No grants/taper changes or review request.")
	case "reviews execution-configure":
		out.Usage += " --file CONFIG_JSON --key KEY"
		out.Required = []string{"--file (owner-private absolute JSON path)", "--key KEY"}
		out.FileExample = map[string]any{"expected_revision": 1, "capability_id": "configured-capability", "mode": "stacked", "queue": []int{42}, "channels": []string{"cli", "hosted"}, "paused": false}
		out.Semantics = append(out.Semantics, "Project manager; full execution replacement. Read reviews execution and preserve unrelated fields. queue may be an explicit empty array. Preserve the current capability_id; capability installation and initial binding are instance-owned. Existing limits/history remain; this requests no review.")
	case "reviews attempt-settle":
		out.Usage += " ATTEMPT_ID --file SETTLEMENT_JSON --key KEY"
		out.Required = []string{"ATTEMPT_ID", "--file (owner-private absolute JSON path)", "--key KEY"}
		out.FileExample = map[string]any{"expected_revision": 4, "expected_repository_id": 123, "expected_head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "expected_external_id": "retained-external-id", "external_action_stopped": true, "reason": "External action independently verified stopped.", "evidence_reference": "private-evidence-reference"}
		out.Semantics = append(out.Semantics, "Project manager; only eligible original uncertain attempts. Independently verify external termination before asserting external_action_stopped. Exact revision/repository/head/external ID guards remain. This does not cancel a provider, fabricate review evidence or award completion credit.")
		out.Documentation = "docs/hosted-settlement.md"
	case "workers create":
		out.Usage += " --name TEXT --key KEY"
		out.Required = []string{"--name TEXT (1-200 UTF-8 bytes, no surrounding whitespace)", "--key KEY"}
		out.Semantics = append(out.Semantics, "Project writer; create one named worker. Does not change the acting worker, create credentials or start an agent.")
		out.Documentation = "docs/local-api.md"
	case "credentials grant":
		out.Usage += " (--token-sha256 HASH | --credential-file PATH) --permission read [--permission write|manage] --key KEY"
		out.Required = []string{"exactly one --token-sha256 (64 lowercase hex) or --credential-file (existing private absolute path)", "--permission (repeat for unique read/write/manage)", "--key KEY"}
		out.Semantics = append(out.Semantics, "Project manager; create one distinct non-admin project credential grant. Only a SHA-256 digest is sent. An explicitly selected owner-private credential file is hashed locally; no raw token is accepted as an argument or printed. manage implies read/write. Existing credentials use update instead.")
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
