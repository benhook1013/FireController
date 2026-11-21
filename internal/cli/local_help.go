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
		Binding: "Use this role/project launcher; --profile and --profiles-file select its binding, never a global active profile.",
		CommandGroups: []helpGroup{
			{"local discovery", []helpCommand{
				{"help | --help | -h", "Local orientation and selected operation --help; no I/O."},
				{"version", "Executable version/source; no credential or network reads."},
				{"context", "Selected identity/source; no credential or service I/O."},
				{"doctor", "Authenticate the bound instance/project/worker; no provider requests."},
			}},
			{"resume and jobs", []helpCommand{
				{"jobs create --title TEXT --key KEY", "Create a job; operation --help lists fields."},
				{"jobs help", "All job operations and local --help."},
				{"jobs primary", "Bound-worker focus; primary-set/clear/history have --help."},
				{"workers help", "Worker discovery, pause/resume and guarded history."},
				{"jobs assigned --status open", "Open assignments; --all includes completed history."},
				{"jobs status JOB_ID", "Compact status/revision; read loads full context."},
				{"jobs read JOB_ID", "Full brief/progress, remaining checklist and job-note previews; no inbox."},
				{"jobs summary JOB_ID", "Authored summary, not the private brief or publication."},
			}},
			{"notes and messages", []helpCommand{
				{"notes help / inbox help", "Local command discovery and operation --help."},
				{"notes list", "Pending previews; read selected full notes."},
				{"notes read NOTE_ID", "Full current body; notes history reads archives explicitly."},
				{"inbox list / sent", "Pending incoming or sent previews; --all includes handled history."},
				{"inbox wait --cursor EVENT_CURSOR", "Bounded event wait using inbox snapshot event_cursor."},
				{"inbox thread CONVERSATION_ID", "One conversation page; no receipt-state change."},
				{"inbox read / delivery MESSAGE_ID", "Full message or compact current recipient receipt. No writes."},
				{"inbox send --to WORKER_ID --body TEXT --key KEY", "Send as the bound worker; explicit recoverable key."},
			}},
			{"guarded changes and archives", []helpCommand{
				{"jobs update | checklist-item | checklist-set | summary-set", "Explicit keys/guards; operation --help gives fields."},
				{"map update ENTRY_ID --expect-revision N --file CHANGES_JSON --key KEY", "Change selected Project fields; preserve omissions."},
				{"publication prepare --file CHANGES_JSON --output NEW_PRIVATE_FILE", "Manager preview; private file, no publication."},
				{"jobs history | progress-history | revision", "Private archives; operation --help gives syntax. No automatic pagination."},
				{"credentials help", "Project grants and permissions; no raw token arguments."},
				{"reviews queue / queue-add / queue-move / queue-remove / queue-restore / queue-reorder", "Queue reads/changes; project writes require revision/key. Local --help."},
				{"reviews operations", "Attention including unfinished results; --status pending selects provider work, --all includes history. operation ID reads detail."},
				{"reviews help", "Review commands, following and exact --help."},
				{"reviews evidence | adjudication | decide | fix | decide-batch | fix-batch | finalize", "Native evidence/guarded proof: docs/review-controls.md. publish --help covers GitHub checkpoints."},
			}},
			{"project content and setup", []helpCommand{
				{"map | items | milestones | presentation | publication | vars | profile | import | history", "Use GROUP help for local discovery; setup/import/collection require authority."},
			}},
		},
	}
	out.Documentation.Basis = "Use documentation from the same release; executable and service revisions may differ."
	out.Documentation.Paths = []string{"docs/development.md", "docs/agent-client.md", "docs/local-api.md"}
	return out
}
