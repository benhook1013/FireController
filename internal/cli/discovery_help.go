package cli

import "strings"

// This is discovery metadata, not an execution router. The bidirectional
// catalogue test compares it and operationHelp with the actual dispatchers.
// Privileged setup/import commands are public, not hidden worker operations.
var discoveryGroups = map[string]string{
	"profile":                   "create launcher",
	"workers":                   "create retirement retire list pause-state pause resume pause-history",
	"credentials":               "grant update revoke",
	"jobs":                      "create list assigned status read revision history progress-history summary summary-set checklist checklist-set checklist-item checklist-history primary primary-set primary-clear primary-history workstream workstream-set workstream-history update",
	"inbox":                     "list sent delivery snapshot events wait thread read send seen ack",
	"notes":                     "list read revision history add update",
	"map":                       "list read history revision create replace update import-preview import-apply import-read import-history import-revision",
	"items":                     "list read history revision create set link unlink upsert",
	"milestones":                "list read history revision create set",
	"presentation":              "read history revision replace",
	"publication":               "read preview prepare apply unpublish",
	"vars":                      "list get set unset history",
	"history":                   "collect collection pages export-page resume cancel",
	"import":                    "state preview apply",
	"reviews":                   "following follow unfollow policy policy-configure execution execution-configure attempt-settle hosted-recover settle-hosted queue queue-add queue-move queue-remove queue-restore queue-reorder queue-priority queue-edit pull runs attempts run evidence adjudication route-create route routes route-decide route-fix assess source-assess ci-assess ci-wait actions-liveness actions-run activity scheduling readiness operations watch operation-control decide fix correct-fix operational-fix decide-batch fix-batch finalize publish publication publication-recover source-operation request start run-read operation source-refresh subagent-record subagent-read subagent-update import-preview import-apply transfer-preview transfer-apply rounds obligations",
	"reviews rounds":            "read grant close taper adopt",
	"reviews rounds adopt":      "preview apply",
	"reviews obligations":       "list read retarget resolve adopt",
	"reviews obligations adopt": "preview apply",
	"reviews scheduling":        "read history diagnostics configure pause resume",
}

func discoveryHelpPath(args []string) (string, bool) {
	if len(args) < 2 {
		return "", false
	}
	for _, arg := range args {
		if arg == "" || strings.ContainsAny(arg, " \t\r\n") {
			return "", false
		}
	}
	var path string
	if args[0] == "help" {
		path = strings.Join(args[1:], " ")
	} else if args[len(args)-1] == "--help" || args[len(args)-1] == "help" {
		path = strings.Join(args[:len(args)-1], " ")
		// Bare `help` is a group alias only, never an operation flag/value.
		if args[len(args)-1] == "help" {
			if _, ok := discoveryGroups[path]; !ok {
				return "", false
			}
		}
	} else {
		return "", false
	}
	if _, ok := discoveryGroups[path]; ok && !strings.Contains(path, " ") {
		path += " help"
	}
	_, ok := operationHelp(path)
	return path, ok
}

func discoveryGroupHelp(group string) (operationHelpResponse, bool) {
	children, ok := discoveryGroups[group]
	if !ok {
		return operationHelpResponse{}, false
	}
	out := operationHelpResponse{
		Usage: "<bound launcher> " + group + " help", Required: []string{}, Optional: []string{},
		Semantics:     []string{"Select an operation's --help for exact local syntax. Help reads no profile, credential, input file or service; listing a command grants no authority to execute it."},
		Documentation: "docs/development.md",
	}
	for _, child := range strings.Fields(children) {
		out.Optional = append(out.Optional, group+" "+child+" --help")
	}
	out.Example = "<bound launcher> " + out.Optional[0]
	return out, true
}

