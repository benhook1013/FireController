package cli

import "io"

type operationHelpResponse struct {
	Usage         string            `json:"usage"`
	Required      []string          `json:"required"`
	Optional      []string          `json:"optional"`
	Semantics     []string          `json:"semantics"`
	Example       string            `json:"example"`
	FileExample   map[string]any    `json:"file_example,omitempty"`
	FileFields    map[string]string `json:"file_fields,omitempty"`
	Next          string            `json:"next,omitempty"`
	Documentation string            `json:"documentation"`
}

func emitOperationHelp(operation string, stdout, stderr io.Writer) int {
	if help, ok := operationHelp(operation); ok {
		return emit(stdout, stderr, help)
	}
	if help, ok := ownerManagementHelp(operation); ok {
		return emit(stdout, stderr, help)
	}
	if operation == "credentials help" || operation == "workers help" || operation == "reviews help" {
		return emit(stdout, stderr, managementLocalHelp(operation))
	}
	usage := "<bound launcher> reviews operations [--pr NUMBER] [--channel cli|hosted] [--all] [--limit N] [--cursor CURSOR]"
	semantics := "Read one compact newest-first page of retained project operations. Default five pending; --all includes held and terminal history. Reuse the same filters with a cursor. No provider reads, automatic pagination, execution or retry."
	documentation := "docs/review-operations.md"
	if operation == "operation-control" {
		documentation = "docs/retained-review-quota-controls.md"
		usage = "<bound launcher> reviews operation-control OPERATION_ID --action pause|resume|cancel --expect-revision N --key KEY"
		semantics = "Control only a queued, undispatched operation using its current revision and explicit key. Pause retains the retry deadline; resume permits the same request to recheck when due. Cancel withdraws intent, never an issued provider request. Preserve exact request and key after unknown outcome. No changes to grants, taper, findings or project pause."
	}
	if operation == "ci-wait" {
		documentation = "docs/ci-wait.md"
		usage = "<bound launcher> reviews ci-wait --target PR:HEAD[:BASE] [--after-observation PR:KEY] [--failure-workflow PR:ID] [--wait-seconds 0-900 (default 300)]"
		semantics = "Read-only CI against the default merge branch. The outer state reports the wake condition; all_targets_passed requires every target. An unchanged passed or failed verdict stays quiet; pending checks alone do not wake this terminal wait. No enrollment or review request."
	}
	if operation == "operation" {
		usage = "<bound launcher> reviews operation OPERATION_ID [--wait-seconds N] [--diagnostics]"
		semantics = "Read compact retained state and original Hosted observer diagnostics; --diagnostics selects the full retained operation. Wait 0-900 seconds, default zero. A posted trigger or successful observer check proves neither provider activity nor completion. Detaching never cancels or retries the review."
	}
	if operation == "start" {
		usage = "<bound launcher> reviews start --channel cli|hosted --key KEY [--expect-pr NUMBER] [--expected-configuration-revision N] [--wait-seconds N]"
		semantics = "Explicitly select at most one review. Expected PR is an assertion, never a forced target. Wait 0-900 seconds, default two. Preserve the exact key and arguments after uncertain output. Terminal holds do not resume automatically."
	}
	if operation == "run-read" {
		usage = "<bound launcher> reviews run-read --selection-id ID [--wait-seconds N]"
		semantics = "Read one retained selection and its linked operation. Wait 0-900 seconds, default zero. No new selection, provider work or automatic retry. Preserve original identities."
	}
	return emit(stdout, stderr, struct {
		Usage         string `json:"usage"`
		Semantics     string `json:"semantics"`
		Documentation string `json:"documentation"`
	}{usage, semantics, documentation})
}
