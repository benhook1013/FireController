# Agent and Client Contract

## Implementation status

The [local CLI foundation](development.md) implements version output, explicit profile selection and local context inspection. The [local API](local-api.md) implements service authentication and instance inspection, scoped projects/workers, basic jobs and durable messages. The typed client and CLI verify the expected instance, authenticate scoped reads and explicit-key job/message mutations, and reject instance replacement on subsequent requests. The [stdio MCP adapter](mcp.md) exposes bound reads and explicit-key job/message mutations through the same typed client. The [incoming-message feed](message-events.md) provides scoped snapshot/replay and bounded waits through CLI/MCP, plus HTTP SSE. Local [profile creation and POSIX bound launchers](onboarding.md#private-agent-setup-commands) are implemented; the [quiet CLI/MCP wait](message-wait.md) now owns a persistent connection for one bounded call. The temporary legacy bootstrap is documented separately in [delivery](delivery.md). [Worker contact](worker-presence.md) adds automatic connection attribution without routine flags or model-facing heartbeat output. The actual CLI wait proof below bounds established host behavior; native MCP concurrency and idle wakeup in an installed host remain unproved.

Prepared historical setup uses explicit administrator commands, separate from ordinary worker tools. The [selected job import](legacy-adoption.md#selected-job-snapshot-import) exposes `fc import state`, `fc import preview --file ...` and `fc import apply --file ... --preview ... --key ...`. These bounded local operations return useful results directly; they do not allocate pending-operation receipts, contact providers or replay historical messages. Preserve the exact input/key/binding for uncertain apply recovery.

## Profiles and identity

A private named profile selects service address, expected instance ID, project ID, worker ID, and credential reference. Optional defaults include output format and timeout. Profile names are local selectors, not server identities or permissions. Stable project and worker IDs survive display-name changes.

Configure the agent host to provide this agent's private profile through its process environment, for example `FC_PROFILE=project-builder`. Routine calls then omit project and worker flags:

```sh
fc inbox list
fc jobs assigned
```

When the host cannot provide a repeatable per-agent environment, a generated launcher can bind the profile on every invocation, for example `fc-builder inbox list`. Explicit `fc --profile project-builder inbox list` remains available for deliberate selection or diagnosis. The [private setup commands](onboarding.md#private-agent-setup-commands) create these bindings and POSIX launchers without service or credential access; [delivery](delivery.md) records the wider implemented boundary.

Agent assigned-job discovery defaults to open work; CLI `--all` or MCP `status:"all"` explicitly includes completed assignments. Management lists and raw HTTP/typed-client filters retain their existing explicit semantics.

Agent inbox list/snapshot calls default to pending (unacknowledged) messages, including ones already seen but not handled. CLI `--all` and explicit MCP `unacknowledged:false` select handled history; keep that selection on pagination. Raw HTTP and typed clients retain their explicit filter semantics. Reads never mark seen or acknowledged.

The binding belongs to that agent's client process or connection, never to a workspace-wide active profile. Worker names resolve within the bound project. Concurrent chats using one project's worker share its assignments and inbox, with distinct client connection IDs for contact attribution. A fresh CLI client has a fresh connection ID; this does not establish a stable host chat identity. See [worker contact](worker-presence.md). A named profile must never silently change another agent's current project or worker.

Profile selection comes from an explicit `--profile`, or a per-process `FC_PROFILE` default. The CLI does not persist a global active profile. An explicit flag can override the process default; a generated bound launcher rejects a conflicting profile override. Unknown profiles, expected-instance mismatches, missing identity, and unauthorized project selection fail before sending a mutation.

Profile files are parsed configuration, never shell code. Credential references resolve through a protected local file or supported credential store. Profiles and credentials stay outside tracked workspace files. Separate shell calls are not assumed to share exported variables; launchers or a configured process environment provide repeatable context.

## MCP and native tools

Provide MCP as a supported AI interface alongside the CLI and browser. Use the [official Go SDK](https://github.com/modelcontextprotocol/go-sdk) for protocol handling, tool schemas and transports; select a pinned release and supported protocol versions through host compatibility proof. The initial local stdio adapter is packaged with FireController, loads private profiles, and calls the service API through a typed client. It does not shell out to CLI commands or duplicate authorization, provider logic or storage. Remote Streamable HTTP MCP can be added at the service boundary without changing application behavior; its authentication and host compatibility require separate proof.

An adapter exposes compact operation tools using the same service API. If a host provides isolated connections with authenticated profile bindings, the visible call can be `inbox_list()` in both projects. A caller authorized for several profiles can select explicitly, such as `inbox_list(profile="project-builder")`, from its connection's allowed set. A selector supplies routing, not authorization: the adapter never loads another credential merely because a call names an available local profile.

Every selectable profile is authorized against the authenticated caller or a trusted connection binding. Indistinguishable callers sharing a connection share its effective permissions; an explicit selector does not provide security isolation between them. Different access grants require separate credential-bound connections unless the host can authenticate each caller independently. Client labels, worker names and unverified chat metadata do not establish that identity.

Tool schemas and results describe the selected operation with bounded structured fields, preserving the API's revisions, completeness, idempotency and error meanings. Protocol capabilities, client labels and tool annotations do not grant application access. SDK protocol handling never substitutes for the service's authentication and scope checks.

Binding is scoped to the authenticated client session and survives only through a verified reconnect. It must not be a server-global variable. Never infer project from the current working directory, a remembered worker name, or an unverified chat identifier. Where host metadata cannot distinguish callers, retain an explicit selector rather than guess.

## Client releases and additive reads

A running MCP connection keeps the executable and source revision with which it started. Replacing a bound launcher updates future client processes; it does not update another agent's running process. MCP `context` and bound CLI `context` expose `client_source_revision` without contacting the service. An incompatible older connection requires one host reconnect using its same private binding, not a review retry, evidence import or credential change. Invalid-response recovery reports this distinction directly; matching client failures still require investigation of the selected resource.

Compact operation-status readers ignore additional metadata fields and emit only their known typed fields. Required fields, exact casing, duplicate keys, known value types, state/action combinations, identities and response budgets remain validated. An added field cannot replace those existing meanings. Unknown metadata does not acquire authority or enter an agent's result. Tool arguments and mutation responses retain their strict contracts.

Hosted evidence readers retain provenance kinds as bounded labels. Inline-comment identity remains distinct; an extensible non-inline label must retain the existing review-bound source key, real review identity, fingerprint and location shape. Reading a compatible label does not authorize a new provider ingestion format, grant review credit or change finalization. The service's evidence admission remains responsible for its supported source formats.

## Quiet asynchronous waiting

The adapter exposes `wait_for_messages` for new incoming-message events after a supplied cursor, under the [quiet wait contract](message-wait.md). This notification does not claim that another connection has left those messages unread or unacknowledged. It returns a bounded result when one is available or when the wait expires; subsequent waits are separate calls. The adapter maintains the network subscription; keeping a connection open does not itself produce periodic model output. Whether a pending call permits unrelated model work is a host capability, not a property guaranteed by MCP.

A normal blocking tool invocation is not equivalent to a background subscription. Background shell handles, asynchronous tool completion, automatic result injection, turn completion, and waking an idle chat are separate host behaviors. Report capability only when established for the installed host; protocol notification support alone does not establish model-visible delivery or idle wakeup.

[Concurrent bounded review waits](../internal/mcpadapter/concurrent_review_wait_test.go) and [incoming-message SSE waits](../internal/mcpadapter/concurrent_message_wait_test.go) use their own HTTP transports; the adapter does not hold unrelated tool calls behind those waits. The same-session SSE fixture keeps an actual server subscription pending while unrelated native message and job mutations plus explicit same-key replays finish, then joins the cancelled subscription. Host scheduling and result delivery remain separate. A yielded or “still running” tool cell is not a service receipt or evidence that a mutation is blocked; a host may collect several calls together and defer a quick result behind another pending call. Collect the original result or handle rather than resending a mutation because its cell yielded. After a genuinely unknown write outcome, retain the exact request, key and binding under the recovery rules below.

The adapter bounds model-facing waits below the configured host timeout. An indefinitely pending MCP call is not a portable contract. Expiry returns an explicit timeout result; empty transport activity is suppressed where the host permits it. A persistent service subscription supplies waits without repeated provider polling.

Waits and read/list calls do not mark messages seen or acknowledged. Explicit seen and acknowledge mutations own those transitions; acknowledgment also establishes seen state atomically. Returned content cannot change the authenticated profile, grant permission or authorize an unrelated operation.

If automatic result delivery is unavailable, the adapter reports that limitation and still supports explicit inbox reads. It must not fake host wakeups. Durable server state retains undelivered messages independently of agent turns or open tool calls.

## State and reconnect semantics

[Job state](job-state.md) has one current progress record alongside the stable task brief. One guarded update changes selected fields and progress; omitted fields remain unchanged and history is automatic. The ordinary job read returns the full current brief/progress, remaining checklist/counts and pending attached-note previews under the completeness rules in [AI usability](ai-usability.md). Lists remain compact; full note bodies, additional pages, worker coordination and archives require deliberate reads.

Ending a turn, closing a client or cancelling a wait does not complete or pause a job, update progress, acknowledge messages, or delete durable resources. Subscription lifetime across turns depends on the host; cancellation releases the applicable wait without acknowledging delivery or reversing committed work.

Reconnect verifies the expected instance and authenticated project/worker binding before operations resume. Valid cursors continue replay; expired retention produces the explicit snapshot/resubscribe behavior owned by [architecture](architecture.md). Reconnect or a job read does not automatically resume a paused job.

The client imposes no end-of-work checklist or progress-update cadence. The separate [suggested agent workflow](agent-workflow.md) is optional guidance for projects and the [onboarding skill](../skills/firecontroller-onboard/SKILL.md).

## CLI behavior

Commands default to compact structured JSON for AI callers; explicit human formatting is an option. Quick mutations return a compact completed outcome, such as the affected resource ID and revision, without rereading full history. A completed mutation result is distinct from an operation receipt that says background work is still pending. Full briefs and archives require explicit reads. Pagination and history limits are visible, and the CLI never silently truncates an authoritative review claim. [AI usability](ai-usability.md) owns response scope and output budgets.

Exit codes distinguish success, argument errors, conflicts or holds, connection/provider failures, and cancellations. Diagnostics belong on stderr; machine-readable stdout contains only the documented response. A lightweight context/doctor command shows resolved instance, project, worker, and capability without printing credentials or performing a PR scan.

A guarded revision conflict reports a changed record and retains its safe current-revision detail. Recovery is to read the current record, preserve concurrent edits and reapply only the intended change with current guards and a new idempotency key. This is distinct from an authorization refusal or an uncertain mutation outcome. The client never retries a stale write or substitutes the latest guard automatically; uncertain outcomes retain the original request and key for recovery.

Mutation recovery retains the original request, binding and idempotency key, plus an operation ID when one was returned; retrying after a disconnect cannot create a second message or provider request. The API remains the authority for permissions and revisions even when an alias or tool hides context arguments.

Service identity verification precedes the selected operation. Transient connection/read timeouts or HTTP 502/503/504 can repeat only that identity GET, with the same credential and expected instance, for at most seven attempts inside the existing ten-second bootstrap budget and the caller’s cancellation/deadline. Retry waits begin at 250 milliseconds, then 500 milliseconds and one second, and cap at two seconds. Successful reads add no retry delay. Authentication refusals, redirects, malformed replies and instance mismatch do not retry. The selected operation is never dispatched during bootstrap or automatically repeated. CLI/MCP ordinary calls allow fifteen seconds overall for bootstrap plus their existing five-second operation limit; longer calls retain their existing total budgets and the MCP host ceiling. If transient bootstrap retries still fail, the client reports temporary service unavailability and that this attempt did not send the selected operation; a profile or credential change is not a prerequisite for recovery. A write retry retains its exact request, idempotency key and binding; a failed identity check does not establish the outcome of an earlier ambiguous write. Failures after write dispatch retain the existing unknown-outcome recovery contract.

An authenticated HTTP 503 `overloaded` read response means the controller temporarily could not complete the read; it does not diagnose an identity or access problem. Clients retain its code, status and `request_id`, provide same-read recovery guidance and omit private server prose. These failures do not start automatic polling. Mutation failures retain their separate exact-request/key unknown-outcome recovery contract.

Fast work returns its useful result directly. Variable-duration commands use a short bounded wait and return an operation receipt only when the work remains pending; explicit wait or immediate-return options let the caller choose. Known long-running work can detach immediately. Durable operation identity remains inside the adapter when possible. Where the host supports asynchronous execution and completion, a native review tool can stay pending and return its useful final result while the model continues independent work. A CLI wait can run through a background-capable shell handle where the installed host supports it; an explicit immediate return exposes the operation receipt. Ordinary reads and quick writes return their result directly. A receipt/poll workflow is a fallback for incapable hosts, not the mandatory agent experience.

## Actual host CLI wait proof

An actual Codex session exercised the unchanged clean `5ba5ed5b7ba620b4b32bfce2cd07c3b701887eea` CLI and daemon through a background shell handle. Two separately bound projects used the same worker display name. Six foreground context, job and inbox reads completed while the original message wait stayed silent. A committed message in the other project left that wait pending; the intended project's incoming message completed the same call with one matching event and `timed_out: false`. Reads and cancellation left both messages unseen and unacknowledged, original-cursor replay matched, and exact-key replay added no durable effects. Every unrelated retained table and both input binaries stayed unchanged; owned processes and private fixture state were cleaned.

The orchestrator explicitly inspected the original handle once to verify silence after the other project's message, then collected its completed result. A second wait returned cancellation exit code 6 without changing durable state. This demonstrates background CLI use in that installed session. It does not establish native MCP concurrency in the host, automatic result injection, delivery across turns, idle wakeup, model token cost or sustained throughput. The [packaged wait checks](../tools/packagedsmoke/smoke_test.go) and [same-session MCP checks](../internal/mcpadapter/review_wait_test.go) remain separate SDK/executable proof.