// Fill gaps in the older selected reference without replacing its richer help
// or changing argument validation, preparation, profile selection or routing.
func discoveryOperationHelp(operation string) (operationHelpResponse, bool) {
	if strings.HasSuffix(operation, " help") {
		return discoveryGroupHelp(strings.TrimSuffix(operation, " help"))
	}
	if _, ok := discoveryGroups[operation]; ok {
		return discoveryGroupHelp(operation)
	}
	out := operationHelpResponse{Usage: "<bound launcher> " + operation, Required: []string{}, Optional: []string{}, Documentation: "docs/development.md"}
	args, semantics := "", ""
	switch operation {
	case "context":
		semantics = "Read the selected local profile's identity and executable source metadata, not credentials. Does not authenticate or contact the service."
	case "doctor":
		semantics = "Verify the bound instance/project/worker and read current worker pause and effective operator variables. No provider requests or PR scan."
	case "version":
		semantics = "Read executable version and source metadata locally, without configuration or service access."
	case "profile create":
		args = "--profile NAME --profiles-file ABSOLUTE_PATH --server URL --instance ID --project ID --worker ID --credential-file PATH"
		semantics = "Create a private local profile without service or credential access. Explicit selectors are required; existing bindings are preserved. Does not provision a credential."
		out.Documentation = "docs/onboarding.md"
	case "profile launcher":
		args = "--profile NAME --profiles-file ABSOLUTE_PATH --output NEW_ABSOLUTE_PATH [--executable ABSOLUTE_PATH]"
		semantics = "Create a POSIX launcher from an existing local profile; never overwrites a different file or contacts the service."
		out.Documentation = "docs/onboarding.md"
	case "jobs list", "jobs assigned":
		args = "[--limit N] [--cursor CURSOR] [--status open|active|blocked|parked|completed]"
		semantics = "Read one compact page; continue has_more explicitly with the returned cursor and the same filters. No full brief, history, provider work or writes."
		if operation == "jobs list" {
			args += " [--worker WORKER_ID]"
		} else {
			args += " [--all]"
			semantics += " Assigned defaults to open work for the bound worker; --all includes completed history and conflicts with --status."
		}
	case "jobs summary":
		args, semantics = "JOB_ID", "Read the authored summary and its independent revision; not the private brief and not publication."
		out.Documentation = "docs/job-summaries.md"
	case "jobs summary-set":
		args = "JOB_ID --expect-revision N --expect-summary-revision N --title TEXT (--body TEXT | --body-file PATH) --key KEY"
		semantics = "Replace the authored summary under job and summary revision guards. Empty title/body explicitly clears it. Does not publish the job or replace its brief. Retain the exact request, binding and key after uncertain output."
		out.Documentation = "docs/job-summaries.md"
	case "jobs checklist":
		args, semantics = "JOB_ID", "Read the ordered checklist, independent checklist revision and current job revision without changing completion."
		out.Documentation = "docs/job-progress.md"
	case "jobs checklist-history", "notes history":
		args, semantics = "ID [--limit N] [--cursor CURSOR]", "Read one bounded history page; continue has_more explicitly. No automatic pagination, writes or provider work."
		out.Documentation = "docs/job-progress.md"
		if operation == "notes history" {
			out.Documentation = "docs/notes.md"
		}
	case "inbox events":
		args = "--cursor EVENT_CURSOR [--limit N] [--wait-ms N]"
		semantics = "Replay one incoming-event page, optionally waiting 0-4000 ms. Use inbox snapshot event_cursor, not its pagination cursor. Does not read full bodies, acknowledge messages or automatically paginate."
		out.Documentation = "docs/message-events.md"
	case "map list":
		args, semantics = "[--kind workstream|return_point|section] [--archived true|false] [--limit N] [--cursor CURSOR]", "Read one compact Project map page without expanding every entry's history or changing state."
		out.Documentation = "docs/project-map.md"
	case "map read", "map history", "map revision", "map create", "map replace", "jobs workstream", "jobs workstream-history", "jobs workstream-set":
		out.Documentation = "docs/project-map.md"
		semantics = "Project map content and job/workstream association revisions are independent of job progress. Omitted work stays unchanged; reads perform no writes or provider work."
		switch operation {
		case "map read":
			args = "ENTRY_ID"
		case "map history":
			args = "ENTRY_ID [--limit N] [--cursor CURSOR]"
		case "map revision":
			args = "ENTRY_ID REVISION"
		case "map create":
			args = "--file ABSOLUTE_JSON_PATH --key KEY"
		case "map replace":
			args = "ENTRY_ID --expect-revision N --file ABSOLUTE_JSON_PATH --key KEY"
			semantics += " Full editable replacement; use map update to change selected fields."
		case "jobs workstream":
			args = "JOB_ID"
		case "jobs workstream-history":
			args = "JOB_ID [--limit N] [--cursor CURSOR]"
		case "jobs workstream-set":
			args = "JOB_ID (--workstream ENTRY_ID | --clear) --expect-revision N --key KEY"
		}
	case "items list", "milestones list", "items read", "milestones read", "items history", "milestones history", "items revision", "milestones revision":
		out.Documentation = "docs/tracking.md"
		semantics = "Read project tracking records using stable keys. Lists and history return one explicit page; no automatic pagination, provider work or writes."
		parts := strings.Fields(operation)
		switch parts[1] {
		case "list":
			if parts[0] == "items" {
				args = "[--limit N] [--cursor CURSOR] [--status STATUS] [--milestone KEY] [--parent KEY] [--label LABEL] [--link TYPE:REF]"
			} else {
				args = "[--state open|closed]"
			}
		case "read":
			args = "KEY"
		case "history":
			args = "KEY [--limit N] [--cursor CURSOR]"
		case "revision":
			args = "KEY REVISION"
		}
	case "presentation read", "presentation history", "presentation revision", "presentation replace":
		out.Documentation = "docs/project-presentation.md"
		semantics = "Read or explicitly replace project presentation settings; replacement requires management authority, a private JSON file and the current revision. Retain the exact request, key and binding after uncertain output."
		switch strings.TrimPrefix(operation, "presentation ") {
		case "history":
			args = "[--limit N] [--cursor CURSOR]"
		case "revision":
			args = "REVISION"
		case "replace":
			args = "--expect-revision N --file ABSOLUTE_JSON_PATH --key KEY"
		}
	case "publication read", "publication preview", "publication apply", "publication unpublish":
		out.Documentation = "docs/public-site.md"
		semantics = "Explicit project-management public-site configuration, separate from ordinary private coordination. Preview does not publish. Retain the exact apply file, preview hash, key and binding after uncertain output."
		switch strings.TrimPrefix(operation, "publication ") {
		case "preview":
			args = "--file ABSOLUTE_JSON_PATH"
		case "apply":
			args = "--file ABSOLUTE_JSON_PATH --preview-sha256 HASH --key KEY"
		case "unpublish":
			args = "--expect-revision N --key KEY"
		}
	case "vars list", "vars get", "vars set", "vars unset", "vars history":
		out.Documentation = "docs/operator-variables.md"
		semantics = "Opaque operator strings: project overrides replace instance values; reads use the bound project. Set/unset require administration. Keys are 1-64 lowercase letters/digits/_.- starting with a letter or digit; values are 1-240 UTF-8 bytes."
		switch strings.TrimPrefix(operation, "vars ") {
		case "get":
			args = "KEY"
		case "set":
			args = "KEY VALUE [--project] [--expect-revision N] [--key KEY]"
		case "unset":
			args = "KEY [--project] [--expect-revision N] [--key KEY]"
		case "history":
			args = "KEY [--project] [--limit N] [--cursor CURSOR]"
		}
		semantics += " Set/unset generate a key when omitted; --project selects the bound project's override for writes/history."
	case "history collect", "history collection", "history pages", "history export-page", "history resume", "history cancel":
		out.Documentation = "docs/history-collection.md"
		semantics = "Administrator-only selected historical collection. Collect/resume explicitly authorize provider acquisition; reads and export use retained captures, and no command automatically imports history."
		switch strings.TrimPrefix(operation, "history ") {
		case "collect":
			args = "--pulls N,N --key KEY [--wait N]"
		case "collection":
			args = "COLLECTION_ID [--wait N]"
		case "pages":
			args = "COLLECTION_ID [--limit N] [--cursor CURSOR]"
		case "export-page":
			args = "COLLECTION_ID PAGE_ID --output NEW_ABSOLUTE_PATH"
		default:
			args = "COLLECTION_ID --expect-revision N --key KEY"
		}
	case "import state", "import preview", "import apply", "reviews import-preview", "reviews import-apply", "reviews transfer-preview", "reviews transfer-apply", "reviews rounds adopt preview", "reviews rounds adopt apply", "reviews obligations adopt preview", "reviews obligations adopt apply", "map import-preview", "map import-apply":
		out.Documentation = "docs/legacy-adoption.md"
		semantics = "Privileged prepared import/transfer, not routine worker mutation. Preview validates without applying; apply retains source provenance and requires the exact preview fingerprint and explicit key. No provider acquisition or automatic review request. Preserve the exact file, fingerprint, key and binding after uncertain output."
		if operation != "import state" {
			args = "--file ABSOLUTE_PRIVATE_JSON_PATH"
			if strings.HasSuffix(operation, "apply") {
				args += " --preview HASH --key KEY"
				if strings.HasPrefix(operation, "map ") {
					args = "--file ABSOLUTE_PRIVATE_JSON_PATH --preview-sha256 HASH --key KEY"
				}
			}
		}
	case "map import-read", "map import-history", "map import-revision":
		args, semantics = "SOURCE_KEY", "Read retained map-import provenance without applying or acquiring source data."
		if operation == "map import-history" {
			args += " [--limit N] [--cursor CURSOR]"
		}
		if operation == "map import-revision" {
			args += " REVISION"
		}
		out.Documentation = "docs/map-import.md"
	case "reviews queue-edit":
		args, semantics = "--file ABSOLUTE_PRIVATE_JSON_PATH --key KEY", "Project-write selected queue metadata/order edit. Preserves omitted entries and retained review history; no provider refresh or review request. Retain the exact file, key and binding after uncertain output."
		out.Documentation = "docs/review-workspace.md"
	case "reviews pull", "reviews attempts":
		args, semantics = "NUMBER [--snapshot SNAPSHOT_ID]", "Read retained PR state, not live provider data. Snapshot selects retained history; no refresh, review request or automatic pagination."
		if operation == "reviews attempts" {
			args += " [--limit N] [--cursor CURSOR]"
		}
		out.Documentation = "docs/review-workspace.md"
	case "reviews obligations list", "reviews obligations read", "reviews obligations retarget", "reviews obligations resolve":
		out.Documentation = "docs/adoption-obligations.md"
		semantics = "Carried obligations are retained unfinished source work, not newly verified defects. Reads never resolve them. Writer changes require current obligation revision, explicit evidence/reason and original-key recovery; no provider refresh or review request."
		switch strings.TrimPrefix(operation, "reviews obligations ") {
		case "list":
			args = "[--pr NUMBER] [--status open|resolved] [--limit N] [--cursor CURSOR]"
		case "read":
			args = "OBLIGATION_ID"
		case "retarget":
			args = "OBLIGATION_ID --expected-revision N (--pr NUMBER | --unassigned) --reason TEXT --key KEY"
		case "resolve":
			args = "OBLIGATION_ID --expected-revision N --reason TEXT --evidence REF --key KEY"
		}
	default:
		return operationHelpResponse{}, false
	}
	if args != "" {
		out.Usage += " " + args
		required, optional, _ := strings.Cut(args, "[")
		if required = strings.TrimSpace(required); required != "" {
			out.Required = []string{required}
		}
		if optional != "" {
			for _, part := range strings.Split(optional, "[") {
				out.Optional = append(out.Optional, strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(part), "]")))
			}
		}
	}
	out.Semantics = []string{semantics}
	out.Example = out.Usage
	return out, true
}
