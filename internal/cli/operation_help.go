package cli

import "io"

func emitOperationHelp(operation string, stdout, stderr io.Writer) int {
	usage := "<bound launcher> reviews operations [--pr NUMBER] [--channel cli|hosted] [--all] [--limit N] [--cursor CURSOR]"
	semantics := "Read one compact newest-first page of retained project operations. Default five pending; --all includes held and terminal history. Reuse the same filters with a cursor. No provider reads, automatic pagination, execution or retry."
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
	}{usage, semantics, "docs/review-operations.md"})
}
