package cli

func operationHelp(operation string) (operationHelpResponse, bool) {
	recovery := "Use an explicit --key (1-128 printable ASCII bytes without surrounding whitespace). After uncertain output retry the same key, exact request and binding; never invent a replacement key."
	out := operationHelpResponse{Required: []string{}, Optional: []string{}}
	switch operation {
	case "reviews scheduling":
		out.Usage = "<bound launcher> reviews scheduling [--pr N] | configure --automatic-hosted true|false [--automatic-cli true|false] --expect-revision N --key KEY | pause --pr N --expect-revision N --key KEY [--reason TEXT] | resume --pr N --expect-revision N --key KEY"
		out.Optional = []string{"--pr N (read one PR pause; omission reads project settings and queued paused PRs)", "configure --automatic-hosted true|false (project manager; authorizes future automatic Hosted requests)", "pause|resume --pr N (writer; holds new/undispatched reviews without changing rounds)", "--reason TEXT (optional with pause, at most 256 UTF-8 bytes)"}
		out.Optional = append(out.Optional, "reviews scheduling read --help", "reviews scheduling configure --help", "reviews scheduling pause --help", "reviews scheduling resume --help")
		out.Semantics = []string{"Automation defaults off. Enabling requires this connection's active bound worker and current project-management authority; CLI automation is separately opt-in via --automatic-cli; omission preserves its current value. Existing grants and taper remain unchanged.", "Pause can precede any intent. Existing provider review collection continues, and separate operation pauses remain in effect. Read the current revision; mutations require an explicit key and preserve original-key recovery.", "No live provider request is needed to read or change a PR pause. Enabling project automation authorizes later Hosted or CLI selection under all existing comparison, obligation, round and provider guards."}
		out.Documentation = "docs/review-scheduling.md"
	case "reviews scheduling read":
		out.Usage = "<bound launcher> reviews scheduling read [--pr N]"
		out.Optional = []string{"--pr N (read one PR pause; omission reads project settings and queued paused PRs)"}
		out.Semantics = []string{"Returns stored scheduling state and its independent revision. Use the project revision for configure, or the selected PR revision for pause/resume. No provider request or writes."}
		out.Example = "<bound launcher> reviews scheduling read --pr 42"
		out.Documentation = "docs/review-scheduling.md"
	case "reviews scheduling configure":
		out.Usage = "<bound launcher> reviews scheduling configure --automatic-hosted true|false [--automatic-cli true|false] --expect-revision N --key KEY"
		out.Required = []string{"--automatic-hosted true|false", "--expect-revision N (current project scheduling revision, including 0)", "--key KEY"}
		out.Semantics = []string{"Requires project-management authority. Read reviews scheduling for the current project scheduling revision. Enabling authorizes future automatic Hosted or CLI requests through this connection's active bound worker under the existing review guards. Both default off; CLI omission preserves the current value. Automation does not adjudicate findings or increase grants.", recovery}
		out.Optional = []string{"--automatic-cli true|false (separate opt-in; omission preserves the current CLI setting)"}
		out.Example = "<bound launcher> reviews scheduling configure --automatic-hosted true --expect-revision 0 --key automatic-hosted-1"
		out.Documentation = "docs/review-scheduling.md"
	case "reviews scheduling pause", "reviews scheduling resume":
		out.Usage = "<bound launcher> " + operation + " --pr N --expect-revision N --key KEY"
		out.Required = []string{"--pr N", "--expect-revision N (current PR scheduling revision, including 0)", "--key KEY"}
		out.Semantics = []string{"Requires project write access. Read reviews scheduling read --pr N for this PR's scheduling revision. Changes new/undispatched review scheduling in both channels; existing provider collection continues. Does not change rounds, channel closure or separate operation pauses.", recovery}
		if operation == "reviews scheduling pause" {
			out.Usage += " [--reason TEXT]"
			out.Optional = []string{"--reason TEXT (optional, at most 256 UTF-8 bytes)"}
			out.Semantics = append(out.Semantics, "May pause before the first review intent. No live provider request is needed to pause a PR.")
		} else {
			out.Semantics = append(out.Semantics, "Clears the PR pause and its reason. Accepts no --reason flag; does not resume separately paused operations or reopen closed channels.")
		}
		out.Example = "<bound launcher> " + operation + " --pr 42 --expect-revision 0 --key scheduling-42-1"
		out.Documentation = "docs/review-scheduling.md"
	default:
		return out, false
	}
	return out, true
}
