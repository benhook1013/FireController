# Agent and Client Contract

## Implementation status

Profiles, native tools, CLI aliases, and subscriptions described here are proposed. The temporary legacy bootstrap is documented separately in [delivery](delivery.md). Codex host behavior has not yet been established by the application proof.

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

## Native tools

A native-tool adapter uses the same service API. If a host provides isolated connections with authenticated profile bindings, the visible call can be `inbox_list()` in both projects. If a connection is shared across agents or projects, calls require an explicit selector such as `inbox_list(profile="beta-overseer")`.

Binding is scoped to the authenticated client session and survives only through a verified reconnect. It must not be a server-global variable. Never infer project from the current working directory, a remembered worker name, or an unverified chat identifier. Where host metadata cannot distinguish callers, retain an explicit selector rather than guess.

## Quiet asynchronous waiting

An orchestrator may launch `wait_for_messages` while doing unrelated work only when its host supports asynchronous tool execution. The tool waits for relevant unread incoming messages after a supplied cursor, returns a bounded result when one is available, and is rearmed by the caller. The adapter maintains the network subscription; keeping a connection open does not itself produce periodic model output.

A normal blocking tool invocation is not equivalent to a background subscription. Background shell handles, asynchronous tool completion, automatic result injection, turn completion, and waking an idle chat are separate host behaviors. Test the installed host before claiming immediate delivery or support for an indefinitely pending call.

Use a bounded wait shorter than the configured host timeout. In Codex integrations, ordinary MCP tools may have a configured timeout, so an arbitrary forever-pending MCP call is not a portable contract. If the wait expires, return an explicit timeout receipt; the adapter should suppress repeated empty model notifications where the host permits it. Prefer a persistent adapter subscription with a bounded model-facing wait over repeated provider polling.

The wait does not mark messages seen or acknowledged and does not act on their contents. A returned event is external tool-level information, not human authorization. The agent reads the relevant conversation, handles work within its existing scope, then explicitly marks handling complete. Messages needing human decisions remain unacknowledged until that decision or handoff is handled.

If automatic result delivery is unavailable, the adapter reports that limitation and supports checking pending messages at safe boundaries. It must not fake host wakeups. After an agent turn ends, durable server state retains undelivered messages; keeping a tool open is not required for message survival.

## End-of-work and resume

At a material milestone, pause, handoff, blocker, or completion, the worker records a concise job update or checkpoint with done, next step, and evidence pointers. Standing instructions belong in the current brief, not solely in an update. Routine edits and individual test runs do not require redundant job updates.

Before ending active work, check pending messages once, handle or identify outstanding coordination, save a checkpoint if resume context changed, and report the remaining state. Cancel any turn-owned wait safely or preserve it only if the host proves subscription lifetime across turns. Cancellation never acknowledges messages. This is not a requirement to stay artificially busy or poll forever after completion.

On resume, read the current brief, latest checkpoint, and pending messages; a compact snapshot alone is not sufficient standing context. Attach using the private profile and resume transport cursors where valid. If replay retention expired, use the snapshot/resubscribe sequence owned by [architecture](architecture.md).

## CLI behavior

Commands default to compact structured JSON for AI callers; explicit human formatting is an option. Successful mutations return compact receipts rather than reread full history. Full briefs and archives require explicit reads. Pagination and history limits are visible, and the CLI never silently truncates an authoritative review claim. [AI usability](ai-usability.md) owns response scope and output budgets.

Exit codes distinguish success, argument errors, conflicts or holds, connection/provider failures, and cancellations. Diagnostics belong on stderr; machine-readable stdout contains only the documented response. A lightweight context/doctor command shows resolved instance, project, worker, and capability without printing credentials or performing a PR scan.

Client timeouts and retries use the operation receipt and idempotency key; retrying after a disconnect cannot create a second message or provider request. The API remains the authority for permissions and revisions even when an alias or tool hides context arguments.

For long actions, durable operation identity remains inside the adapter when possible. A native asynchronous review tool can stay pending and return its useful final result while the model continues independent work. The CLI can submit and wait through a background-capable shell handle; explicit detach returns an operation receipt. Ordinary reads and quick writes return their result directly. A receipt/poll workflow is a fallback for incapable hosts, not the mandatory agent experience.
