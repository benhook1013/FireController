package cli

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

func validHelpOptions(o options) bool {
	return (!o.hasProfile || validSetupText(o.profile)) && (!o.hasProfilesFile || validSetupText(o.profilesFile))
}

func argumentHelpAction(call invocation) string {
	operation := call.command + " " + call.subcommand
	if _, ok := operationHelp(operation); ok {
		return "Run the bound launcher with " + operation + " --help for required fields and examples."
	}
	return "Run the bound launcher with help to discover supported commands and matching-release documentation."
}

// This selected local reference supplements the real parsers. Only the exact
// operation-help grammar reaches it, before any profile, file or service work.
func operationHelp(operation string) (operationHelpResponse, bool) {
	guard := "Use the current resource --expect-revision. Omitted fields stay unchanged; at least one edit is required."
	recovery := "Use an explicit --key (1-128 printable ASCII bytes without surrounding whitespace). After uncertain output retry the same key, exact request and binding; never invent a replacement key."
	out := operationHelpResponse{Required: []string{}, Optional: []string{}}
	switch operation {
	case "jobs update":
		out.Usage = "<bound launcher> jobs update JOB_ID --expect-revision N --key KEY [changes]"
		out.Required = []string{"JOB_ID", "--expect-revision N", "--key KEY", "at least one optional edit"}
		out.Optional = []string{"--title TEXT", "--brief TEXT", "--worker WORKER_ID", "--status active|blocked|parked|completed", "--progress TEXT", "--next TEXT", "--blocker TEXT", "--evidence REF (repeat up to 10)"}
		out.Semantics = []string{guard, "Empty --brief, --worker, --progress, --next or --blocker clears that field; title cannot be empty. A sole --evidence '' clears evidence; otherwise supplied evidence replaces the complete reference list. History is automatic.", recovery}
		out.Example = "<bound launcher> jobs update JOB_ID --expect-revision 3 --key job-next-1 --next 'Run focused validation'"
		out.Documentation = "docs/job-state.md"
	case "notes add", "notes update":
		out.Optional = []string{"--worker WORKER_ID | --clear-worker", "--job JOB_ID", "--phase TEXT", "--kind reminder|source"}
		out.Semantics = []string{"Exactly one of --body TEXT or --body-file PATH when supplying body; body must be nonblank UTF-8, at most 16 KiB. A body file must be regular and owner-private on POSIX.", "New notes and explicit scope edits require an existing same-project worker or job attachment. Phase is only an optional label, not a sufficient attachment. No current job is inferred. Retained phase-only notes can still be consumed without a scope edit.", recovery}
		if operation == "notes add" {
			out.Usage = "<bound launcher> notes add (--body TEXT | --body-file PATH) --key KEY [scope]"
			out.Required = []string{"--body TEXT | --body-file PATH", "--key KEY"}
			out.Semantics = append(out.Semantics, "Defaults to the bound worker and kind reminder. --clear-worker removes the default worker and requires --job; --phase may label that attachment.")
			out.Example = "<bound launcher> notes add --body 'Revisit after the API settles' --key reminder-1"
		} else {
			out.Usage = "<bound launcher> notes update NOTE_ID --expect-revision N --key KEY [changes]"
			out.Required = []string{"NOTE_ID", "--expect-revision N", "--key KEY", "at least one optional edit"}
			out.Optional = append(out.Optional, "--body TEXT | --body-file PATH", "--clear-job", "--clear-phase", "--status pending|consumed|dismissed", "--reason TEXT")
			out.Semantics = append(out.Semantics, guard, "Use --clear-worker/--clear-job/--clear-phase instead of empty scope values; each conflicts with its corresponding value flag. Consuming needs no unchanged body. Dismissal requires a nonblank --reason; pending/consumed cannot have a nonempty reason.")
			out.Example = "<bound launcher> notes update NOTE_ID --expect-revision 2 --key reminder-consumed-1 --status consumed"
		}
		out.Documentation = "docs/notes.md"
	case "map update":
		out.Usage = "<bound launcher> map update ENTRY_ID --expect-revision N --file /absolute/changes.json --key KEY"
		out.Required = []string{"ENTRY_ID", "--expect-revision N", "--file ABSOLUTE_PATH", "--key KEY"}
		out.Semantics = []string{guard, "The regular JSON file is at most 64 KiB and contains only changed fields, not an entry wrapper. Unknown, duplicate and null fields are refused. Kind cannot change.", "Empty optional text/arrays clears where valid for the entry kind. Selected arrays replace that array; existing phase IDs must be retained (archive phases instead of deleting).", recovery}
		out.FileFields = map[string]string{"title": "Nonblank title", "description": "Project narrative", "state": "Editorial state", "where": "Workstream position", "next": "Workstream milestone", "trigger": "Return-point reconsideration trigger", "phases": "Ordered workstream phases: id, name, state, archived", "links": "Links: label, url", "archived": "Boolean", "position": "Integer 0-1000000"}
		out.FileExample = map[string]any{"state": "In progress"}
		out.Example = "<bound launcher> map update ENTRY_ID --expect-revision 3 --file /private/changes.json --key project-state-1"
		out.Documentation = "docs/project-map.md"
	case "publication prepare":
		out.Usage = "<bound launcher> publication prepare --file /absolute/changes.json --output /absolute/new-approved.json"
		out.Required = []string{"--file ABSOLUTE_PATH", "--output NEW_ABSOLUTE_PATH"}
		out.Semantics = []string{"Administrator only. Reads approved content and selected public revisions, previews the result, and saves an exact owner-private apply file; does not publish. No --key or revision CLI flag.", "The owner-private regular plan file is at most 8 KiB: current expected_revision plus at least one edit. Omitted content stays unchanged; null, unknown and duplicate fields are refused. Output must be new, in an existing directory, and different from input; never overwrites.", "Selections use upsert/remove lists, not full replacements. Pin explicit job-summary/map revisions. Remove by selection key (reviews by PR number); description '' clears description.", "Retain the prepared file and returned preview_sha256. Apply separately with an explicit key; after uncertain apply retry that same key, exact file, hash and binding."}
		out.FileFields = map[string]string{"expected_revision": "Required current publication configuration revision (>=1)", "description": "Optional public description", "job_refs": "upsert: key/job_id/summary_revision; remove: keys", "map_refs": "upsert: key/entry_id/revision; remove: keys", "worker_refs": "upsert: key/worker_id/label; remove: keys", "reviews": "upsert: number/title/url; remove: PR numbers"}
		out.FileExample = map[string]any{"expected_revision": 2, "description": "Our project direction"}
		out.Example = "<bound launcher> publication prepare --file /private/changes.json --output /private/approved.json"
		out.Next = "Review the prepared publication, then publication apply --file PREPARED_FILE --preview-sha256 RETURNED_HASH --key KEY. Use publication preview --file PREPARED_FILE to recover a lost preview hash."
		out.Documentation = "docs/public-site.md"
	default:
		return operationHelpResponse{}, false
	}
	return out, true
}
