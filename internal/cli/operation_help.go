package cli

import "io"

func emitOperationHelp(operation string, stdout, stderr io.Writer) int {
	usage := "<bound launcher> reviews operations [--pr NUMBER] [--channel cli|hosted] [--all] [--limit N] [--cursor CURSOR]"
	semantics := "Read one compact newest-first page of retained project operations. Default five pending; --all includes held and terminal history. Reuse the same filters with a cursor. No provider reads, automatic pagination, execution or retry."
	if operation == "operation" {
		usage = "<bound launcher> reviews operation OPERATION_ID [--wait-seconds N] [--diagnostics]"
		semantics = "Read compact retained state and original Hosted observer diagnostics; --diagnostics selects the full retained operation. Wait 0-900 seconds, default zero. A posted trigger or successful observer check proves neither provider activity nor completion. Detaching never cancels or retries the review."
	}
	return emit(stdout, stderr, struct {
		Usage         string `json:"usage"`
		Semantics     string `json:"semantics"`
		Documentation string `json:"documentation"`
	}{usage, semantics, "docs/review-operations.md"})
}
