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
				{"help | --help | -h", "This local orientation; no profile, credential or network reads."},
				{"version", "Local version JSON only; no profile, credential or network reads, including with bound launcher selectors."},
				{"context", "Read the selected local profile identity without opening credentials or contacting the service."},
				{"doctor", "Authenticate the bound instance/project/worker; no provider requests."},
			}},
			{"resume and jobs", []helpCommand{
				{"jobs assigned --status open", "Compact assigned jobs; not full instructions or a complete resume context."},
				{"jobs context JOB_ID", "Explicit current private brief, latest checkpoint and bounded pending coordination; inspect completeness and omissions."},
				{"jobs read JOB_ID", "Full private job detail and current revision."},
				{"jobs summary JOB_ID", "Explicit authored summary, separate from the private brief and not automatically public."},
			}},
			{"notes and messages", []helpCommand{
				{"notes list", "Compact pending-note discovery; use explicit reads for full text."},
				{"notes read NOTE_ID", "Full current note body; history requires an explicit notes history read."},
				{"inbox list", "Bounded incoming-message discovery; reads do not mark seen or acknowledged."},
				{"inbox thread CONVERSATION_ID", "Explicit conversation page with full message bodies."},
				{"inbox send --to WORKER_ID --body TEXT --key KEY", "Send one message as the bound worker; explicit mutation with a recoverable key."},
			}},
			{"guarded changes and archives", []helpCommand{
				{"jobs edit | update | checkpoint | checklist-set | summary-set", "Deliberate job changes use explicit keys and required revision guards. See release documentation for fields and byte limits."},
				{"jobs history JOB_ID", "One explicit private job-history page; list/history/thread commands never silently fetch every page."},
				{"reviews queue", "Stored review workspace only. Review requests, source refreshes and publication are separate authorized operations; help grants no permission."},
			}},
		},
	}
	out.Documentation.Basis = "Use documentation from the same release as this executable; a current source checkout or server may expose newer behavior. Release provenance is separate from version JSON."
	out.Documentation.Paths = []string{"docs/development.md", "docs/agent-client.md", "docs/local-api.md"}
	return out
}
