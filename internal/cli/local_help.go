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
		Binding: "Use this role/project launcher; --profile and --profiles-file select its binding, without mutable active selection.",
		CommandGroups: []helpGroup{
			{"local discovery", []helpCommand{
				{"help | --help | -h", "Local orientation and selected operation --help; no I/O."},
				{"version", "Executable version/source; no credential or network reads."},
				{"context", "Selected identity/source; no credential or service I/O."},
				{"doctor", "Authenticate the bound instance/project/worker; no provider requests."},
			}},
			{"resume and jobs", []helpCommand{
				{"jobs primary", "Bound-worker resume focus; primary-set/clear/history have local --help."},
				{"workers help", "Local worker discovery, pause/resume and guarded history syntax."},
				{"jobs assigned --status open", "Open assignments; --all includes completed history. Read jobs for context."},
				{"jobs status JOB_ID", "Compact status/revision; jobs read loads full context."},
				{"jobs read JOB_ID", "Full brief/progress, remaining checklist and job-note previews; no inbox."},
				{"jobs summary JOB_ID", "Authored summary, separate from private brief and publication."},
			}},
			{"notes and messages", []helpCommand{
				{"notes list", "Pending-note previews; read selected full notes."},
				{"notes read NOTE_ID", "Full current note body; history requires an explicit notes history read."},
				{"inbox list / sent", "Pending incoming or sent previews; --all includes handled history."},
				{"inbox wait --cursor EVENT_CURSOR", "Bounded event wait using inbox snapshot event_cursor."},
				{"inbox thread CONVERSATION_ID", "One conversation page for surrounding context; no receipt-state change."},
				{"inbox read / delivery MESSAGE_ID", "Full message or compact current recipient receipt. No writes."},
				{"inbox send --to WORKER_ID --body TEXT --key KEY", "Send one message as the bound worker; explicit mutation with a recoverable key."},
			}},
			{"guarded changes and archives", []helpCommand{
				{"jobs update | checklist-item | checklist-set | summary-set", "Explicit keys and revision guards; operation --help gives fields."},
				{"map update ENTRY_ID --expect-revision N --file CHANGES_JSON --key KEY", "Change only selected Project fields; omitted content stays unchanged."},
				{"publication prepare --file CHANGES_JSON --output NEW_PRIVATE_FILE", "Manager preview; private apply file, no publication."},
				{"jobs progress-history JOB_ID", "One progress-history page; no automatic continuation."},
				{"jobs revision JOB_ID REVISION", "One exact retained state and original brief; no paging."},
				{"jobs history JOB_ID", "One private job-history page; no automatic pagination."},
				{"credentials help", "Project grants and permissions; no raw token arguments."},
				{"reviews queue / queue-add / queue-move / queue-remove", "Queue reads/add/move/removal; revision/key required for changes. Local --help."},
				{"reviews operations", "Pending operations; --all includes history. operation ID reads detail."},
				{"reviews help", "Local review discovery and exact operation --help."},
				{"reviews evidence | adjudication | decide | fix | decide-batch | fix-batch | finalize", "Native evidence and guarded decisions/fix proof: docs/review-controls.md. reviews publish --help covers GitHub checkpoints."},
			}},
		},
	}
	out.Documentation.Basis = "Use documentation from the same release; executable and service revisions may differ."
	out.Documentation.Paths = []string{"docs/development.md", "docs/agent-client.md", "docs/local-api.md"}
	return out
}
