package cli

import "strings"

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
	if call.command == "reviews" && (call.subcommand == "evidence" || call.subcommand == "adjudication" || call.subcommand == "assess") && len(call.position) == 1 && !reviewID(call.position[0], "review_evidence_") {
		return "EVIDENCE_ID must be review_evidence_ followed by exactly 32 lowercase hexadecimal characters. Use the evidence ID returned by the original operation; run " + operation + " --help for details."
	}
	if _, ok := operationHelp(operation); ok {
		return "Run the bound launcher with " + operation + " --help for required fields and examples."
	}
	if call.command == "reviews" || strings.HasPrefix(call.helpOperation, "reviews ") {
		return "Run the bound launcher with reviews --help for supported review commands; reviews rounds --help lists per-PR limits and closure."
	}
	return "Run the bound launcher with help to discover supported commands and matching-release documentation."
}

// This selected local reference supplements the real parsers. Only the exact
// operation-help grammar reaches it, before any profile, file or service work.
func operationHelp(operation string) (operationHelpResponse, bool) {
	guard := "Use the current resource --expect-revision. Omitted fields stay unchanged; at least one edit is required."
	recovery := "Use an explicit --key (1-128 printable ASCII bytes without surrounding whitespace). After uncertain output retry the same key, exact request and binding; never invent a replacement key."
	evidenceID := "EVIDENCE_ID (review_evidence_ followed by exactly 32 lowercase hexadecimal characters)"
	out := operationHelpResponse{Required: []string{}, Optional: []string{}}
	switch operation {
	case "workers help":
		out.Usage = "<bound launcher> workers help"
		out.Optional = []string{"workers list --help", "workers pause-state --help", "workers pause --help", "workers resume --help", "workers pause-history --help"}
		out.Semantics = []string{"Worker discovery stays in the bound project. Pause reads and changes target the bound worker, independently of job status, primary focus and review execution.", "All help is local; no profile, credential or network reads."}
		out.Example = "<bound launcher> workers list --help"
		out.Documentation = "docs/worker-pause.md"
	case "workers list":
		out.Usage = "<bound launcher> workers list [--limit N] [--cursor CURSOR]"
		out.Optional = []string{"--limit N (1-100, default 20)", "--cursor CURSOR"}
		out.Semantics = []string{"Returns one compact page of worker IDs and names in the bound project. Names do not select an authenticated identity or change the current binding.", "Check has_more and continue with the returned cursor. No automatic pagination, pause/history fanout, provider work or writes."}
		out.Example = "<bound launcher> workers list --limit 20"
		out.Documentation = "docs/development.md"
	case "workers pause-state":
		out.Usage = "<bound launcher> workers pause-state"
		out.Semantics = []string{"Reads the bound worker's current worker_id, revision, paused flag and private reason. Initially revision 0, paused false and empty reason.", "Coordination pause does not stop a chat, deny writes or change jobs, primary focus or review execution. No history, provider work or writes."}
		out.Example = "<bound launcher> workers pause-state"
		out.Documentation = "docs/worker-pause.md"
	case "workers pause", "workers resume":
		out.Usage = "<bound launcher> " + operation + " --expect-revision N --key KEY"
		out.Required = []string{"--expect-revision N (current worker-pause revision, including 0)", "--key KEY"}
		out.Semantics = []string{"Read workers pause-state for the current revision. Targets only the bound worker; the pause revision is independent of job and primary-focus revisions.", "Changes coordination state only; does not start/stop a chat, deny mutations, edit jobs or change review execution. Same-state no-op preserves its revision.", recovery}
		out.Example = "<bound launcher> workers resume --expect-revision 2 --key resume-1"
		if operation == "workers pause" {
			out.Usage += " --reason TEXT"
			out.Required = append(out.Required, "--reason TEXT (explicit, including empty; at most 1024 UTF-8 bytes)")
			out.Semantics = append(out.Semantics, "The reason stays private; new assignments, progress changes and reconnects preserve the pause. Resume explicitly with workers resume.")
			out.Example = "<bound launcher> workers pause --expect-revision 0 --key pause-1 --reason 'Awaiting human direction'"
		} else {
			out.Semantics = append(out.Semantics, "Explicitly clears both paused and reason. No --reason flag.")
		}
		out.Documentation = "docs/worker-pause.md"
	case "workers pause-history", "jobs primary-history":
		out.Usage = "<bound launcher> " + operation + " [--limit N] [--cursor CURSOR]"
		out.Optional = []string{"--limit N (1-100, default 20)", "--cursor CURSOR"}
		out.Semantics = []string{"Reads one newest-revision-first history page for the bound worker. Check has_more and continue with the returned cursor. No automatic pagination, full job briefs, provider work or writes."}
		out.Example = "<bound launcher> " + operation + " --limit 20"
		out.Documentation = "docs/worker-pause.md"
		if operation == "jobs primary-history" {
			out.Documentation = "docs/worker-focus.md"
		}
	case "jobs primary":
		out.Usage = "<bound launcher> jobs primary"
		out.Semantics = []string{"Reads the bound worker's optional resume focus: worker_id, independent focus revision and job_id. No focus initially means revision 0 and an empty job_id.", "Focus does not limit concurrent jobs. Blocked, parked and completed jobs can remain primary; read jobs read JOB_ID for full context, or jobs assigned for other open assignments. No automatic job/history reads, provider work or writes."}
		out.Example = "<bound launcher> jobs primary"
		out.Documentation = "docs/worker-focus.md"
	case "jobs primary-set", "jobs primary-clear":
		out.Usage = "<bound launcher> " + operation + " --expect-revision N --key KEY"
		out.Required = []string{"--expect-revision N (current primary-focus revision, including 0)", "--key KEY"}
		out.Semantics = []string{"Read jobs primary for the focus revision; do not use the job revision. Targets the bound worker and preserves job status, brief, assignment and other active jobs.", "An unchanged selection preserves the focus revision. Reassigning or unassigning a selected job clears its previous worker's focus automatically.", recovery}
		out.Example = "<bound launcher> jobs primary-clear --expect-revision 2 --key focus-clear-1"
		if operation == "jobs primary-set" {
			out.Usage = "<bound launcher> jobs primary-set JOB_ID --expect-revision N --key KEY"
			out.Required = append([]string{"JOB_ID (currently assigned to the bound worker in this project)"}, out.Required...)
			out.Example = "<bound launcher> jobs primary-set job_0123456789abcdef0123456789abcdef --expect-revision 0 --key focus-1"
		}
		out.Documentation = "docs/worker-focus.md"
	case "jobs help":
		out.Usage = "<bound launcher> jobs help"
		out.Optional = []string{"jobs primary --help", "jobs primary-set --help", "jobs primary-clear --help", "jobs primary-history --help", "jobs assigned", "jobs read JOB_ID", "jobs status JOB_ID", "jobs revision JOB_ID REVISION", "jobs history --help", "jobs progress-history --help", "jobs update --help", "jobs checklist-item --help"}
		out.Semantics = []string{"Use jobs read for the full current standing brief and progress; jobs status and assigned are selection aids.", "jobs history contains private state snapshots including historical briefs; progress-history contains only archived progress. Each archive read is one bounded page with explicit continuation."}
		out.Example = "<bound launcher> jobs history --help"
		out.Documentation = "docs/development.md"
	case "jobs history", "jobs progress-history":
		out.Usage = "<bound launcher> " + operation + " JOB_ID [--limit N] [--cursor CURSOR]"
		out.Required = []string{"JOB_ID"}
		out.Optional = []string{"--limit N (1-100, default 20)", "--cursor CURSOR"}
		out.Semantics = []string{"One private archive page. Check has_more and use only the returned cursor for deliberate continuation; byte budgets may shorten the page. No automatic pagination, provider work or writes."}
		if operation == "jobs history" {
			out.Semantics = append(out.Semantics, "Oldest revision first. Each item's state includes its historical standing brief and job fields. For a known revision use jobs revision JOB_ID REVISION directly, without paging. This archive does not recreate unimported source history.", "Save larger pages privately and extract only the needed revision locally rather than passing all historical briefs into model context. Use jobs read JOB_ID for current standing instructions.")
		} else {
			out.Semantics = append(out.Semantics, "Newest progress sequence first. Contains archived progress, not historical standing briefs; use jobs revision JOB_ID REVISION for a known brief revision or jobs history JOB_ID for discovery.")
		}
		out.Example = "<bound launcher> " + operation + " job_0123456789abcdef0123456789abcdef --limit 20"
		out.Documentation = "docs/job-state.md"
	case "reviews help":
		out.Usage = "<bound launcher> reviews help"
		out.Optional = []string{"reviews queue-add --help", "reviews queue-move --help", "reviews execution --help", "reviews subagent-record --help", "reviews subagent-read --help", "reviews subagent-update --help", "reviews queue --help", "reviews rounds --help", "reviews evidence --help", "reviews adjudication --help", "reviews route-create --help", "reviews route --help", "reviews routes --help", "reviews route-decide --help", "reviews route-fix --help", "reviews assess --help", "reviews source-assess --help", "reviews ci-assess --help", "reviews readiness --help", "reviews operations --help", "reviews runs --help", "reviews decide --help", "reviews fix --help", "reviews decide-batch --help", "reviews fix-batch --help", "reviews finalize --help", "reviews publish --help", "reviews publication --help", "reviews publication-recover --help", "reviews source-operation --help", "reviews request --help", "reviews operation --help", "reviews source-refresh --help"}
		out.Semantics = []string{"queue, rounds read and assessments read retained state. Read findings with reviews evidence EVIDENCE_ID and decisions/revision with reviews adjudication EVIDENCE_ID.", "source-refresh explicitly reads GitHub; request submits review work. finalize records adjudication and automatically queues its GitHub summary; publish repairs historical delivery. decide/fix and their batches change local evidence. Round grants/closure only change local settings.", "All help is local and reads no profile, credential or service. Documentation paths refer to the same installed release."}
		out.Example = "<bound launcher> reviews decide --help"
		out.Documentation = "docs/review-controls.md"
		out.Next = "docs/review-publication.md describes GitHub checkpoint publication; docs/review-execution.md describes review requests and retained operations."
	case "reviews subagent-record":
		out.Usage = "<bound launcher> reviews subagent-record --file PRIVATE_JSON --key KEY"
		out.Required = []string{"--file PRIVATE_JSON", "--key KEY"}
		out.Semantics = []string{"Records one actual completed subagent review through the bound worker; no administrator import, provider request or CodeRabbit credit.", "Use a private regular UTF-8 JSON file within256KiB with pull_number, full reviewed head_sha, reviewer, scope, ordered started_at/finished_at and findings. Counts and identities are derived. Optional finalized:true requires all decisions, including a valid zero-finding review.", "reviewer and scope: nonblank, at most 128 UTF-8 bytes each. coverage_limits: optional array of at most 16 nonblank strings, each at most 1024 UTF-8 bytes; put long limitations there. findings: array of at most 100 entries; title at most 500 bytes, detail at most 16 KiB, decision reason at most 2048 bytes. See matching docs for enums and update fields.", recovery}
		out.Example = "<bound launcher> reviews subagent-record --file /private/review.json --key subagent-1"
		out.Documentation = "docs/subagent-reviews.md"
	case "reviews subagent-read":
		out.Usage = "<bound launcher> reviews subagent-read RUN_ID"
		out.Required = []string{"RUN_ID (review_run_ followed by32 lowercase hexadecimal characters)"}
		out.Semantics = []string{"Reads one worker-recorded subagent review, its current findings and stable per-run revision without provider work or writes.", "Use the returned revision for guarded changes. Finding indexes are zero-based immutable array order. Does not claim CodeRabbit coverage or merge readiness."}
		out.Example = "<bound launcher> reviews subagent-read review_run_0123456789abcdef0123456789abcdef"
		out.Documentation = "docs/subagent-reviews.md"
	case "reviews subagent-update":
		out.Usage = "<bound launcher> reviews subagent-update RUN_ID --file PRIVATE_JSON --key KEY"
		out.Required = []string{"RUN_ID", "--file PRIVATE_JSON", "--key KEY"}
		out.Semantics = []string{"Use a private regular UTF-8 JSON file within256KiB with current expected_revision and decisions, fixes or finalize:true. Omitted findings stay unchanged; the selected changes commit atomically.", "Each decision has finding, accepted/rejected disposition and its own reason. Each fix has finding, required expected_sha (empty when absent), full fix_sha and proof_note. Finalization freezes counts and requires every finding decided; later corrections retain captured text and frozen counts.", recovery}
		out.Example = "<bound launcher> reviews subagent-update review_run_0123456789abcdef0123456789abcdef --file /private/changes.json --key subagent-fix-1"
		out.Documentation = "docs/subagent-reviews.md"
	case "reviews rounds":
		out.Usage = "<bound launcher> reviews rounds read --pr NUMBER --channel cli|hosted"
		out.Optional = []string{"reviews rounds read --help", "reviews rounds grant --help", "reviews rounds close --help"}
		out.Semantics = []string{"read returns current local per-PR/channel minimum, maximum, used, dry streak, taper, closure and revision without provider work.", "grant sets finite additional bounds or reopens the selected channel; close stops new dispatch. Both require project write authority, the current --expected-revision and an explicit --key. Help performs no action."}
		out.Example = "<bound launcher> reviews rounds read --pr 42 --channel hosted"
		out.Documentation = "docs/review-round-controls.md"
	case "reviews rounds grant":
		out.Usage = "<bound launcher> reviews rounds grant --pr NUMBER --channel cli|hosted --expected-revision N --max N [--min N] --key KEY | --exact N instead of --min/--max"
		out.Required = []string{"--pr NUMBER (canonical positive PR number, 1-2147483647)", "--channel cli|hosted", "--expected-revision N (current round-control revision, initially 0)", "--max N (1-1000 additional completed rounds), or --exact N", "--key KEY"}
		out.Optional = []string{"--min N (0 to --max, default 0)", "--exact N (1-1000; sets equal minimum/maximum; exclusive with --min and --max)"}
		out.Semantics = []string{"Project writers can grant or reopen this PR/channel directly. Read reviews rounds read first for its current revision; the flag is --expected-revision, not --expect-revision.", "Bounds count additional completed rounds, including underway and complete-unfinalized work. One more includes that occupied round; wait for its finalization before requiring a round after it. A maximum below unavoidable occupied work is refused.", "Preserves earned dry streak, completed counts and history. Every grant reopens this channel, including after taper. The next eligible completion evaluates taper again; the requested minimum and maximum stay unchanged. Other channels are unchanged. This changes local settings without provider dispatch or a new review.", recovery}
		out.Example = "<bound launcher> reviews rounds grant --pr 42 --channel hosted --expected-revision 1 --min 2 --max 3 --key hosted-42-grant"
		out.Documentation = "docs/review-round-controls.md"
	case "reviews rounds close":
		out.Usage = "<bound launcher> reviews rounds close --pr NUMBER --channel cli|hosted --expected-revision N --key KEY"
		out.Required = []string{"--pr NUMBER (canonical positive PR number, 1-2147483647)", "--channel cli|hosted", "--expected-revision N (current round-control revision)", "--key KEY"}
		out.Semantics = []string{"Project writers can close this PR/channel directly. Read reviews rounds read for the current --expected-revision. No --min, --max or --exact is accepted.", "Stops new dispatch, including queued/prepared work at its final guard. Does not cancel a possibly posted action: its result can still be captured, adjudicated and counted once.", "Preserves earned dry streak, completed counts and history; other channels are unchanged. A later explicit grant reopens this channel. This local mutation performs no provider dispatch.", recovery}
		out.Example = "<bound launcher> reviews rounds close --pr 42 --channel cli --expected-revision 2 --key cli-42-close"
		out.Documentation = "docs/review-round-controls.md"
	case "reviews rounds read":
		out.Usage = "<bound launcher> reviews rounds read --pr NUMBER --channel cli|hosted"
		out.Required = []string{"--pr NUMBER (canonical positive PR number, 1-2147483647)", "--channel cli|hosted"}
		out.Semantics = []string{"Reads current local settings and credited completion counters for one PR/channel without provider work or writes. configured:false and revision zero explicitly mean no retained grant.", "Returns minimum, maximum, used, dry, taper, tapered, closed and revision. These counters are separate from historical run totals.", "For a finite maximum, maximum minus used is remaining completed-round capacity before in-flight work. maximum_unbounded:true has no finite remainder. Taper, closure, pending finalization and other admission requirements can still prevent another start; this read grants no permission."}
		out.Example = "<bound launcher> reviews rounds read --pr 42 --channel cli"
		out.Documentation = "docs/review-round-controls.md"
	case "reviews queue":
		out.Usage = "<bound launcher> reviews queue [--active] [--limit N] [--cursor CURSOR]"
		out.Optional = []string{"--active (open, nonretired PRs only, filtered before pagination; compact scalar summaries)", "--limit N (1-100, default 5)", "--cursor CURSOR (continue the returned retained-snapshot page with the same view)"}
		out.Semantics = []string{"Reads one ordered retained queue page without provider work or automatic pagination. Default output preserves merged/closed history and its full queue detail; --active selects only open, nonretired entries before applying the page limit.", "--active returns scalar PR summaries rather than nested round-control history. The default 5 is the retained page size, not channel-front selection. Continue has_more with the returned cursor and same view.", "Optional review_size supplies configured sizing preferences, observed file count, headroom and historical completed-review context. Suggested maximum warnings never require acknowledgement or block review. Accepts no positional PR number or --snapshot flag. Queue entries alone do not establish review coverage, current CI or merge readiness."}
		out.Example = "<bound launcher> reviews queue --active --limit 20"
		out.Documentation = "docs/review-workspace.md"
	case "reviews evidence", "reviews adjudication":
		out.Usage = "<bound launcher> " + operation + " EVIDENCE_ID"
		out.Required = []string{evidenceID}
		out.Semantics = []string{"Reads retained data for one native evidence ID in the bound project without provider work, new reviews, writes or automatic finalization. Use the evidence ID returned by its original operation; a run, refresh or attempt ID is a different identity."}
		if operation == "reviews evidence" {
			out.Semantics = append(out.Semantics, "Returns the original captured comparison, completeness, outcome and finding text/indexes. Capture completion does not mean its findings are accepted, fixed or finalized; read reviews adjudication EVIDENCE_ID for current decisions and revision.")
		} else {
			out.Semantics = append(out.Semantics, "Returns the current adjudication revision, dispositions, reasons, fix proof, finalized flag and any frozen original counts. Read reviews evidence EVIDENCE_ID for original finding text; use this returned revision for guarded decision, fix or finalization edits.")
		}
		out.Example = "<bound launcher> " + operation + " review_evidence_0123456789abcdef0123456789abcdef"
		out.Documentation = "docs/review-controls.md"
	case "reviews route-create":
		out.Usage = "<bound launcher> reviews route-create EVIDENCE_ID --finding INDEX --target-pr NUMBER --expect-revision N --reason TEXT --key KEY [--expect-route-revision N]"
		out.Required = []string{evidenceID, "--finding INDEX (zero-based, 0-99)", "--target-pr NUMBER (different enrolled PR in the same project and repository)", "--expect-revision N (current source adjudication revision, initially 0)", "--reason TEXT (nonblank, up to 2048 UTF-8 bytes)", "--key KEY"}
		out.Optional = []string{"--expect-route-revision N (current receiving-route revision when rerouting; omitted means 0 for an initial route)"}
		out.Semantics = []string{"Read reviews evidence EVIDENCE_ID and reviews adjudication EVIDENCE_ID for the original finding and current source revision. Routes target an actual different enrolled PR, never a worker or job. A job/note/message handoff alone does not settle the source finding.", "Creates a route and changes the source disposition to routed. Returns the new route id/revision and source_revision; use the updated source revision for subsequent source edits. Source and receiving obligations stay pending until the receiving decision/fix satisfies them.", "Rerouting requires the existing route revision and preserves superseded history. Original evidence and finalized counts stay unchanged; a route with fix proof cannot be silently superseded. No provider work, review request, implicit message or owner acceptance.", recovery}
		out.Example = "<bound launcher> reviews route-create review_evidence_0123456789abcdef0123456789abcdef --finding 0 --target-pr 84 --expect-revision 2 --reason 'Owned by the receiving PR' --key route-create-1"
		out.Documentation = "docs/review-routes.md"
	case "reviews route":
		out.Usage = "<bound launcher> reviews route ROUTE_ID"
		out.Required = []string{"ROUTE_ID (review_route_ followed by exactly 32 lowercase hexadecimal characters)"}
		out.Semantics = []string{"Reads one same-project route, receiving revision/disposition/fix proof and original source anchor/finding text. Explicit reads retain superseded history; superseded routes cannot receive new decisions or fixes.", "Provider-free read with no writes, acceptance, source refresh or review request. Use the route revision for route-decide or route-fix; source changes use the separate source adjudication revision."}
		out.Example = "<bound launcher> reviews route review_route_0123456789abcdef0123456789abcdef"
		out.Documentation = "docs/review-routes.md"
	case "reviews routes":
		out.Usage = "<bound launcher> reviews routes --pr NUMBER [--limit N] [--cursor CURSOR]"
		out.Required = []string{"--pr NUMBER (receiving PR, canonical positive number)"}
		out.Optional = []string{"--limit N (1-100, default 5)", "--cursor CURSOR (continue the same receiving PR)"}
		out.Semantics = []string{"Lists active incoming routes for this receiving PR, with compact source identities and receiving state. Full finding text and superseded history require reviews route ROUTE_ID.", "Reads one scoped page with cursor and has_more, without automatic pagination, provider work, writes or new reviews. Accepts no positional PR, --snapshot or worker/job target selector."}
		out.Example = "<bound launcher> reviews routes --pr 84 --limit 5"
		out.Documentation = "docs/review-routes.md"
	case "reviews route-decide":
		out.Usage = "<bound launcher> reviews route-decide ROUTE_ID --expect-revision N --disposition accepted|rejected --reason TEXT --key KEY"
		out.Required = []string{"ROUTE_ID (review_route_ followed by 32 lowercase hexadecimal characters)", "--expect-revision N (current receiving-route revision, at least 1)", "--disposition accepted|rejected", "--reason TEXT (nonblank, up to 2048 UTF-8 bytes)", "--key KEY"}
		out.Semantics = []string{"Read reviews route ROUTE_ID first. Records the receiving decision using that route's revision, not the source adjudication revision. Acceptance without fix proof remains pending; rejection records its reason without inventing a fix.", "Preserves original source evidence and frozen counts. Superseded routes and proof-discarding changes are refused. No provider work, source decision rewrite or implicit review request.", recovery}
		out.Example = "<bound launcher> reviews route-decide review_route_0123456789abcdef0123456789abcdef --expect-revision 1 --disposition accepted --reason 'Verified receiving scope' --key route-decision-1"
		out.Documentation = "docs/review-routes.md"
	case "reviews route-fix":
		out.Usage = "<bound launcher> reviews route-fix ROUTE_ID --expect-revision N --expect-sha SHA --fix-sha SHA --proof TEXT --key KEY"
		out.Required = []string{"ROUTE_ID (review_route_ followed by 32 lowercase hexadecimal characters)", "--expect-revision N (current receiving-route revision, at least 1)", "--expect-sha SHA (use --expect-sha= when no fix exists)", "--fix-sha SHA (40 lowercase hexadecimal characters)", "--proof TEXT (nonblank, up to 2048 UTF-8 bytes)", "--key KEY"}
		out.Semantics = []string{"Read reviews route ROUTE_ID for its current receiving revision and fix SHA. Records verified fix proof only for an active accepted route. Empty --expect-sha= asserts no prior fix; correction requires the exact current SHA.", "Preserves original source evidence, attribution and frozen counts while updating the receiving obligation. No provider work, new review, source finalization or allowance change.", recovery}
		out.Example = "<bound launcher> reviews route-fix review_route_0123456789abcdef0123456789abcdef --expect-revision 2 --expect-sha= --fix-sha 0123456789abcdef0123456789abcdef01234567 --proof 'Verified published fix' --key route-fix-1"
		out.Documentation = "docs/review-routes.md"
	case "reviews assess":
		out.Usage = "<bound launcher> reviews assess EVIDENCE_ID"
		out.Required = []string{evidenceID}
		out.Semantics = []string{"Applies current project policy and local finding obligations to the PR identified by native evidence; its original captured comparison is retained.", "sufficient describes completed PR/channel review progress, preserved across later Git changes. It does not verify current GitHub source, required CI or unresolved provider threads, or declare merge readiness.", "Reads retained facts without provider work or writes. Use reviews source-assess --pr NUMBER for source policy or reviews readiness --pr NUMBER for the combined retained assessment."}
		out.Example = "<bound launcher> reviews assess review_evidence_0123456789abcdef0123456789abcdef"
		out.Documentation = "docs/review-controls.md"
	case "reviews source-assess":
		out.Usage = "<bound launcher> reviews source-assess --pr NUMBER"
		out.Required = []string{"--pr NUMBER (canonical positive PR number, 1-2147483647)"}
		out.Semantics = []string{"Applies current local review policy, completed PR/channel review progress and finding obligations for the selected PR using its latest retained source identity.", "Source observation and policy evaluation times differ. This result does not establish current GitHub freshness or assess required CI, provider threads or merge-state gates; reviews readiness --pr NUMBER combines those retained components.", "Reads stored facts without provider work or writes. Use reviews source-refresh --help for explicit external collection."}
		out.Example = "<bound launcher> reviews source-assess --pr 42"
		out.Documentation = "docs/review-source.md"
	case "reviews ci-assess":
		out.Usage = "<bound launcher> reviews ci-assess --pr NUMBER [--diagnostics]"
		out.Optional = []string{"--diagnostics (retained required contexts and selected CheckRun/Commit Status identities; no GitHub calls)"}
		out.Required = []string{"--pr NUMBER (canonical positive PR number, 1-2147483647)"}
		out.Semantics = []string{"Assesses retained required CI for the latest stored source comparison and current local repository, configuration and capability binding.", "Reports satisfied, failed, pending, unproven or not required. not_required means the selected base branch has no required checks; optional workflows may still be running or failing and are not assessed here. Review-channel policy, findings and provider thread/merge-state gates remain separate; reviews readiness --pr NUMBER combines the retained assessments.", "Reads stored facts without provider work or writes. Use reviews source-refresh --help for explicit CI or readiness collection."}
		out.Example = "<bound launcher> reviews ci-assess --pr 42"
		out.Documentation = "docs/review-ci.md"
	case "reviews readiness":
		out.Usage = "<bound launcher> reviews readiness --pr NUMBER"
		out.Required = []string{"--pr NUMBER (canonical positive PR number, 1-2147483647)"}
		out.Semantics = []string{"Combines current local policy, finalized PR/channel review progress, finding/route obligations, retained required CI, thread resolution and provider merge-state facts for the latest stored comparison.", "Required CI describes the selected base branch, not all workflow health. not_required does not mean workflows passed or stopped; GitHub merge-state gates remain separate. ready_as_observed describes the retained observation times. Missing, changed or conflicting input bindings remain held; it does not authorize a merge or guarantee GitHub remains unchanged.", "Reads stored facts without provider work or writes. Use reviews source-refresh --help to explicitly collect readiness with --readiness."}
		out.Example = "<bound launcher> reviews readiness --pr 42"
		out.Documentation = "docs/review-readiness.md"
	case "reviews runs":
		out.Usage = "<bound launcher> reviews runs NUMBER [--snapshot SNAPSHOT_ID] [--limit N] [--cursor CURSOR]"
		out.Required = []string{"NUMBER (canonical positive PR number)"}
		out.Optional = []string{"--snapshot SNAPSHOT_ID (retained snapshot; omission reads the current stored workspace)", "--limit N (1-100, default 5)", "--cursor CURSOR (continue the same scoped snapshot)"}
		out.Semantics = []string{"Reads one newest-first stored run-summary page without full findings, automatic pagination, provider refresh or new review.", "CLI/MCP omit unrelated workspace-origin source, observed_at and recorded_at fields; the API retains them. Each item's started_at/finished_at describes that review's capture window, and item source identifies its original producer. Snapshot identity, revision and continuation remain intact.", "Use reviews run RUN_ID for full retained findings and original native operation/evidence anchors for execution status and applicability. Run timestamps alone do not establish current GitHub freshness or coverage."}
		out.Example = "<bound launcher> reviews runs 42 --limit 5"
		out.Documentation = "docs/review-workspace.md"
	case "reviews decide":
		out.Usage = "<bound launcher> reviews decide EVIDENCE_ID --key KEY --expect-revision N --finding INDEX --disposition accepted|rejected --reason TEXT [--expect-route-revision N]"
		out.Required = []string{"EVIDENCE_ID", "--key KEY", "--expect-revision N (current adjudication revision, initially 0)", "--finding INDEX (zero-based, 0-99)", "--disposition accepted|rejected", "--reason TEXT (nonblank, up to 2048 UTF-8 bytes)"}
		out.Optional = []string{"--expect-route-revision N (current route revision when replacing a routed decision)"}
		out.Semantics = []string{"Read reviews evidence EVIDENCE_ID for finding text and reviews adjudication EVIDENCE_ID for current revision and dispositions. Use the returned revision for each subsequent edit.", "accepted means the finding is useful work; rejected records why it is not accepted. Create routed findings with reviews route-create, not --disposition routed. An accepted finding with fix proof cannot be silently rejected.", "Decisions retain captured text and original finalized counts; no provider work, new review, publication or allowance change occurs.", recovery}
		out.Example = "<bound launcher> reviews decide review_evidence_0123456789abcdef0123456789abcdef --key decision-1 --expect-revision 0 --finding 0 --disposition accepted --reason 'Verified against current source'"
		out.Documentation = "docs/review-controls.md"
	case "reviews finalize":
		out.Usage = "<bound launcher> reviews finalize EVIDENCE_ID --expect-revision N --key KEY"
		out.Required = []string{"EVIDENCE_ID", "--expect-revision N (current adjudication revision)", "--key KEY"}
		out.Semantics = []string{"Requires a completed attempt, complete retained capture and a decision for every finding; a complete zero-finding review is valid. Read reviews adjudication EVIDENCE_ID for the current revision.", "Freezes original found/accepted/routed source counts. A finalized review can contain accepted findings awaiting fixes; finalization is not a clean-review or merge-readiness verdict.", "Finalization automatically queues the GitHub summary in the same transaction, without waiting for fixes or GitHub. No separate reviews publish call is needed. Later decision/fix corrections preserve frozen source counts; no source refresh or new review is requested.", recovery}
		out.Example = "<bound launcher> reviews finalize review_evidence_0123456789abcdef0123456789abcdef --expect-revision 2 --key finalize-1"
		out.Documentation = "docs/review-controls.md"
	case "reviews decide-batch", "reviews fix-batch":
		out.Usage = "<bound launcher> " + operation + " EVIDENCE_ID --file PATH --key KEY"
		out.Required = []string{"EVIDENCE_ID", "--file PATH (private regular UTF-8 JSON file, at most 2 MiB)", "--key KEY"}
		out.Semantics = []string{"Validates the complete strict file before loading profiles or contacting the service. One expected_revision and 1-100 explicitly selected unique zero-based finding indexes (0-99) are required; unknown, duplicate and null required fields are refused.", "Applies all items atomically, including browser projection, audit/event history and one compact receipt. A bad later item or stale guard rolls back the complete batch. No per-item commits, provider work, automatic retries or implicit finalization.", "Read reviews evidence EVIDENCE_ID for original findings and reviews adjudication EVIDENCE_ID for the current revision, dispositions and fix SHAs. Immutable source and finalized counts are retained.", recovery}
		out.FileFields = map[string]string{"expected_revision": "Current adjudication revision, initially 0", "items": "1-100 explicit unique finding objects; unselected findings stay unchanged"}
		if operation == "reviews decide-batch" {
			out.FileExample = map[string]any{"expected_revision": 0, "items": []any{map[string]any{"finding": 0, "disposition": "accepted", "reason": "Verified against source and focused proof"}}}
			out.Semantics = append(out.Semantics, "Each item requires finding, disposition accepted|rejected and its own nonblank reason (at most 2048 UTF-8 bytes). Optional expected_route_revision guards a routed source correction; receiving fix authority remains separate. No accept-all selection or shared default reason.")
		} else {
			out.FileExample = map[string]any{"expected_revision": 1, "items": []any{map[string]any{"finding": 0, "expected_sha": "", "fix_sha": "0123456789abcdef0123456789abcdef01234567", "proof_note": "Verified published fix and focused test"}}}
			out.Semantics = append(out.Semantics, "Each item requires finding, expected_sha (the currently stored fix_sha, or an explicit empty string for the first fix; not the PR head or original reviewed SHA), fix_sha (40 lowercase hexadecimal characters) and its own nonblank proof_note (at most 2048 UTF-8 bytes). Only accepted source findings can receive proof; receiving routed fixes use route controls.")
		}
		out.Example = "<bound launcher> " + operation + " review_evidence_0123456789abcdef0123456789abcdef --file /private/selected-findings.json --key selected-batch-1"
		out.Documentation = "docs/review-controls.md"
	case "reviews publish":
		out.Usage = "<bound launcher> reviews publish EVIDENCE_ID --expect-revision N --key KEY [--wait-seconds N]"
		out.Required = []string{"EVIDENCE_ID", "--expect-revision N (current source adjudication revision)", "--key KEY"}
		out.Optional = []string{"--wait-seconds N (0-900, default 2)"}
		out.Semantics = []string{"Explicitly requests a GitHub checkpoint comment for complete finalized native evidence, subject to existing publication authority. Uses original frozen counts and captured head; it does not publish finding bodies or current fix proof.", "A pending receipt is not confirmation. Read reviews publication PUBLICATION_ID to recover stored state without provider work. Publication does not request a review, refresh source or change allowances; finalization already queues delivery, so routine review work needs no reviews publish call.", recovery}
		out.Example = "<bound launcher> reviews publish review_evidence_0123456789abcdef0123456789abcdef --expect-revision 3 --key checkpoint-1 --wait-seconds 0"
		out.Documentation = "docs/review-publication.md"
	case "reviews publication":
		out.Usage = "<bound launcher> reviews publication PUBLICATION_ID"
		out.Required = []string{"PUBLICATION_ID"}
		out.Semantics = []string{"Reads one retained checkpoint publication, including its state, revision and confirmed comment link, without provider work or retrying publication.", "This is the CLI equivalent of MCP review_checkpoint_read. Use reviews publication-recover only when deliberate recovery is needed."}
		out.Example = "<bound launcher> reviews publication review_publication_0123456789abcdef0123456789abcdef"
		out.Documentation = "docs/review-publication.md"
	case "reviews publication-recover":
		out.Usage = "<bound launcher> reviews publication-recover PUBLICATION_ID --expect-revision N --key KEY [--retry-unposted] [--wait-seconds N]"
		out.Required = []string{"PUBLICATION_ID", "--expect-revision N (current publication revision)", "--key KEY"}
		out.Optional = []string{"--retry-unposted (explicitly permit retry after proven nonpublication)", "--wait-seconds N (0-900, default 2)"}
		out.Semantics = []string{"Explicitly recovers the original checkpoint publication under its current revision; recovery may read GitHub. Read reviews publication PUBLICATION_ID first for the retained state and revision.", "Does not request another review or change its allowance. Retain the original publication identity; do not blindly publish another comment.", recovery}
		out.Example = "<bound launcher> reviews publication-recover review_publication_0123456789abcdef0123456789abcdef --expect-revision 2 --key recover-checkpoint-1"
		out.Documentation = "docs/review-publication.md"
	case "reviews queue-add", "reviews queue-move":
		out.Usage = "<bound launcher> " + operation + " PR --expect-config N --key KEY [destination]"
		out.Required = []string{"PR", "--expect-config N (read reviews execution)", "--key KEY"}
		out.Optional = []string{"--before PR | --after PR | --first | --last"}
		out.Semantics = []string{"Project write access; changes one queue entry without replacing provider, mode, channels or pause settings. Adding without a destination appends; moving requires one destination.", "Updates website enrollment/order in the same transaction. Existing allowances/taper/history stay unchanged; new PR/channel pairs receive product defaults. No provider call or review request. A no-op move retains its configuration revision. Enqueue may include review_size with stored file count, independently configured sizing settings, headroom and historical completed-review context. A suggested-size warning never refuses enrollment or needs acknowledgement.", recovery}
		out.Example = "<bound launcher> reviews queue-add 42 --after 41 --expect-config 3 --key queue-add-42"
		out.Documentation = "docs/review-execution.md"
	case "reviews execution":
		out.Usage = "<bound launcher> reviews execution"
		out.Semantics = []string{"Reads the current stored review execution configuration and revision, without provider work or writes.", "Use this revision for an explicit source-refresh or review-request guard. A stale guard refuses before queuing work; read current configuration and submit only the intended operation with a new key."}
		out.Documentation = "docs/review-execution.md"
	case "reviews source-operation":
		out.Usage = "<bound launcher> reviews source-operation OPERATION_ID"
		out.Required = []string{"OPERATION_ID"}
		out.Semantics = []string{"Reads one retained source-refresh operation without starting another GitHub refresh or review.", "Use the complete original review_refresh_ ID from reviews source-refresh, including its prefix and 32 lowercase hexadecimal characters. Review-execution operations are read with reviews operation instead."}
		out.Example = "<bound launcher> reviews source-operation review_refresh_0123456789abcdef0123456789abcdef"
		out.Documentation = "docs/review-source.md"
	case "reviews fix":
		out.Usage = "<bound launcher> reviews fix EVIDENCE_ID --key KEY --expect-revision N --finding INDEX --expect-sha SHA --fix-sha SHA --proof TEXT"
		out.Required = []string{"EVIDENCE_ID", "--key KEY", "--expect-revision N", "--finding INDEX (zero-based, 0-99)", "--expect-sha SHA (use --expect-sha= when no fix exists)", "--fix-sha SHA (40 lowercase hexadecimal characters)", "--proof TEXT (nonblank, up to 2048 UTF-8 bytes)"}
		out.Semantics = []string{"Read reviews adjudication EVIDENCE_ID for the current revision and finding fix SHA. Record a verified published fix for an accepted finding; use the returned revision for the next edit. Existing source finalization does not prevent fix proof.", "Empty --expect-sha= asserts no fix is recorded; replacing proof requires its exact current SHA. Retains original finding text, disposition, finalized counts and history. No provider work, source refresh, new review or allowance change.", recovery}
		out.Example = "<bound launcher> reviews fix review_evidence_0123456789abcdef0123456789abcdef --key fix-1 --expect-revision 4 --finding 0 --expect-sha= --fix-sha 0123456789abcdef0123456789abcdef01234567 --proof 'Verified published fix and focused test'"
		out.Documentation = "docs/review-controls.md"
	case "reviews operations":
		out.Usage = "<bound launcher> reviews operations [--pr NUMBER] [--channel cli|hosted] [--all] [--limit N] [--cursor CURSOR]"
		out.Optional = []string{"--pr NUMBER (positive PR number)", "--channel cli|hosted", "--all (include terminal and held history)", "--limit N (1-100, default 5)", "--cursor CURSOR (continue with the same filters)"}
		out.Semantics = []string{"Reads one compact newest-first page of retained project operations without provider work or automatic continuation. Defaults to pending queued/preparing/posting/active/uncertain operations and held operations retaining an execution reservation.", "--all recovers lost operation receipts from history. Mutable state is evaluated on each page; pagination does not promise a historical snapshot. Select reviews operation OPERATION_ID for detail and recovery; listing never requests or retries a review."}
		out.Example = "<bound launcher> reviews operations --pr 42 --all --limit 5"
		out.Documentation = "docs/review-operations.md"
	case "reviews operation":
		out.Usage = "<bound launcher> reviews operation OPERATION_ID [--wait-seconds N] [--diagnostics]"
		out.Required = []string{"OPERATION_ID"}
		out.Optional = []string{"--wait-seconds N (0-900, default 0)", "--diagnostics (full retained operation, configuration/attempt and capture diagnostics)"}
		out.Semantics = []string{"Default output is compact status: stage, reason, original operation/attempt IDs, discoverable evidence_id and next_action. With evidence, review_finished_at dates review finish and evidence_recorded_at dates controller retention; updated_at remains the operation transition. --diagnostics selects the full retained operation. Both are local reads without provider work, retries or requesting a review; bounded wait behavior is unchanged.", "Hosted hosted_observation_pending returns next_action wait_for_observation: the original trigger is issued and scheduled observation owns its result. Continue other work and check later; do not repost or immediately poll. An uncertain state ends the bounded result wait immediately. CLI capture_retention_failed does not prove output was lost; capture_publication_pending identifies a retained result awaiting browser publication. The local observer can finish complete retained CLI evidence without rerunning the process.", "Use reviews runs PR and reviews run RUN_ID for published history; reviews evidence EVIDENCE_ID reads known native evidence. There is no reviews attempt command. If uncertain work has no discoverable result, report the original operation/attempt IDs to the maintainer; do not invent evidence IDs or start a replacement."}
		out.Example = "<bound launcher> reviews operation review_operation_0123456789abcdef0123456789abcdef --wait-seconds 40"
		out.Documentation = "docs/review-execution.md"
	case "reviews request":
		out.Usage = "<bound launcher> reviews request NUMBER --expect-config N --channel cli|hosted --head SHA --key KEY [options]"
		out.Required = []string{"NUMBER", "--expect-config N", "--channel cli|hosted", "--head SHA", "--key KEY"}
		out.Optional = []string{"--wait-seconds N (0-900, default 2)", "--out-of-order", "--override-file-limit"}
		out.Semantics = []string{"Explicitly requests one review against the asserted current target and configuration; requires existing project authority. Out-of-order skips queue scheduling only, preserving target source, findings, limits and provider checks.", "--override-file-limit skips only the optional project file ceiling for this exact request. Source/path validation, provider restrictions, findings and allowances still apply. The operation retains the choice; changing it under the same key conflicts. A pending receipt is not completion. Read the retained operation by ID; a held operation does not automatically retry or establish review credit.", "Optional review_size uses a stored count only for a matching observed head; otherwise the count and derived headroom are unknown. Admission checks the materialized comparison, not this preview. No extra source fetch is needed to fill it.", "Hosted hosted_allowance_reply_pending is a terminal preparation hold with no review trigger. For the still-authorized planned review, a later fresh key/current configuration/head rechecks fresh allowance through normal preflight. Respect known cooldowns; no manual GitHub repair or round reset is required. A late available reply cannot resume the old operation or authorize a review.", recovery}
		out.Example = "<bound launcher> reviews request 42 --expect-config 3 --channel hosted --head 0123456789abcdef0123456789abcdef01234567 --key review-42-hosted"
		out.Documentation = "docs/review-execution.md"
	case "reviews start":
		out.Usage = "<bound launcher> reviews start --channel cli|hosted --key KEY [--expect-pr NUMBER] [--wait-seconds N]"
		out.Required = []string{"--channel cli|hosted", "--key KEY"}
		out.Optional = []string{"--expect-pr NUMBER", "--wait-seconds N (0-900, default 2)"}
		out.Semantics = []string{"Selects the channel front and requests at most one review. An expected PR is an assertion, not permission to redirect selection. Does not accept --out-of-order or --override-file-limit.", "Retain the selection receipt and follow its original operation link; a pending receipt does not mean a review completed.", recovery}
		out.Example = "<bound launcher> reviews start --channel hosted --expect-pr 42 --key next-hosted-review"
		out.Documentation = "docs/review-round-controls.md"
	case "reviews obligations":
		out.Usage = "<bound launcher> reviews obligations list --pr NUMBER [--status open|resolved] [--limit N] [--cursor CURSOR] | read OBLIGATION_ID --pr NUMBER"
		out.Semantics = []string{"Reads carried unfinished findings, routes and unsettled review results. They are retained obligations, not newly verified current code defects. Open obligations remain pending until deliberately resolved with evidence.", "Project writers can resolve an obligation using its current revision and an explicit key; imports require administrator access. No provider refresh or review request is performed.", recovery}
		out.Example = "<bound launcher> reviews obligations list --pr 3251 --status open"
		out.Documentation = "docs/adoption-obligations.md"
	case "reviews source-refresh":
		out.Usage = "<bound launcher> reviews source-refresh --pr NUMBER --head SHA --expect-config N --key KEY [--ci] [--readiness] [--wait-seconds N]"
		out.Semantics = []string{"Explicitly observe the exact comparison; does not request a review. Routine queue freshness is maintained by the automatic observer, not by this command. --ci and --readiness assess required checks for the selected base branch, not optional workflow health.", "Held default_base_changed means there is no selected unmerged parent but the PR targets a nondefault branch. Check and reconcile the PR base with its intended default branch, then refresh the new comparison. Do not change worker credentials for a comparison hold.", recovery}
		out.Example = "<bound launcher> reviews source-operation REFRESH_ID"
		out.Documentation = "docs/review-source.md"
	case "jobs checklist-set":
		out.Usage = "<bound launcher> jobs checklist-set JOB_ID --expect-revision N --expect-checklist-revision N --items JSON|--items-file PATH --key KEY"
		out.Required = []string{"JOB_ID", "--expect-revision N (current job revision)", "--expect-checklist-revision N (0 for absent checklist)", "exactly one of --items JSON or --items-file PATH", "--key KEY"}
		out.Semantics = []string{"Replace the complete ordered checklist when restructuring; each item has id, text and done. Empty [] clears it. Up to 100 items; existing IDs identify retained steps.", "Both job and checklist guards are required. This does not rewrite brief/progress or advance the job revision. Use checklist-item for one completion/reopen instead.", recovery}
		out.Example = "<bound launcher> jobs checklist-set JOB_ID --expect-revision 3 --expect-checklist-revision 0 --items-file /private/items.json --key checklist-plan-1"
		out.FileFields = map[string]string{"--items-file": "The file itself is a JSON array of {id,text,done} items, not an object wrapper."}
		out.Documentation = "docs/job-progress.md"
	case "jobs checklist-item":
		out.Usage = "<bound launcher> jobs checklist-item JOB_ID ITEM_ID --done true|false --expect-checklist-revision N --key KEY"
		out.Required = []string{"JOB_ID", "ITEM_ID", "--done true|false", "--expect-checklist-revision N", "--key KEY"}
		out.Semantics = []string{"Set only one existing item's completion. Use checklist revision from the current job read; no job revision or full checklist input is required. Other items, text and order remain unchanged.", "Unrelated job progress does not invalidate the checklist guard. A stale checklist revision is refused even if the boolean already matches; a current-revision no-op adds no history.", recovery}
		out.Example = "<bound launcher> jobs checklist-item JOB_ID verify --done true --expect-checklist-revision 2 --key checklist-verify-1"
		out.Documentation = "docs/job-progress.md"
	case "jobs create":
		out.Usage = "<bound launcher> jobs create --title TEXT --key KEY [options]"
		out.Required = []string{"--title TEXT", "--key KEY"}
		out.Optional = []string{"--brief TEXT | --brief-file PATH", "--worker WORKER_ID", "--status active|blocked|parked|completed"}
		out.Semantics = []string{"Brief files contain exact UTF-8 text, at most 32 KiB; empty is allowed. Use a regular file with owner-private POSIX permissions. Inline and file forms are mutually exclusive.", recovery}
		out.Example = "<bound launcher> jobs create --title 'Implement transport' --brief-file /private/brief.md --key job-create-1"
		out.Documentation = "docs/job-state.md"
	case "jobs revision":
		out.Usage = "<bound launcher> jobs revision JOB_ID REVISION"
		out.Required = []string{"JOB_ID", "REVISION (positive integer identifying one retained native job state)"}
		out.Semantics = []string{"Reads exactly one immutable history entry: revision, actor_id, original state including full brief, and created_at. No pagination, current notes/checklist context, provider work or writes.", "A missing retained revision returns not_found. No fallback to today's job, nearest revision or unimported source history. Use jobs read JOB_ID for current standing instructions."}
		out.Example = "<bound launcher> jobs revision job_0123456789abcdef0123456789abcdef 193"
		out.Documentation = "docs/job-state.md"
	case "jobs read":
		out.Usage = "<bound launcher> jobs read JOB_ID"
		out.Required = []string{"JOB_ID"}
		out.Semantics = []string{"Reads the full current brief/progress, remaining checklist and bounded pending job-note previews. Does not read worker inbox or job archives.", "Use jobs revision JOB_ID REVISION for a known historical full state. Use jobs history for one explicit oldest-first archive page; progress-history contains progress only."}
		out.Example = "<bound launcher> jobs read job_0123456789abcdef0123456789abcdef"
		out.Documentation = "docs/job-state.md"
	case "inbox delivery":
		out.Usage = "<bound launcher> inbox delivery MESSAGE_ID"
		out.Required = []string{"MESSAGE_ID"}
		out.Semantics = []string{"Reads one current recipient receipt (seen/acknowledged) under the bound project, without the message body or a different worker’s inbox. No writes or provider work.", "Discover outgoing handoffs with inbox sent; use --all there to include already handled history."}
		out.Example = "<bound launcher> inbox delivery msg_0123456789abcdef0123456789abcdef"
		out.Documentation = "docs/message-events.md"
	case "inbox read":
		out.Usage = "<bound launcher> inbox read MESSAGE_ID"
		out.Required = []string{"MESSAGE_ID"}
		out.Semantics = []string{"Reads one immutable message body and its own metadata under the bound project/worker. No automatic conversation expansion, seen transition or acknowledgement.", "Find full IDs through inbox list; use inbox thread CONVERSATION_ID for deliberate surrounding context. Reads perform no provider work or writes."}
		out.Example = "<bound launcher> inbox read msg_0123456789abcdef0123456789abcdef"
		out.Documentation = "docs/development.md"
	case "inbox thread":
		out.Usage = "<bound launcher> inbox thread CONVERSATION_ID [--limit N] [--cursor CURSOR]"
		out.Required = []string{"CONVERSATION_ID"}
		out.Optional = []string{"--limit N (1-100, default 20)", "--cursor CURSOR"}
		out.Semantics = []string{"Reads one explicit conversation page containing full message bodies. Check has_more and use the returned cursor deliberately; no automatic pagination.", "Reading a thread never marks messages seen or acknowledged and performs no provider work or writes."}
		out.Example = "<bound launcher> inbox thread conv_0123456789abcdef0123456789abcdef --limit 20"
		out.Documentation = "docs/development.md"
	case "notes read", "notes revision":
		out.Usage = "<bound launcher> " + operation + " NOTE_ID"
		out.Required = []string{"NOTE_ID"}
		out.Semantics = []string{"Reads the selected full private note body and metadata. Does not consume or dismiss the note, expand histories or perform provider work."}
		out.Example = "<bound launcher> " + operation + " note_0123456789abcdef0123456789abcdef"
		if operation == "notes revision" {
			out.Usage += " REVISION"
			out.Required = append(out.Required, "REVISION (positive retained note revision)")
			out.Example += " 1"
			out.Semantics = append(out.Semantics, "Reads that immutable note revision, including recorded attribution; missing history never falls back to the current note.")
		}
		out.Documentation = "docs/notes.md"
	case "jobs status":
		out.Usage = "<bound launcher> jobs status JOB_ID"
		out.Required = []string{"JOB_ID"}
		out.Semantics = []string{"Reads compact current status, revision, worker and updated_at plus progress_summary (up to 1024 UTF-8 bytes), next and blocker (512 bytes each), explicit truncation flags and evidence_count. No evidence reference strings are included.", "Returns one coherent authorized snapshot without brief, checklist, notes, history, provider work or writes. Previews are status aids; use jobs read JOB_ID for complete working context before resuming."}
		out.Example = "<bound launcher> jobs status job_0123456789abcdef0123456789abcdef"
		out.Documentation = "docs/job-state.md"
	case "inbox ack", "inbox seen":
		out.Usage = "<bound launcher> " + operation + " MESSAGE_ID --key KEY"
		out.Required = []string{"MESSAGE_ID (full message ID returned by inbox list or inbox read)", "--key KEY"}
		out.Semantics = []string{"Only the bound actual recipient can change this message. Reading a message never changes seen or acknowledged state.", recovery}
		if operation == "inbox ack" {
			out.Semantics = append(out.Semantics, "Explicitly acknowledges the message and also establishes seen state. Acknowledgement is available through CLI and MCP inbox_ack.")
		} else {
			out.Semantics = append(out.Semantics, "Explicitly marks the message seen without acknowledging it. Seen messages remain pending until acknowledged.")
		}
		out.Example = "<bound launcher> " + operation + " msg_0123456789abcdef0123456789abcdef --key message-state-1"
		out.Documentation = "docs/development.md"
	case "inbox send":
		out.Usage = "<bound launcher> inbox send --to WORKER_ID (--body TEXT | --body-file PATH) --key KEY [--reply-to MESSAGE_ID]"
		out.Required = []string{"--to WORKER_ID", "--body TEXT | --body-file PATH", "--key KEY"}
		out.Optional = []string{"--reply-to MESSAGE_ID"}
		out.Semantics = []string{"The body must be nonblank UTF-8 and at most 16 KiB. Body files preserve exact text and must be regular with owner-private POSIX permissions. Inline and file forms are mutually exclusive.", "Send to a worker in this project. --reply-to retains the existing conversation; reads never mark a message seen or acknowledged.", recovery}
		out.Example = "<bound launcher> inbox send --to WORKER_ID --body-file /private/message.md --key message-1"
		out.Documentation = "docs/development.md"
	case "inbox wait":
		out.Usage = "<bound launcher> inbox wait --cursor EVENT_CURSOR [--limit N] [--wait-seconds N]"
		out.Required = []string{"--cursor EVENT_CURSOR (event_cursor from inbox snapshot or cursor from a prior successful wait; not inbox.cursor pagination)"}
		out.Optional = []string{"--limit N (1-100, default 20)", "--wait-seconds N (1-60, default 25)"}
		out.Semantics = []string{"One quiet bounded wait returns new incoming-message or job-assignment event metadata, or an empty timed_out result with a continuation cursor. Full message bodies require inbox read MESSAGE_ID; this is not a pending-inbox query.", "No automatic retry, reconnect, pagination or acknowledgment occurs; reads never acknowledge or mark messages seen. On error retain the saved cursor, then retry the wait or read the bound-worker inbox when the service is available."}
		out.Example = "<bound launcher> inbox wait --cursor saved-event-cursor --wait-seconds 25"
		out.Documentation = "docs/message-wait.md"
	case "inbox list", "inbox snapshot", "inbox sent":
		out.Usage = "<bound launcher> " + operation + " [--limit N] [--cursor CURSOR] [--unread] [--unacknowledged] [--newest] | <bound launcher> " + operation + " [--limit N] [--cursor CURSOR] [--newest] --all"
		out.Optional = []string{"--limit N (1-100)", "--cursor CURSOR (inbox pagination)", "--unread", "--unacknowledged (default)", "--newest (latest pending first, including seen-but-unhandled)", "--all (cannot combine with --unread or --unacknowledged)"}
		out.Semantics = []string{"Reads one compact inbox page for the bound worker. Defaults to pending, unacknowledged messages, including seen-but-unhandled messages; --unread narrows to unseen pending messages, not all pending work. --all includes handled history.", "Pages default to oldest-first insertion order. --newest reads newest first without hiding or acknowledging older work. Continue has_more with cursor and the same filters/order. inbox snapshot also returns event_cursor for later arrivals via inbox events or inbox wait; this is not an inbox pagination cursor.", "No provider work, automatic pagination, message seen update or acknowledgment occurs. Read selected full message bodies with inbox read MESSAGE_ID."}
		out.Example = "<bound launcher> " + operation + " --unread --limit 20"
		if operation == "inbox sent" {
			out.Semantics[0] = "Reads this bound sender’s outgoing handoffs, including recipient and current seen/acknowledged state. Defaults to pending; --all includes handled history. Read-only and never acknowledges for a recipient."
			out.Semantics[1] = "Continue with the returned outbox cursor and the same filters/order. Incoming and outgoing cursors are distinct. Use inbox delivery MESSAGE_ID for one exact receipt without reading message bodies."
		}
		if operation == "inbox snapshot" {
			out.Semantics[0] = "Reads one compact inbox page plus event_cursor from the same snapshot. Defaults to pending, unacknowledged messages, including seen-but-unhandled messages; --unread narrows to unseen pending messages, not all pending work. --all includes handled history."
			out.Semantics[1] = "Pages default to oldest-first insertion order. --newest reads newest first without hiding or acknowledging older work. Continue inbox.has_more with inbox.cursor and the same filters/order. event_cursor is a separate watermark for inbox events or inbox wait to discover later arrivals; it is not an inbox pagination cursor."
			out.Example = "<bound launcher> inbox snapshot --unacknowledged --limit 20"
		}
		out.Documentation = "docs/message-events.md"
	case "jobs update":
		out.Usage = "<bound launcher> jobs update JOB_ID --expect-revision N --key KEY [changes]"
		out.Required = []string{"JOB_ID", "--expect-revision N", "--key KEY", "at least one optional edit"}
		out.Optional = []string{"--title TEXT", "--brief TEXT | --brief-file PATH", "--worker WORKER_ID", "--status active|blocked|parked|completed", "--progress TEXT | --progress-file PATH", "--next TEXT | --next-file PATH", "--blocker TEXT | --blocker-file PATH", "--evidence REF (repeat up to 10)"}
		out.Semantics = []string{guard, "Empty --brief, --worker, --progress, --next or --blocker clears that field; title cannot be empty. A sole --evidence '' clears evidence; otherwise supplied evidence replaces the complete reference list. History is automatic.", "Text files preserve exact UTF-8; each conflicts with its matching inline flag. Use regular owner-private files. Brief: 32 KiB; progress/next: 8 KiB each; blocker: 4 KiB; combined progress including evidence: 16 KiB. Empty files clear valid fields.", recovery}
		out.Example = "<bound launcher> jobs update JOB_ID --expect-revision 3 --key job-next-1 --next 'Run focused validation'"
		out.Documentation = "docs/job-state.md"
	case "notes list":
		out.Usage = "<bound launcher> notes list [scope and filters] [--limit N] [--cursor CURSOR]"
		out.Optional = []string{"--worker WORKER_ID (select another worker in the bound project)", "--all-workers (project scope; exclusive with --worker and --direct-only)", "--direct-only (only notes naming the selected worker; omit assigned-job notes)", "--job JOB_ID", "--phase TEXT (exact label)", "--kind reminder|source", "--status pending|consumed|dismissed|all (default pending)", "--limit N (1-100, default 20)", "--cursor CURSOR (continue the same filters and scope)"}
		out.Semantics = []string{"Defaults to pending notes naming the bound worker or a job currently assigned to it. An explicit --job selects all notes for that exact job, independently of worker defaults or assignment. Phase, kind and status still filter within that job.", "Continue notes from jobs read with --job JOB_ID and its cursor; no --all-workers flag is needed. Plain worker discovery keeps its existing scope.", "Returns compact body_preview and body_truncated with cursor and has_more; no automatic pagination, full bodies, writes or provider work. Read selected full content with notes read NOTE_ID; listing never consumes notes."}
		out.Example = "<bound launcher> notes list --job JOB_ID --limit 20"
		out.Documentation = "docs/notes.md"
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
