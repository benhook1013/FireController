package cli

type helpCommand struct {
	Command string `json:"command"`
	Scope   string `json:"scope"`
}
type helpGroup struct {
	Name     string        `json:"name"`
	Commands []helpCommand `json:"commands"`
}
type helpResponse struct {
	Usage         string      `json:"usage"`
	Binding       string      `json:"binding"`
	CommandGroups []helpGroup `json:"command_groups"`
	Documentation struct {
		Basis string   `json:"basis"`
		Paths []string `json:"paths"`
	} `json:"documentation"`
}

// localHelp contains a bounded orientation, not a second argument parser or an
// exhaustive tool inventory. It never resolves the selected profile or service.
func localHelp() helpResponse {
	out := helpResponse{
		Usage:   "<bound launcher> <command> [arguments]",
		Binding: "Use the launcher assigned to this role/project. Global --profile and --profiles-file select an explicit local binding; no mutable active selection is saved.",
		CommandGroups: []helpGroup{
			{"local discovery", []helpCommand{
				{"help | --help | -h", "Local orientation. Selected commands accept --help; use matching release docs for other operations. No profile, credential or network reads."},
				{"version", "Local version and running source revision/state; no profile, credential or network reads, including with bound launcher selectors."},
				{"context", "Read selected identity and running client revision/state without credentials or service I/O."},
				{"doctor", "Authenticate the bound instance/project/worker; no provider requests."},
			}},
			{"resume and jobs", []helpCommand{
				{"jobs assigned --status open", "Default open assigned jobs (active, blocked, parked); --all explicitly includes completed history. Not full instructions."},
				{"jobs read JOB_ID", "Current private brief, progress, remaining checklist and exact-job pending notes; no inbox."},
				{"jobs summary JOB_ID", "Explicit authored summary, separate from the private brief and not automatically public."},
			}},
			{"notes and messages", []helpCommand{
				{"notes list", "Compact pending-note discovery; use explicit reads for full text."},
				{"notes read NOTE_ID", "Full current note body; history requires an explicit notes history read."},
				{"inbox list", "Pending messages by default, including seen-but-unhandled. --all explicitly includes handled history; reads never mark seen or acknowledged."},
				{"inbox wait --cursor EVENT_CURSOR", "One quiet bounded wait for new incoming-message events. Use event_cursor from inbox snapshot; inbox wait --help is local."},
				{"inbox thread CONVERSATION_ID", "One conversation page for surrounding context; no receipt-state change."},
				{"inbox read MESSAGE_ID", "One full selected message; no older bodies or receipt-state change. Use inbox thread for conversation context."},
				{"inbox send --to WORKER_ID --body TEXT --key KEY", "Send one message as the bound worker; explicit mutation with a recoverable key."},
			}},
			{"guarded changes and archives", []helpCommand{
				{"jobs update | checklist-item | checklist-set | summary-set", "Deliberate job changes use explicit keys and required revision guards. See release documentation for fields and byte limits."},
				{"map update ENTRY_ID --expect-revision N --file CHANGES_JSON --key KEY", "Change only selected Project fields; omitted content stays unchanged."},
				{"publication prepare --file CHANGES_JSON --output NEW_PRIVATE_FILE", "Administrator linked-selection preview; writes an exact private apply file without publishing."},
				{"jobs progress-history JOB_ID", "One explicit current-progress archive page; no automatic continuation."},
				{"jobs history JOB_ID", "One explicit private job-history page; list/history/thread commands never silently fetch every page."},
				{"reviews queue", "Stored review workspace only. Review requests, source refreshes and publication are separate authorized operations; help grants no permission."},
				{"reviews operations", "One compact pending-operation page; --all includes retained history for lost receipts. Select reviews operation OPERATION_ID for detail; no provider work."},
				{"reviews help", "Local review command discovery; decide, fix, finalize and publish each accept --help with exact fields, guards and side effects."},
				{"reviews evidence | adjudication | decide | fix | finalize", "Native evidence reads and guarded finding decisions/fix proof; see docs/review-controls.md. reviews publish --help explains the separate GitHub checkpoint action."},
			}},
		},
	}
	out.Documentation.Basis = "Use documentation from the same release as this executable; a current source checkout or server may expose newer behavior. Client source metadata does not identify the service deployment."
	out.Documentation.Paths = []string{"docs/development.md", "docs/agent-client.md", "docs/local-api.md"}
	return out
}
