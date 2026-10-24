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
	case "reviews help":
		out.Usage = "<bound launcher> reviews help"
		out.Optional = []string{"reviews operations --help", "reviews runs --help", "reviews decide --help", "reviews fix --help", "reviews finalize --help", "reviews publish --help", "reviews request --help", "reviews operation --help"}
		out.Semantics = []string{"Read retained findings with reviews evidence EVIDENCE_ID and current decisions/revision with reviews adjudication EVIDENCE_ID.", "decide, fix and finalize are guarded local evidence changes. publish explicitly requests a historical-count checkpoint comment on GitHub; request explicitly requests a review. Neither is implied by adjudication or help.", "Use the help for the selected operation; all help is local and reads no profile, credential or service. Documentation paths refer to the same installed release."}
		out.Example = "<bound launcher> reviews decide --help"
		out.Documentation = "docs/review-controls.md"
		out.Next = "docs/review-publication.md describes GitHub checkpoint publication; docs/review-execution.md describes review requests and retained operations."
	case "reviews runs":
		out.Usage = "<bound launcher> reviews runs NUMBER [--snapshot SNAPSHOT_ID] [--limit N] [--cursor CURSOR]"
		out.Required = []string{"NUMBER (canonical positive PR number)"}
		out.Optional = []string{"--snapshot SNAPSHOT_ID (retained snapshot; omission reads the current stored workspace)", "--limit N (1-100, default 5)", "--cursor CURSOR (continue the same scoped snapshot)"}
		out.Semantics = []string{"Reads one newest-first stored run-summary page without full findings, automatic pagination, provider refresh or new review.", "Top-level observed_at is the retained workspace source-observation time. Publishing one native run preserves it; it can be older than that run's finished_at and does not mean the run is stale.", "Top-level recorded_at is when that workspace snapshot was recorded, not when every PR was refreshed. Each item's started_at/finished_at describes that review's capture window; item source identifies its original producer, distinct from top-level source.", "Use reviews run RUN_ID for full retained findings and original native operation/evidence anchors for execution status and applicability. Neither envelope nor run timestamps alone establish current GitHub freshness or coverage."}
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
		out.Semantics = []string{"Requires a completed attempt, complete retained capture and a decision for every finding; a complete zero-finding review is valid. Read reviews adjudication EVIDENCE_ID for the current revision.", "Freezes original found/accepted/routed source counts. A finalized review can contain accepted findings awaiting fixes; finalization is not a clean-review or merge-readiness verdict.", "Later supported decision/fix corrections preserve frozen source counts. No GitHub comment, source refresh or new review is requested; reviews publish is separate.", recovery}
		out.Example = "<bound launcher> reviews finalize review_evidence_0123456789abcdef0123456789abcdef --expect-revision 2 --key finalize-1"
		out.Documentation = "docs/review-controls.md"
	case "reviews publish":
		out.Usage = "<bound launcher> reviews publish EVIDENCE_ID --expect-revision N --key KEY [--wait-seconds N]"
		out.Required = []string{"EVIDENCE_ID", "--expect-revision N (current source adjudication revision)", "--key KEY"}
		out.Optional = []string{"--wait-seconds N (0-900, default 2)"}
		out.Semantics = []string{"Explicitly requests a GitHub checkpoint comment for complete finalized native evidence, subject to existing publication authority. Uses original frozen counts and captured head; it does not publish finding bodies or current fix proof.", "A pending receipt is not confirmation. Read reviews publication PUBLICATION_ID to recover stored state without provider work. Publication does not request a review, refresh source or change allowances; local source finalization alone does not post a comment.", recovery}
		out.Example = "<bound launcher> reviews publish review_evidence_0123456789abcdef0123456789abcdef --expect-revision 3 --key checkpoint-1 --wait-seconds 0"
		out.Documentation = "docs/review-publication.md"
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
		out.Usage = "<bound launcher> reviews operation OPERATION_ID [--wait-seconds N]"
		out.Required = []string{"OPERATION_ID"}
		out.Optional = []string{"--wait-seconds N (0-900, default 0)"}
		out.Semantics = []string{"Reads the original retained operation without provider work, retrying or requesting a review. An uncertain result is not proof the external review failed or completed.", "Hosted hosted_observation_pending means the original review trigger was issued and scheduled observation owns its result. Do not repost. CLI capture_retention_failed does not prove output was lost; capture_publication_pending identifies a retained result awaiting browser publication. The local observer can finish complete retained CLI evidence without rerunning the process.", "Use reviews runs PR and reviews run RUN_ID for published history; reviews evidence EVIDENCE_ID reads known native evidence. There is no reviews attempt command. If uncertain work has no discoverable result, report the original operation/attempt IDs to the maintainer; do not invent evidence IDs or start a replacement."}
		out.Example = "<bound launcher> reviews operation review_operation_0123456789abcdef0123456789abcdef --wait-seconds 40"
		out.Documentation = "docs/review-execution.md"
	case "reviews request":
		out.Usage = "<bound launcher> reviews request NUMBER --expect-config N --channel cli|hosted --head SHA --key KEY [options]"
		out.Required = []string{"NUMBER", "--expect-config N", "--channel cli|hosted", "--head SHA", "--key KEY"}
		out.Optional = []string{"--wait-seconds N (0-900, default 2)", "--out-of-order"}
		out.Semantics = []string{"Explicitly requests one review against the asserted current target and configuration; requires existing project authority. Out-of-order skips queue scheduling only, preserving target source, findings, limits and provider checks.", "A pending receipt is not completion. Read the retained operation by ID; a held operation does not automatically retry or establish review credit.", recovery}
		out.Example = "<bound launcher> reviews request 42 --expect-config 3 --channel hosted --head 0123456789abcdef0123456789abcdef01234567 --key review-42-hosted"
		out.Documentation = "docs/review-execution.md"
	case "reviews start":
		out.Usage = "<bound launcher> reviews start --channel cli|hosted --key KEY [--expect-pr NUMBER] [--wait-seconds N]"
		out.Required = []string{"--channel cli|hosted", "--key KEY"}
		out.Optional = []string{"--expect-pr NUMBER", "--wait-seconds N (0-900, default 2)"}
		out.Semantics = []string{"Selects the channel front and requests at most one review. An expected PR is an assertion, not permission to redirect selection. Does not accept --out-of-order.", "Retain the selection receipt and follow its original operation link; a pending receipt does not mean a review completed.", recovery}
		out.Example = "<bound launcher> reviews start --channel hosted --expect-pr 42 --key next-hosted-review"
		out.Documentation = "docs/review-round-controls.md"
	case "reviews obligations":
		out.Usage = "<bound launcher> reviews obligations list --pr NUMBER [--status open|resolved] [--limit N] [--cursor CURSOR] | read OBLIGATION_ID --pr NUMBER"
		out.Semantics = []string{"Reads carried unfinished findings, routes and unsettled review results. They are retained obligations, not newly verified current code defects. Open obligations remain pending until deliberately resolved with evidence.", "Project writers can resolve an obligation using its current revision and an explicit key; imports require administrator access. No provider refresh or review request is performed.", recovery}
		out.Example = "<bound launcher> reviews obligations list --pr 3251 --status open"
		out.Documentation = "docs/adoption-obligations.md"
	case "reviews source-refresh":
		out.Usage = "<bound launcher> reviews source-refresh --pr NUMBER --head SHA --expect-config N --key KEY [--ci] [--readiness] [--wait-seconds N]"
		out.Semantics = []string{"Explicitly observe the exact comparison; does not request a review. Routine queue freshness is maintained by the automatic observer, not by this command.", "Held default_base_changed means there is no selected unmerged parent but the PR targets a nondefault branch. Check and reconcile the PR base with its intended default branch, then refresh the new comparison. Do not change worker credentials for a comparison hold.", recovery}
		out.Example = "<bound launcher> reviews source-operation REFRESH_ID"
		out.Documentation = "docs/review-source.md"
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
		out.Semantics = []string{"One quiet bounded wait returns new incoming-message event metadata, or an empty timed_out result with a continuation cursor. Full message bodies require inbox read MESSAGE_ID; this is not a pending-inbox query.", "No automatic retry, reconnect, pagination or acknowledgment occurs; reads never acknowledge or mark messages seen. On error retain the saved cursor, then retry the wait or read the bound-worker inbox when the service is available."}
		out.Example = "<bound launcher> inbox wait --cursor saved-event-cursor --wait-seconds 25"
		out.Documentation = "docs/message-wait.md"
	case "inbox list", "inbox snapshot":
		out.Usage = "<bound launcher> " + operation + " [--limit N] [--cursor CURSOR] [--unread] [--unacknowledged] | <bound launcher> " + operation + " [--limit N] [--cursor CURSOR] --all"
		out.Optional = []string{"--limit N (1-100)", "--cursor CURSOR (inbox pagination)", "--unread", "--unacknowledged (default)", "--all (cannot combine with --unread or --unacknowledged)"}
		out.Semantics = []string{"Reads one compact inbox page for the bound worker. Defaults to pending, unacknowledged messages, including seen-but-unhandled messages; --unread narrows to unseen pending messages, not all pending work. --all includes handled history.", "Pages use durable insertion order, oldest first; no recent-first option exists. Continue has_more with cursor and the same filters. inbox snapshot also returns event_cursor for later arrivals via inbox events or inbox wait; this is not an inbox pagination cursor.", "No provider work, automatic pagination, message seen update or acknowledgment occurs. Read selected full message bodies with inbox read MESSAGE_ID."}
		out.Example = "<bound launcher> " + operation + " --unread --limit 20"
		if operation == "inbox snapshot" {
			out.Semantics[0] = "Reads one compact inbox page plus event_cursor from the same snapshot. Defaults to pending, unacknowledged messages, including seen-but-unhandled messages; --unread narrows to unseen pending messages, not all pending work. --all includes handled history."
			out.Semantics[1] = "Pages use durable insertion order, oldest first; no recent-first option exists. Continue inbox.has_more with inbox.cursor and the same filters. event_cursor is a separate watermark for inbox events or inbox wait to discover later arrivals; it is not an inbox pagination cursor."
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
