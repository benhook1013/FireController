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
		Binding: "Use this role/project launcher. --profile and --profiles-file select an explicit binding; no mutable active selection.",
		CommandGroups: []helpGroup{
			{"local discovery", []helpCommand{
				{"help | --help | -h", "Local orientation and selected operation --help; no I/O."},
				{"version", "Local version and client source revision/state; no profile, credential or network reads."},
				{"context", "Selected identity and running client revision; no credentials or service I/O."},
				{"doctor", "Authenticate the bound instance/project/worker; no provider requests."},
			}},
			{"resume and jobs", []helpCommand{
				{"jobs primary", "Bound-worker resume focus; primary-set/clear/history have local --help."},
				{"workers help", "Local worker discovery, pause/resume and guarded history syntax."},
				{"jobs assigned --status open", "Open assignments by default; --all includes completed history. Not full instructions."},
				{"jobs status JOB_ID", "Compact status/revision and bounded progress previews; full context requires jobs read."},
				{"jobs read JOB_ID", "Full brief/progress, remaining checklist and job-note previews; no inbox."},
				{"jobs summary JOB_ID", "Explicit authored summary, separate from the private brief and not automatically public."},
			}},
			{"notes and messages", []helpCommand{
				{"notes list", "Pending-note previews; read selected full notes."},
				{"notes read NOTE_ID", "Full current note body; history requires an explicit notes history read."},
				{"inbox list / sent", "Pending incoming or sent previews; --all includes handled history."},
				{"inbox wait --cursor EVENT_CURSOR", "Quiet bounded event wait using inbox snapshot event_cursor; --help is local."},
				{"inbox thread CONVERSATION_ID", "One conversation page for surrounding context; no receipt-state change."},
				{"inbox read / delivery MESSAGE_ID", "Full message or compact current recipient receipt. No writes."},
				{"inbox send --to WORKER_ID --body TEXT --key KEY", "Send one message as the bound worker; explicit mutation with a recoverable key."},
			}},
			{"guarded changes and archives", []helpCommand{
				{"jobs update | checklist-item | checklist-set | summary-set", "Explicit keys and revision guards; operation --help gives fields."},
				{"map update ENTRY_ID --expect-revision N --file CHANGES_JSON --key KEY", "Change only selected Project fields; omitted content stays unchanged."},
				{"publication prepare --file CHANGES_JSON --output NEW_PRIVATE_FILE", "Administrator preview; saves a private apply file without publishing."},
				{"jobs progress-history JOB_ID", "One explicit current-progress archive page; no automatic continuation."},
				{"jobs revision JOB_ID REVISION", "One exact retained state and original brief; no paging."},
				{"jobs history JOB_ID", "One private job-history page; no automatic pagination."},
				{"reviews policy / policy-configure / execution-configure / attempt-settle", "Scoped project management; operation --help supplies exact guards and file formats."},
				{"workers create / credentials grant / update / revoke", "Bound project worker/access management; no instance administration or raw token arguments."},
				{"reviews queue / queue-add / queue-move / queue-remove", "Queue reads/add/move/removal; revision/key required for changes. Local --help."},
				{"reviews operations", "Pending operations; --all includes history. operation ID reads detail."},
				{"reviews help", "Local review discovery and exact operation --help."},
				{"reviews evidence | adjudication | decide | fix | decide-batch | fix-batch | finalize", "Native evidence and guarded decisions/fix proof: docs/review-controls.md. reviews publish --help covers GitHub checkpoints."},
			}},
		},
	}
	out.Documentation.Basis = "Use documentation from the same release. Client source metadata identifies this executable, not the service deployment."
	out.Documentation.Paths = []string{"docs/development.md", "docs/agent-client.md", "docs/local-api.md"}
	return out
}
