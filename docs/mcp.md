# Local MCP Tools

## Implemented boundary

`fcmcp` exposes the tools listed below over local stdio using the official [Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk), pinned to v1.8.0. Tools call the shared typed HTTP client. The service owns authentication, project scope, permission checks, cursors and storage; the adapter never shells out to `fc`; explicit review requests authorize service-owned background provider work. Both CLI and MCP job/message and review-evidence mutations use the same typed client and API. [Unified job state](job-state.md) supplies one guarded update and one default working-context read; progress history is explicit. The [quiet message wait](message-wait.md) owns a persistent incoming stream for one bounded call. Remote MCP and automatic chat wakeups remain unimplemented.

[Worker pause](worker-pause.md) is explicit coordination state and does not stop agents or change jobs, focus, messages or review execution.

[Default job reads](job-state.md#default-reads) return the selected brief, current progress, remaining checklist and compact pending job notes. Worker inbox, pause and focus use their own tools.

[Deferred notes](notes.md) own reminder/source scope, explicit consumption, compact previews and immutable revisions. Note operations perform no provider work. Default job reads include only compact pending notes attached to that exact job; full note bodies remain explicit.

Execution tool arguments and recovery are owned by [review execution](review-execution.md#bound-execution-tools).

## Configure one agent binding

Build with `make build`. Configure the host to launch this command with this agent's private profile:

```sh
/path/to/bin/fcmcp --profiles-file /private/profiles.json --profile project-builder
```

A repeatable process `FC_PROFILE` can replace the profile argument. The default profile file and credential-reference rules match the [CLI](development.md#private-profile-file). This connection keeps its startup instance/project/worker binding. There is no per-tool profile selector or workspace-wide current worker. Different grants or agent identities use separately bound processes/connections. Restart explicitly to select another binding.

Startup validates configuration without opening the credential or contacting the service. Initialization, discovery and local `context` also need no credential or network. Service calls load the selected protected credential and verify the bound instance before the scoped operation, using the [client bootstrap and operation budgets](agent-client.md#state-and-reconnect-semantics). An explicitly longer `review_start`, `review_selection_read`, `review_request`, `review_operation_read`, `review_source_refresh`, `review_checkpoint_publish` or `review_checkpoint_recover` wait and the quiet message wait have their selected wait plus five seconds; they do not change other tool deadlines. Argument validation precedes credential and network access. Each actual MCP session owns a lazy finite-request connection pool, released when that session closes. Every service tool call still reloads the protected credential and performs authenticated instance verification; the pool retains only transport connections. Separate sessions have separate pools. Long message/provider waits retain their owned transport clones, so they do not occupy the finite-request pool.

`job_update` discovery presents one typed object with explicit selectors, guard and editable fields. It does not use root object-union alternatives. Progress text belongs inside `progress`, for example `"progress":{"summary":"Current result","next":"Next action"}`; `brief` is a separate top-level string. Omitted fields remain unchanged. Local validation identifies the invalid declared field or nested progress member, reports UTF-8 byte bounds without echoing authored text, and confirms that no request was sent.

The combined `job_update` body supports the API's 384 KiB encoded JSON limit, including escaped brief and progress text. Its tool arguments allow a further 1 KiB for selectors and the idempotency key; the stdio frame allows a further 16 KiB for the protocol envelope. Finding-decision/fix batches allow 2 MiB plus 1 KiB of selectors; the stdio frame allows a further 16 KiB. Native project-management documents retain the typed client/API document budgets (at most 256 KiB), with 1 KiB for their outer selectors and key. Other mutations retain their 256 KiB argument limit and ordinary reads retain 16 KiB. These transport allowances do not increase field or API body limits. A valid atomic CLI update can therefore be recovered through MCP using the same content and key without splitting it into new writes.

## Tools and output

| Tool | Input and result |
| --- | --- |
| `context` | No fields; configured local identity plus running client source revision/state, without service verification. |
| `presentation_read` | Explicit bound-project appearance snapshot and project-metadata watermark; no provider work or mutation. |
| `doctor` | No fields; verified instance/project/worker identity plus running client source revision/state. |
| `workers_list` | Optional limit/cursor; one compact worker page. |
| `worker_pause_read` | Optional worker_id in the bound project; defaults to the acting worker. Reads explicit pause, reason and independent revision without switching identity. |
| `worker_pause_history` | Optional same-project worker_id/limit/cursor; one explicit newest-first pause-history page. Worker selection does not change the actor. |
| `jobs_list` | Optional worker_id/status/limit/cursor; one compact job page. |
| `jobs_assigned` | Optional status/limit/cursor; default status:open selects active/blocked/parked assignments. Explicit status:all includes completed history. |
| `job_status_read` | Required job_id; compact current status/revision/assignment and bounded progress/next/blocker previews with truncation flags and evidence count; no brief, checklist, notes or history. |
| `job_read` | Required job_id; complete current brief/progress, remaining checklist/counts and compact pending exact-job notes. |
| `job_summary_read` | Required job_id; current optional shareable title/body and independent summary revision; remains private. |
| `job_summary_set` | Required job_id, expected_job_revision, expected_revision, title, body and idempotency_key; guarded summary replacement or explicit clear. |
| `job_revision_read` | Required job_id and positive revision; one retained full-state snapshot including its original brief, with no paging/current context/provider work. Missing history returns not_found. |
| `job_history` | Required job_id and optional limit/cursor; explicit full-state history page. |
| `job_progress_history` | Required job_id and optional limit/cursor; one explicit unified progress archive page. |
| `job_update` | Required job_id, expected_revision and idempotency_key; at least one selected title/brief/worker_id/status or progress edit; progress is an object with summary, next, blocker and evidence members. One atomic update and compact receipt. |
| `job_primary_read` | Optional same-project worker_id, defaulting to the acting worker; selected worker’s primary job and independent focus revision. |
| `job_primary_history` | Optional same-project worker_id/limit/cursor; one page of the selected worker’s focus history, without switching actor identity. |
| `job_checklist_read` | Required job_id; current ordered checklist, its independent checklist revision and current job revision. |
| `job_checklist_history` | Required job_id and optional limit/cursor; one newest-first full checklist revision page. |
| `job_checklist_item_update` | Required job_id/item_id/expected_revision/done/idempotency_key; set one existing item's completion guarded only by checklist revision, preserving other items and job text. Compact checklist receipt. |
| `map_entry_list` | Optional kind/archived/limit/cursor; one compact map page, no phases or descriptions. |
| `map_entry_read` | Required entry_id; one complete current workstream or return point. |
| `map_entry_history` | Required entry_id and optional limit/cursor; compact newest-first revision summaries. |
| `map_entry_revision_read` | Required entry_id/revision; one complete immutable editorial snapshot. |
| `map_entry_create` | Required idempotency_key/entry; create one bounded workstream or return point. |
| `map_entry_replace` | Required idempotency_key/entry_id/expected_revision/entry; full guarded replacement, preserving retained phase IDs. |
| `map_entry_update` | Required entry_id, expected_revision, changes and idempotency_key; patch only selected editable fields, preserving omitted content. No provider or public publication work. |
| `job_workstream_read` | Required job_id; independent association revision and captured-at-association title. |
| `job_workstream_replace` | Required idempotency_key/job_id/expected_revision/workstream_id; select a scoped active workstream or explicitly clear with empty text. |
| `job_workstream_history` | Required job_id and optional limit/cursor; one compact captured-association history page. |
| `note_read` | Required note_id; one full current note. |
| `note_list` | Optional worker_id/all_workers/direct_only/job_id/phase/kind/status/limit/cursor; compact notes for the bound worker and its currently assigned jobs by default. |
| `note_history` | Required note_id and optional limit/cursor; one compact revision page. |
| `note_revision_read` | Required note_id/revision; one full immutable note revision. |
| `outbox_list` | Bound sender’s outgoing handoffs with recipient seen/acknowledged state. Same pagination/status/order selectors as inbox_list; explicit unacknowledged:false includes handled history. |
| `inbox_delivery_read` | Required message_id; compact current recipient receipt in the bound project. No body, impersonation or receipt writes. |
| `inbox_list` | Optional limit/cursor/unread/unacknowledged/newest; pending previews by default, including seen-but-unhandled. Explicit unacknowledged:false includes handled history; newest:true reverses insertion order. Preserve selection on continuation. No receipt change. |
| `inbox_thread` | Required conversation_id and optional limit/cursor; explicit full-body conversation page. |
| `inbox_message_read` | Required message_id; one full selected immutable message, without older conversation bodies or seen/ack changes. |
| `inbox_snapshot` | Same pending/history selection as inbox_list, plus an explicit event cursor from the same read transaction; no receipt change. |
| `inbox_events` | Required event cursor; optional limit and wait_ms 0–4000; one bounded incoming-event page. |
| `wait_for_messages` | Required event cursor; optional limit 1–100 and wait_seconds 1–50, default 20/25. One new-message event page or quiet timeout over a call-owned persistent stream. |
| `review_queue_add` | Project writer; current expected_configuration_revision, pull_number and idempotency_key; optional before, after or position. Adds scheduling and Delivery together, preserving unrelated settings and existing controls. No provider work. |
| `review_queue_move` | Same bound selectors; one destination is required. Reorders the configured queue and Delivery together. No-op preserves revision; no provider work. |
| `review_queue_remove` | Project writer; current expected_configuration_revision, pull_number, reason (`superseded` or `defunct`) and idempotency_key. Atomically removes scheduling and visible enrollment, preserving all history and settings. Refuses merged PRs and pending operations; never cancels work or requests a review. |
| `review_execution_read` | No fields; stored execution configuration and revision, without provider work. |
| `review_operation_read` | Required operation_id; optional wait_seconds (0–50/default 0) collects compact stage, cause, evidence handle and next action without polling. Pending-evidence holds include bounded current blocker references; see [operation discovery](review-operations.md#pending-native-evidence). Optional diagnostics:true returns the original full operation. Queued wait metadata retains automatic retry deadlines and explicit paused state; neither means a review completed. |
| `review_operation_control` | Project writer; operation_id, expected_revision, action pause/resume/cancel and explicit idempotency_key. Controls only queued undispatched intent; pause preserves the retry deadline, resume allows the same operation to retry when due, cancel withdraws without claiming provider cancellation. No grant/taper/project-pause changes. |
| `review_operations_list` | Optional pull_number, channel cli/hosted, status pending/all (default pending), limit 1–100 (default 5), cursor; one compact retained operation page, including history with all for lost-receipt recovery. No provider work. See [operation discovery](review-operations.md). |
| `review_request` | Required idempotency_key/expected_configuration_revision/pull_number/channel/head_sha; optional allow_out_of_order (boolean/default false) bypasses only queue scheduling; override_file_limit (boolean/default false) skips only the optional project file ceiling for that exact request; wait_seconds (0–50/default 2) returns terminal state or a compact pending receipt authorizing background provider work. |
| `review_source_refresh` | Required idempotency_key/expected_configuration_revision/pull_number/head_sha; optional wait_seconds (default 2, range 0–50), include_ci (default false) and include_readiness (default false). Returns a completed or held outcome within the wait, otherwise an operation receipt; schedules selected source/CI reads, never a review. |
| `review_source_refresh_read` | Required refresh_id; stored operation and observed comparison, no provider calls. |
| `review_source_ci_assess` | Required pull_number; compact retained required-CI facts and local applicability, with observation age. Optional diagnostics names required contexts and selected CheckRun/Commit Status facts; no provider work or merge-readiness claim. |
| `review_source_policy_assess` | Required pull_number; current local policy against the last completed source observation, with its age; no CI or merge-readiness claim. |
| `review_policy_assess` | Required evidence_id; current local policy against that retained captured comparison; no source refresh, CI or merge-readiness claim. |
| `review_start` | Required channel/idempotency_key; optional expected_pull_number, expected_configuration_revision and wait_seconds (0–50/default 2). One explicit channel-front selection/request; no unattended successor. |
| `review_selection_read` | Required selection_id; optional wait_seconds (0–50/default 0). One retained selection and original linked operation; no provider action. |
| `review_rounds_read` | Required pull_number and channel; compact stored additional-round progress and closure. No provider calls or readiness verdict. |
| `review_rounds_configure` | Project-writer grant/close with pull_number, channel, expected_revision, action and idempotency_key. Grant requires minimum/maximum; close omits both. Changes local controls without dispatching reviews. |
| `review_evidence_read` | Required evidence_id; explicit full retained native capture and parsed findings, excluding raw diagnostics. |
| `review_adjudication_read` | Required evidence_id; current guarded decisions and fix references without repeating finding text. |
| `review_findings_decide` | Required evidence_id, expected_revision, explicit items (1–100 unique indices, dispositions, reasons and applicable route guards), idempotency_key; atomic single-revision batch, no implicit finalization. |
| `review_findings_fix` | Required evidence_id, expected_revision, explicit items (1–100 unique indices, expected/fix SHA and proof), idempotency_key; atomic single-revision batch preserving source counts. |
| `review_finding_decide` | Required idempotency_key/evidence_id/expected_revision/finding/disposition/reason; completed adjudication result. |
| `review_finding_fix` | Required idempotency_key/evidence_id/expected_revision/finding/expected_sha/fix_sha/proof_note; completed fix-reference result. |
| `review_source_finalize` | Required idempotency_key/evidence_id/expected_revision; completed source-count finalization result. |
| `review_subagent_record` | Record one actual completed independent Subagent review under the bound project writer; counts and identities derived; no provider request or allowance use. |
| `review_subagent_read` | Read a recorded Subagent run, stable run revision and original findings for decision/fix updates. |
| `review_subagent_update` | Guarded partial decisions/fix proof/finalization on a recorded Subagent review; original capture and finalized counts retained. |
| `review_finding_route` | Required idempotency_key/evidence_id/expected_revision/finding/target_pull_number/reason; optional expected_route_revision; compact route/source revision result. |
| `review_route_read` | Required route_id; explicit route detail with its one original source finding. |
| `review_routes_list` | Required pull_number; optional limit/cursor; compact incoming routes in the bound project. |
| `review_route_decide` | Required idempotency_key/route_id/expected_revision/disposition/reason; independent receiving decision. |
| `review_route_fix` | Required idempotency_key/route_id/expected_revision/expected_sha/fix_sha/proof_note; guarded receiving fix reference. |
| `review_queue` | Optional active/limit/cursor; default five stored PRs including history; active:true filters open PRs before paging and returns compact scalar state. |
| `review_pull` | Required pull_number, optional snapshot; compact PR summary without loading the attempt archive. |
| `review_attempts` | Required pull_number, optional snapshot/limit/cursor; default five newest-first retained attempts with original producer identity, without automatic paging or provider work. |
| `review_runs` | Required pull_number, optional snapshot/limit/cursor; default five stored run summaries, retaining each run’s own dates and pinned cursor while omitting unrelated workspace-origin timestamps. |
| `review_run` | Required run_id, optional snapshot; explicit full findings/decisions/fix proof. |
| `review_checkpoint_publish` | Required idempotency_key/evidence_id/expected_revision; optional wait_seconds (default 2, range 0–50). Explicitly publishes finalized historical source counts, returning terminal facts or a pending receipt. |
| `review_checkpoint_read` | Required publication_id; compact stored publication state, without provider work. |
| `review_checkpoint_recover` | Required idempotency_key/publication_id/expected_revision; optional retry_unposted (default false) and wait_seconds. Observation-only recovery by default; uncertain posts are never blindly repeated. |
| `review_source_readiness` | Required pull_number; compact retained observations evaluated with current local policy, without provider work or merge authorization. |
| `jobs_create` | Required idempotency_key/title; optional brief/worker_id/status; compact job receipt. |
| `worker_pause_replace` | Required idempotency_key/expected_revision/paused/reason; guarded pause for optional same-project worker_id (defaults to the acting worker), with empty reason required for resume. |
| `job_primary_replace` | Required idempotency_key/expected_revision/job_id; guarded focus replacement; optional same-project worker_id defaults to the acting worker, and empty job ID clears. |
| `job_checklist_replace` | Required idempotency_key/job_id/expected_job_revision/expected_revision/items; guarded ordered replacement and compact changed/unchanged receipt. |
| `note_create` | Required idempotency_key/body; optional worker_id/job_id/phase/kind; compact creation receipt. |
| `note_update` | Required idempotency_key/note_id/expected_revision plus selected scope/kind/body/status/status_reason fields; compact changed/unchanged receipt. |
| `inbox_send` | Required idempotency_key/recipient_worker_id/body; optional reply_to_message_id; compact message receipt. |
| `inbox_seen` | Required idempotency_key/message_id; explicit recipient seen transition. |
| `inbox_ack` | Required idempotency_key/message_id; explicit recipient acknowledgment, also establishing seen. |
| `presentation_replace` | Replace this project's presentation using the native {expected_revision,presentation} document. Requires project management and an explicit key; preserves the normal theme/config validation. |
| `presentation_history` | Read one presentation history page; no automatic continuation. |
| `presentation_revision_read` | Read one retained presentation revision. |
| `publication_configuration_read` | Read this project's selected public publication configuration. Private edits are not published implicitly. |
| `publication_prepare` | Prepare selected publication changes against current stored configuration and return the exact native input and preview hash. Does not apply or publish; changes use the existing editorial selection contract. |
| `publication_preview` | Preview a complete native publication input {expected_revision,enabled,manifest}; returns its content and preview_sha256, without publishing. |
| `publication_apply` | Apply the exact approved publication input with its preview_sha256, expected revision and explicit key. Makes only explicitly selected public content available. |
| `publication_unpublish` | Disable the selected project's public publication with current expected_revision and an explicit key. Retains private content and publication history. |
| `coordination_import_read` | Read this project's current coordination-import accounting and provenance. |
| `coordination_import_preview` | Validate and preview one strict native coordination-import batch without applying it. Project management is required; no provider work. |
| `coordination_import_apply` | Apply exactly the previewed native coordination-import batch with preview_sha256 and explicit key. Preserves existing provenance, counts and concurrency guards. |
| `review_import_preview` | Validate and preview a complete native review-import document without applying it or inventing review evidence. |
| `review_import_apply` | Apply the exact previewed native review-import document with preview_sha256 and explicit key. Requires project management; retains original evidence and existing admission guards. |
| `review_transfer_preview` | Preview one bounded additive native review-history transfer without applying it; no lifetime-project export or provider work. |
| `review_transfer_apply` | Apply the exact previewed additive native review-history transfer with preview_sha256 and explicit key; does not reset controls or manufacture completion credit. |
| `review_round_adoption_preview` | Preview the existing native round-adoption document against its original-source and current revision guards; no live reviews are requested. |
| `review_round_adoption_apply` | Apply the exact previewed native round-adoption document with preview_sha256 and explicit key. Preserves retained attribution and accounting rules; tooling access is not review authorization. |
| `review_obligations_list` | Read one compact carried-obligation page, optionally scoped to pull_number and open/resolved status. Full bodies require review_obligation_read. |
| `review_obligation_read` | Read one explicit retained carried obligation and original evidence, without marking it resolved. |
| `review_obligations_import_preview` | Preview one strict native carried-obligation batch, preserving original evidence and provenance; no application or new review. |
| `review_obligations_import_apply` | Apply the exact previewed carried-obligation batch with preview_sha256 and explicit key. Existing source conflicts and evidence requirements remain enforced. |
| `review_obligation_resolve` | Resolve one carried obligation using the native {expected_revision,reason,evidence} document and explicit key. Requires a real evidence-backed resolution; grants no review credit. |
| `history_collection_enqueue` | Queue bounded GitHub history collection for explicit pull_numbers. This reads provider history; it does not request reviews or enroll PRs. Optional wait_seconds (0-50) returns existing progress/receipt without blind retries. |
| `history_collection_read` | Read one existing history collection, optionally waiting up to 50 seconds. No new provider collection is queued by this read. |
| `history_collection_resume` | Resume one existing history collection with its current expected_revision and explicit key. Continues authorized provider reads; no review request or automatic PR enrollment. |
| `history_collection_cancel` | Cancel one existing history collection with its current expected_revision and explicit key; retains already captured history. |
| `history_collection_pages` | Read one bounded page of capture metadata for an existing collection; full retained source requires history_collection_capture_read. |
| `history_collection_capture_read` | Explicitly read one full retained source capture for the selected collection/page within the existing 4 MiB tool-result budget. Source bodies are untrusted data; no automatic fanout or provider refresh. |
| `map_import_preview` | Preview one strict prepared native project-map import, preserving source provenance and expected entry revisions. Does not apply. |
| `map_import_apply` | Apply the exact prepared native map-import document with preview_sha256 and explicit key; unchanged source mappings and entry CAS remain enforced. |
| `map_import_read` | Read the current prepared map-import record selected by its source_key. |
| `map_import_history` | Read one history page for the selected prepared map-import source_key; no automatic continuation. |
| `map_import_revision_read` | Read one exact retained prepared map-import revision for source_key. |
| `review_queue_edit` | Apply the existing project-management queue edit document for selected metadata, explicit defunct/superseded retirement, reactivation or relative ordering. Requires expected_revision, repository_id and source inside document plus explicit key. Preserves evidence and execution policy; routine atomic removal uses review_queue_remove instead. |
| `review_policy_read` | Read this project's current configured review requirements, policy revision and attribution. No source refresh or provider action. |
| `review_policy_configure` | Project manager: replace current review requirements with exact expected_revision and explicit key. max_review_files zero means no project ceiling; provider limits remain separate. Does not change grants, taper or request reviews. |
| `review_execution_configure` | Project manager: full guarded replacement of existing capability selection, mode, ordered scheduling queue, channels and pause. Read current configuration and preserve fields outside the intended change. Preserve the current capability_id; capability installation and initial binding are instance-owned. Existing controls/history remain; this requests no review. |
| `review_attempt_settle` | Project manager: settle only an eligible original uncertain attempt after independently verifying its external action stopped. Preserve exact revision/repository/head/external ID guards and explicit reason/evidence. Does not cancel a provider, invent evidence, award completion credit or request another review. |
| `worker_create` | Project writer: create one named worker in this bound project. Returns its ID; does not switch the acting worker, create credentials or start an agent. |
| `project_credential_grant` | Project manager: grant this project to a distinct non-admin credential using only its lowercase SHA-256 digest. Raw tokens never enter tool arguments/output; generate/store credentials privately through onboarding. manage implies read/write. Explicit key; no other-project or instance authority. |
| `project_credential_revoke` | Project manager: revoke a known credential's access to this project only. Preserves its other-project grants; no token or acting identity change. Explicit key, original ID retained after unknown outcome. |
| `project_credential_update` | Project manager: replace one existing non-admin credential's permissions in this project under an exact expected_permissions guard. Preserves credential, principal, token and worker binding; changes no other project. manage implies read/write. No hidden role or identity switching. |

Review queue/run/attempt lists default to five entries; other limits use the API default when omitted and accept explicit values from 1 to 100. Reads never advance seen/ack state, change a job or write a checkpoint. Assigned jobs do not claim to contain primary-job or full resume context. Snapshot and event cursors have different scopes. Event reads retain no cursor and never automatically poll or resubscribe; an explicitly requested bounded wait can return an empty `timed_out` page. The finite snapshot/event operations are separate from the explicit quiet wait; none establishes idle-agent wakeup. Invalid, unknown, duplicate, exactly mis-cased, null or oversized tool fields fail before service work. Profile selectors cannot be supplied as tool arguments.

[Job progress](job-progress.md) owns update/checklist content limits, independent revisions, explicit archives and integration proof. Checklist replacement can remove or replace effective items and is annotated destructive; its immutable history remains available. Job updates and checklist operations perform no provider work.

Project-management tools use the same bound identity and guarded typed-client operations as the CLI. An explicit `manage` grant permits management of that selected project; it never grants instance administration. Read/write clients retain their documented ordinary operations. Presentation/publication/import/transfer tools accept their native structured documents and validate them before service access; no tool accepts an arbitrary URL, HTTP route or raw credential. Preview tools do not apply changes. Apply tools require the exact document, preview hash and explicit key; publication includes only selected approved public content. The exposed history-collection operations read GitHub history and do not request provider reviews.

Mutations require the caller to retain an explicit key and request; the adapter never generates a replacement key or automatically retries. Job edits require the current expected revision; empty brief or worker clears that field. A dispatched write with a lost, canceled or invalid response returns `outcome_unknown` when a tool result can be delivered. Host cancellation may suppress that result, so absence of a reply is never proof of rollback. Recover using the original request, key and binding. The [MCP annotations](https://modelcontextprotocol.io/specification/2025-11-25/schema#toolannotations) distinguish reads from writes. Repeating identical arguments retains the same explicit key and has no additional effect; job edits and review decision/fix corrections can replace effective fields while retained history remains; creation/checkpoint/send/receipt transitions add records or handling state. Job, message and evidence controls operate within the configured service. `review_start`, `review_request`, `review_source_refresh`, `review_checkpoint_publish` and `review_checkpoint_recover` are annotated open-world because they authorize external provider work; a queued receipt does not establish started or completed execution. The [CLI recovery rules](development.md#explicit-agent-mutations) also apply.

All explicit MCP message and result waits are capped at 50 seconds in both discovery and argument validation. With the five-second service budget, these calls leave at least five seconds below a 60-second host tool deadline. Hosts with shorter configured limits need shorter requested waits. This MCP cap does not narrow CLI or typed-client limits.

Results contain structured data with a compatibility text representation. Tool failures use `isError` and safe code/message/action plus bounded request ID/details where available. Standard output contains only MCP protocol frames; startup/connection diagnostics use standard error. The adapter emits no progress stream, provider scans or heartbeat content into tool results. Every service response and tool result is limited to 4 MiB; routine lists stay compact.

## Proof and limitations

SDK transport tests cover four legacy initialize versions (2024-11-05, 2025-03-26, 2025-06-18 and 2025-11-25) and the 2026-07-28 discovery/metadata path. An actual Linux stdio subprocess passed 11 packaged check groups, including all nine original non-event read tools under 2025-11-25, strict arguments, cancellation, the total deadline and reconnect after service restart. The official SDK command transport verified newest-protocol discovery, list and local context; the newest version does not use the legacy initialize sequence. Tests against actual public service handlers cover scoped reads, strict arguments, complete pages, privacy errors, cancellation and the total deadline, with unchanged domain/events/receipts and provider counts. Linux build, race tests, vet and Windows amd64/macOS arm64 cross-builds passed for the adapter slice.

Six additional packaged mutation groups passed through actual Linux stdio binaries, covering all five write tools, sixteen invalid-input cases with zero HTTP, scope/revision/replay/recipient transitions and revocation. An owned proxy committed writes before dropping, holding or canceling the response; each original attempt dispatched once, and explicit same-key recovery retained one effect. The deadline case completed in 5.003 seconds. Mutation protocol proof uses 2025-11-25.

Seven packaged message-event groups passed with all sixteen tools discovered. Twenty-eight invalid argument cases made zero HTTP requests, including missing-credential precedence. Actual two-project read-grant snapshot/replay, repeated delivery, filtered pages and sent/seen/ack metadata preserved scope without changing recipient state. An actual four-second wait completed in 4.004 seconds; concurrent local context and scoped worker reads finished in 0.007 seconds on the same stdio session. Explicit JSON-RPC cancellation closed the forwarded HTTP wait in 0.003 seconds, and the shared deadline returned in 5.005 seconds without retry. These are single-run observations, not throughput or host scheduling guarantees.

The 62-file Go/module manifest was unchanged before/after that run: SHA-256 `442352ccfee0a73fca72fd4677bb90205519f3e817b3532b6d58e29335d45fbf`. Actual `fcd` and `fcmcp` binaries and their private copies also remained unchanged. The run used local stdio protocol 2025-11-25; setup used read-only public GitHub repository verification and all disposable state/processes were removed.

These checks establish the SDK/protocol and service boundary, not compatibility with every installed agent host. Whether a pending tool permits other model work, delivers completion across turns, or wakes an idle chat remains separate host proof. Native Windows initialization/list/local-context and safe missing-credential behavior passed. Native Windows service-backed MCP read and job-create/replay checks also passed against actual `fcd`; all five mutation tools have Linux packaged proof. The [packaged smoke guide](../tools/packagedsmoke/README.md#native-platform-gate) records newer native Windows/macOS SDK execution, quiet-wait cancellation, concurrent reads and session reuse. Installed-agent-host delivery across turns remains unproven.

## Native review evidence

The [Subagent review tools](subagent-reviews.md) provide ordinary worker recording, stable run reads and batched decision/fix/finalization updates without provider work or an administrator import.

The [native evidence controls](review-controls.md#bound-cli-and-mcp) add explicit retained capture/adjudication reads and keyed finding decision, fix-reference and source-finalization tools. These operations do not dispatch a review; source finalization atomically queues the GitHub summary for background delivery without waiting for fixes or provider I/O. The [readiness tool](review-readiness.md#service-and-bound-tools) combines retained observations and current local policy without provider work; `include_readiness` on explicit source refresh requests its collection. The five [finding-route tools](review-routes.md#bound-controls) add explicit creation, incoming lists, full route reads, receiving decisions and guarded fix references. They are local, profile-bound operations with independent source and receiving revisions.

Source finalization automatically queues the [public review checkpoint](review-publication.md). The three explicit checkpoint tools remain for historical repair, delivery inspection and exceptional recovery; ordinary agents need no separate publish call. Their bounded wait returns useful terminal facts when available and a compact pending receipt otherwise. A lost response preserves the same source, request and key; observation-only recovery does not authorize a repost.

The [worker primary-job contract](worker-focus.md) owns focus semantics and proof. Current primary reads are compact selectors; they do not imply that the job brief, checkpoint or messages have been loaded. Zero revision is valid for an initial focus, checklist or worker-pause conflict and remains visible in safe error details.

## Discovery page boundary

The bounded current catalog fits on the first `tools/list` page (configured page size 128), including messaging, worker listing and pause tools. Cursor-aware clients still follow `nextCursor` for a larger catalog. Default discovery checks compare the complete expected tool set; a synthetic 129-tool server exercises actual 128-plus-one SDK pagination without changing production tools.

Clean source `e65315f3141fcba180499110cb0a5ad6f4948d6c` passed the actual packaged daemon/CLI/MCP stdio workload on its first run in 0.208 seconds, including first-page critical-tool assertions. All 567 tracked source/asset hashes and three clean CGO-free executable hashes stayed unchanged. The tested `fcmcp` SHA-256 was `36f9954648d3c12cbc45fd26212d22bf3cb8a234a90e603f258971c73ad2cf75`. Focused discovery/map and documentation-inventory checks, race and vet passed separately. This establishes the supplied server and official SDK behavior; it makes no claim about a particular installed agent host's pagination.

## Remote connection proof

Clean client checkpoint `08423ac5110dca2befcdfc6330c5696cc526a1ea` passed the complete typed-client and MCP-adapter race suites in 52.790 and 48.251 seconds, scoped vet and six actual packaged daemon/CLI/MCP/upgrade/import cases in 1.902 seconds. Focused actual HTTP/SDK checks verify connection reuse, distinct-session isolation and cleanup, credential rotation/removal, changed-instance mutation refusal, and five pending message streams without blocking a finite instance check. An independent source review checked the session-drain and transport ownership boundaries.

An actual stdio MCP connection then made six sequential bounded reads through a trusted hosted HTTPS endpoint. Its first call took 1.121 seconds; the five subsequent calls had a 0.527-second median. Before reuse, six MCP reads had a 1.074-second median and six separate CLI reads a 1.065-second median. Every tool still performs its instance check and selected domain read; the improvement comes from retaining transport connections. Result sizes stayed the same, inputs stayed unchanged, and the child process was joined with no stderr. The installed service remained at the separately qualified `5ba5ed5b7ba620b4b32bfce2cd07c3b701887eea` runtime. These small sequential samples establish neither fleet throughput nor a latency guarantee; request counts are source-derived, and native host scheduling and idle wakeup remain separate.
