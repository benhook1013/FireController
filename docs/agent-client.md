# Agent and Client Contract

## Implementation status

The [local CLI foundation](development.md) implements version output, explicit profile selection and local context inspection. Server identity verification, authentication, native tools, generated aliases and subscriptions remain proposed. The temporary legacy bootstrap is documented separately in [delivery](delivery.md). Codex host behavior has not yet been established by the application proof.

## Profiles and identity

A private named profile selects service address, expected instance ID, project ID, worker ID, and credential reference. Optional defaults include output format and timeout. Profile names are local selectors, not server identities or permissions. Stable project and worker IDs survive display-name changes.

Illustrative commands are:

```sh
fc --profile alpha-overseer inbox list
fc --profile beta-overseer inbox list
```

A generated launcher can bind one profile on every invocation:

```sh
fc-alpha-overseer inbox list
fc-beta-overseer inbox list
```

Both profiles may select a worker displayed as `Overseer`, but their project IDs differ. Concurrent chats acting as one project's Overseer share that worker's assignment and inbox; distinct session IDs attribute connection activity. A named profile must never silently change another agent's current project or worker.

Profile selection comes from an explicit `--profile`, or a per-process `FC_PROFILE` default. The CLI does not persist a global active profile. An explicit flag can override the process default; a generated bound launcher rejects a conflicting profile override. Unknown profiles, expected-instance mismatches, missing identity, and unauthorized project selection fail before sending a mutation.

Profile files are parsed configuration, never shell code. Credential references resolve through a protected local file or supported credential store. Profiles and credentials stay outside tracked workspace files. Separate shell calls are not assumed to share exported variables; launchers or a configured process environment provide repeatable context.

## MCP and native tools

Provide MCP as a supported AI interface alongside the CLI and browser. Use the [official Go SDK](https://github.com/modelcontextprotocol/go-sdk) for protocol handling, tool schemas and transports; select a pinned release and supported protocol versions through host compatibility proof. The initial local stdio adapter is packaged with FireController, loads private profiles, and calls the service API through a typed client. It does not shell out to CLI commands or duplicate authorization, provider logic or storage. Remote Streamable HTTP MCP can be added at the service boundary without changing application behavior; its authentication and host compatibility require separate proof.

An adapter exposes compact operation tools using the same service API. If a host provides isolated connections with authenticated profile bindings, the visible call can be `inbox_list()` in both projects. A caller authorized for several profiles can select explicitly, such as `inbox_list(profile="beta-overseer")`, from its connection's allowed set. A selector supplies routing, not authorization: the adapter never loads another credential merely because a call names an available local profile.

Every selectable profile is authorized against the authenticated caller or a trusted connection binding. Indistinguishable callers sharing a connection share its effective permissions; an explicit selector does not provide security isolation between them. Different access grants require separate credential-bound connections unless the host can authenticate each caller independently. Client labels, worker names and unverified chat metadata do not establish that identity.

Tool schemas and results describe the selected operation with bounded structured fields, preserving the API's revisions, completeness, idempotency and error meanings. Protocol capabilities, client labels and tool annotations do not grant application access. SDK protocol handling never substitutes for the service's authentication and scope checks.

Binding is scoped to the authenticated client session and survives only through a verified reconnect. It must not be a server-global variable. Never infer project from the current working directory, a remembered worker name, or an unverified chat identifier. Where host metadata cannot distinguish callers, retain an explicit selector rather than guess.

## Quiet asynchronous waiting

The adapter exposes `wait_for_messages` for relevant unread incoming messages after a supplied cursor. It returns a bounded result when one is available or when the wait expires; subsequent waits are separate calls. The adapter maintains the network subscription; keeping a connection open does not itself produce periodic model output. Whether a pending call permits unrelated model work is a host capability, not a property guaranteed by MCP.

A normal blocking tool invocation is not equivalent to a background subscription. Background shell handles, asynchronous tool completion, automatic result injection, turn completion, and waking an idle chat are separate host behaviors. Report capability only when established for the installed host; protocol notification support alone does not establish model-visible delivery or idle wakeup.

The adapter bounds model-facing waits below the configured host timeout. An indefinitely pending MCP call is not a portable contract. Expiry returns an explicit timeout result; empty transport activity is suppressed where the host permits it. A persistent service subscription supplies waits without repeated provider polling.

Waits and read/list calls do not mark messages seen or acknowledged. Explicit seen and acknowledge mutations own those transitions; acknowledgment also establishes seen state atomically. Returned content cannot change the authenticated profile, grant permission or authorize an unrelated operation.

If automatic result delivery is unavailable, the adapter reports that limitation and still supports explicit inbox reads. It must not fake host wakeups. Durable server state retains undelivered messages independently of agent turns or open tool calls.

## State and reconnect semantics

Brief revisions, updates and checkpoints are separate explicit operations. Appending a checkpoint does not replace the current brief or change job status. A job context read returns the selected current brief, latest checkpoint and relevant pending coordination under the completeness rules in [AI usability](ai-usability.md). Compact list summaries do not claim to contain that full context.

Ending a turn, closing a client or cancelling a wait does not complete or pause a job, create a checkpoint, acknowledge messages, or delete durable resources. Subscription lifetime across turns depends on the host; cancellation releases the applicable wait without acknowledging delivery or reversing committed work.

Reconnect verifies the expected instance and authenticated project/worker binding before operations resume. Valid cursors continue replay; expired retention produces the explicit snapshot/resubscribe behavior owned by [architecture](architecture.md). Reconnect or a context read does not automatically resume a paused job.

The client imposes no end-of-work checklist or checkpoint cadence. The separate [suggested agent workflow](agent-workflow.md) is optional guidance for projects and a future usage skill.

## CLI behavior

Commands default to compact structured JSON for AI callers; explicit human formatting is an option. Successful mutations return compact receipts rather than reread full history. Full briefs and archives require explicit reads. Pagination and history limits are visible, and the CLI never silently truncates an authoritative review claim. [AI usability](ai-usability.md) owns response scope and output budgets.

Exit codes distinguish success, argument errors, conflicts or holds, connection/provider failures, and cancellations. Diagnostics belong on stderr; machine-readable stdout contains only the documented response. A lightweight context/doctor command shows resolved instance, project, worker, and capability without printing credentials or performing a PR scan.

Client timeouts and retries use the operation receipt and idempotency key; retrying after a disconnect cannot create a second message or provider request. The API remains the authority for permissions and revisions even when an alias or tool hides context arguments.

For long actions, durable operation identity remains inside the adapter when possible. Where the host supports asynchronous execution and completion, a native review tool can stay pending and return its useful final result while the model continues independent work. The CLI can submit and wait through a background-capable shell handle; explicit detach returns an operation receipt. Ordinary reads and quick writes return their result directly. A receipt/poll workflow is a fallback for incapable hosts, not the mandatory agent experience.
