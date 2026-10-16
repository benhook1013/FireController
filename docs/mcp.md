# Local MCP Tools

## Implemented boundary

`fcmcp` exposes the tools listed below over local stdio using the official [Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk), pinned to v1.8.0. Tools call the shared typed HTTP client. The service owns authentication, project scope, permission checks, cursors and storage; the adapter never shells out to `fc`; explicit review requests authorize service-owned background provider work. Both CLI and MCP job/message and review-evidence mutations use the same typed client and API. [Checkpoint tools](job-checkpoints.md) append immutable progress and provide explicit latest/archive reads. Subscriptions, remote MCP and automatic chat wakeups remain unimplemented.

Execution tool arguments and recovery are owned by [review execution](review-execution.md#bound-execution-tools).

## Configure one agent binding

Build with `make build`. Configure the host to launch this command with this agent's private profile:

```sh
/path/to/bin/fcmcp --profiles-file /private/profiles.json --profile project-builder
```

A repeatable process `FC_PROFILE` can replace the profile argument. The default profile file and credential-reference rules match the [CLI](development.md#private-profile-file). This connection keeps its startup instance/project/worker binding. There is no per-tool profile selector or workspace-wide current worker. Different grants or agent identities use separately bound processes/connections. Restart explicitly to select another binding.

Startup validates configuration without opening the credential or contacting the service. Initialization, discovery and local `context` also need no credential or network. Service calls load the selected protected credential and normally share a five-second budget across authenticated instance verification and the scoped operation. An explicitly longer source-refresh wait has its selected wait plus five seconds; it does not change other tool deadlines. Argument validation precedes credential and network access.

## Tools and output

| Tool | Input and result |
| --- | --- |
| `context` | No fields; configured local identity, without service verification. |
| `doctor` | No fields; verified instance/project/worker identity. |
| `workers_list` | Optional limit/cursor; one compact worker page. |
| `jobs_list` | Optional worker_id/status/limit/cursor; one compact job page. |
| `jobs_assigned` | Optional status/limit/cursor; one compact page assigned to the bound worker. |
| `job_read` | Required job_id; explicit full job detail and brief. |
| `job_history` | Required job_id and optional limit/cursor; explicit full-state history page. |
| `job_checkpoint_latest` | Required job_id; explicit latest checkpoint and current job revision, including null when none exists. |
| `job_checkpoint_history` | Required job_id and optional limit/cursor; explicit newest-first checkpoint archive. |
| `inbox_list` | Optional unread/unacknowledged/limit/cursor; bound worker's compact preview page. |
| `inbox_thread` | Required conversation_id and optional limit/cursor; explicit full-body conversation page. |
| `inbox_snapshot` | Optional unread/unacknowledged/limit/cursor; one inbox page plus its atomic incoming-event watermark. |
| `inbox_events` | Required event cursor; optional limit and wait_ms 0–4000; one bounded incoming-event page. |
| `review_execution_read` | No fields; stored execution configuration and revision, without provider work. |
| `review_operation_read` | Required operation_id; retained native operation state and reason. |
| `review_request` | Required idempotency_key/expected_configuration_revision/pull_number/channel/head_sha; compact queued receipt authorizing background provider work. |
| `review_source_refresh` | Required idempotency_key/expected_configuration_revision/pull_number/head_sha; optional wait_seconds (default 2, range 0–60), include_ci (default false) and include_readiness (default false). Returns a completed outcome within the wait, otherwise an operation receipt; schedules selected source/CI reads, never a review. |
| `review_source_refresh_read` | Required refresh_id; stored operation and observed comparison, no provider calls. |
| `review_source_ci_assess` | Required pull_number; compact retained required-CI facts and local applicability, with observation age; no provider work or merge-readiness claim. |
| `review_source_policy_assess` | Required pull_number; current local policy against the last completed source observation, with its age; no CI or merge-readiness claim. |
| `review_policy_assess` | Required evidence_id; current local policy against that retained captured comparison; no source refresh, CI or merge-readiness claim. |
| `review_evidence_read` | Required evidence_id; explicit full retained native capture and parsed findings, excluding raw diagnostics. |
| `review_adjudication_read` | Required evidence_id; current guarded decisions and fix references without repeating finding text. |
| `review_finding_decide` | Required idempotency_key/evidence_id/expected_revision/finding/disposition/reason; completed adjudication result. |
| `review_finding_fix` | Required idempotency_key/evidence_id/expected_revision/finding/expected_sha/fix_sha/proof_note; completed fix-reference result. |
| `review_source_finalize` | Required idempotency_key/evidence_id/expected_revision; completed source-count finalization result. |
| `review_finding_route` | Required idempotency_key/evidence_id/expected_revision/finding/target_pull_number/reason; optional expected_route_revision; compact route/source revision result. |
| `review_route_read` | Required route_id; explicit route detail with its one original source finding. |
| `review_routes_list` | Required pull_number; optional limit/cursor; compact incoming routes in the bound project. |
| `review_route_decide` | Required idempotency_key/route_id/expected_revision/disposition/reason; independent receiving decision. |
| `review_route_fix` | Required idempotency_key/route_id/expected_revision/expected_sha/fix_sha/proof_note; guarded receiving fix reference. |
| `review_queue` | Optional limit/cursor; stored queue, default five PRs. |
| `review_pull` | Required pull_number, optional snapshot; explicit PR summary and attempts. |
| `review_runs` | Required pull_number, optional snapshot/limit/cursor; default five stored run summaries. |
| `review_run` | Required run_id, optional snapshot; explicit full findings/decisions/fix proof. |
| `review_checkpoint_publish` | Required idempotency_key/evidence_id/expected_revision; optional wait_seconds (default 2, range 0–60). Explicitly publishes finalized historical source counts, returning terminal facts or a pending receipt. |
| `review_checkpoint_read` | Required publication_id; compact stored publication state, without provider work. |
| `review_checkpoint_recover` | Required idempotency_key/publication_id/expected_revision; optional retry_unposted (default false) and wait_seconds. Observation-only recovery by default; uncertain posts are never blindly repeated. |
| `review_source_readiness` | Required pull_number; compact retained observations evaluated with current local policy, without provider work or merge authorization. |
| `jobs_create` | Required idempotency_key/title; optional brief/worker_id/status; compact job receipt. |
| `job_edit` | Required idempotency_key/job_id/expected_revision plus selected title/brief/worker_id/status fields; compact receipt. |
| `job_checkpoint_append` | Required idempotency_key/job_id/expected_job_revision/done/next; optional blocker/evidence; compact checkpoint receipt. |
| `inbox_send` | Required idempotency_key/recipient_worker_id/body; optional reply_to_message_id; compact message receipt. |
| `inbox_seen` | Required idempotency_key/message_id; explicit recipient seen transition. |
| `inbox_ack` | Required idempotency_key/message_id; explicit recipient acknowledgment, also establishing seen. |

Review queue/run lists default to five entries; other limits use the API default when omitted and accept explicit values from 1 to 100. Reads never advance seen/ack state, change a job or write a checkpoint. Assigned jobs do not claim to contain primary-job or full resume context. Snapshot and event cursors have different scopes. Event reads retain no cursor and never automatically poll or resubscribe; an explicitly requested bounded wait can return an empty `timed_out` page. These recovery/read operations are not a quiet background subscription or an idle-agent wakeup interface. Invalid, unknown, duplicate, exactly mis-cased, null or oversized tool fields fail before service work. Profile selectors cannot be supplied as tool arguments.

Mutations require the caller to retain an explicit key and request; the adapter never generates a replacement key or automatically retries. Job edits require the current expected revision; empty brief or worker clears that field. A dispatched write with a lost, canceled or invalid response returns `outcome_unknown` when a tool result can be delivered. Host cancellation may suppress that result, so absence of a reply is never proof of rollback. Recover using the original request, key and binding. The [MCP annotations](https://modelcontextprotocol.io/specification/2025-11-25/schema#toolannotations) distinguish reads from writes. Repeating identical arguments retains the same explicit key and has no additional effect; job edits and review decision/fix corrections can replace effective fields while retained history remains; creation/checkpoint/send/receipt transitions add records or handling state. Job, message and evidence controls operate within the configured service. `review_request`, `review_source_refresh`, `review_checkpoint_publish` and `review_checkpoint_recover` are annotated open-world because they authorize external provider work; a queued receipt does not establish started or completed execution. The [CLI recovery rules](development.md#explicit-agent-mutations) also apply.

Results contain structured data with a compatibility text representation. Tool failures use `isError` and safe code/message/action plus bounded request ID/details where available. Standard output contains only MCP protocol frames; startup/connection diagnostics use standard error. The adapter emits no progress stream, provider scans or heartbeat content into tool results. Every service response and tool result is limited to 4 MiB; routine lists stay compact.

## Proof and limitations

SDK transport tests cover four legacy initialize versions (2024-11-05, 2025-03-26, 2025-06-18 and 2025-11-25) and the 2026-07-28 discovery/metadata path. An actual Linux stdio subprocess passed 11 packaged check groups, including all nine original non-event read tools under 2025-11-25, strict arguments, cancellation, the total deadline and reconnect after service restart. The official SDK command transport verified newest-protocol discovery, list and local context; the newest version does not use the legacy initialize sequence. Tests against actual public service handlers cover scoped reads, strict arguments, complete pages, privacy errors, cancellation and the total deadline, with unchanged domain/events/receipts and provider counts. Linux build, race tests, vet and Windows amd64/macOS arm64 cross-builds passed for the adapter slice.

Six additional packaged mutation groups passed through actual Linux stdio binaries, covering all five write tools, sixteen invalid-input cases with zero HTTP, scope/revision/replay/recipient transitions and revocation. An owned proxy committed writes before dropping, holding or canceling the response; each original attempt dispatched once, and explicit same-key recovery retained one effect. The deadline case completed in 5.003 seconds. Mutation protocol proof uses 2025-11-25.

Seven packaged message-event groups passed with all sixteen tools discovered. Twenty-eight invalid argument cases made zero HTTP requests, including missing-credential precedence. Actual two-project read-grant snapshot/replay, repeated delivery, filtered pages and sent/seen/ack metadata preserved scope without changing recipient state. An actual four-second wait completed in 4.004 seconds; concurrent local context and scoped worker reads finished in 0.007 seconds on the same stdio session. Explicit JSON-RPC cancellation closed the forwarded HTTP wait in 0.003 seconds, and the shared deadline returned in 5.005 seconds without retry. These are single-run observations, not throughput or host scheduling guarantees.

The 62-file Go/module manifest was unchanged before/after that run: SHA-256 `442352ccfee0a73fca72fd4677bb90205519f3e817b3532b6d58e29335d45fbf`. Actual `fcd` and `fcmcp` binaries and their private copies also remained unchanged. The run used local stdio protocol 2025-11-25; setup used read-only public GitHub repository verification and all disposable state/processes were removed.

These checks establish the SDK/protocol and service boundary, not compatibility with every installed agent host. Whether a pending tool permits other model work, delivers completion across turns, or wakes an idle chat remains separate host proof. Native Windows initialization/list/local-context and safe missing-credential behavior passed. Native Windows service-backed MCP read and job-create/replay checks also passed against actual `fcd`; all five mutation tools have Linux packaged proof. Native macOS execution and live subscriptions remain unproven.

## Native review evidence

The [native evidence controls](review-controls.md#bound-cli-and-mcp) add explicit retained capture/adjudication reads and keyed finding decision, fix-reference and source-finalization tools. These local operations do not dispatch a review or contact providers. The [readiness tool](review-readiness.md#service-and-bound-tools) combines retained observations and current local policy without provider work; `include_readiness` on explicit source refresh requests its collection. The five [finding-route tools](review-routes.md#bound-controls) add explicit creation, incoming lists, full route reads, receiving decisions and guarded fix references. They are local, profile-bound operations with independent source and receiving revisions.

The three [public review checkpoint tools](review-publication.md#bound-controls) distinguish explicit GitHub publication from local source finalization. Their bounded wait returns useful terminal facts when available and a compact pending receipt otherwise. A lost response preserves the same source, request and key; observation-only recovery does not authorize a repost.
