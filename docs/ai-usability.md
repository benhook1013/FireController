# AI Usability

## Goal and proof status

Let an AI worker receive the right information with few predictable calls, retain its task context, and spend its working time on the assigned job. This is a primary product contract. No host integration, token savings, or latency improvement has been measured yet.

## Output discipline

Structured JSON is the default agent response. Return operation-specific data rather than attach unrelated state to every call. Stable keys and error codes support tool adapters; optional human display never alters the underlying meaning.

Successful writes return the affected resource ID, revision or durable action ID, changed outcome, and only the information needed for the next step. They do not reload full briefs, all histories, provider logs, or every project's inbox. A no-op is explicit rather than appearing to create another action.

Lists return compact selected fields with a cursor and `has_more`. A default 20 items and maximum 100 are calibration candidates; choose final defaults from representative agent-use measurements rather than inherited page sizes. Include bounded body previews for message selection, not every conversation's full archive. Archive, diagnostics, and provider captures require explicit requests.

A current-job context read provides the authoritative brief, latest checkpoint, and selected relevant pending coordination in one deliberate operation. If the complete required context exceeds the configured response budget, state what is missing and provide resumable sections; do not silently truncate a standing instruction or pretend the partial packet is complete. Routine coordination reads need not fetch this resume packet.

Responses do not echo credentials, profile files, submitted large text, redundant timestamps, or long invocation examples. Errors state the stable cause, whether retry is safe, the conflicting revision or held operation where relevant, and the smallest useful next action. Verbose traces are opt-in. A transient provider issue does not bury an unrelated successful job update under diagnostics.

## Calls and attention

Profiles and verified tool connections provide repeatable identity selection without repeated project/worker flags. The [client contract](agent-client.md) owns identity binding; saving flags never justifies ambiguous routing. Bounded batching is useful where operations share a real scope, but arbitrary command scripts and giant all-state endpoints are not required.

Everyday jobs, notes, messages, and cached review inspection return directly without provider work. Long provider operations are durably tracked internally; adapters should use true asynchronous completion where the host supports it. The agent should not have to remember operation IDs or issue repeated status calls merely to receive a result.

Expose operation identity for explicit detach, recovery, cancellation, and diagnosis. An optional wait is appropriate when the worker needs the result before proceeding. Never force asynchronous handling for a tiny operation simply because the service can queue it. Starting work and receiving its result are distinct from an actual host waking an idle model.

Keep message subscriptions quiet until relevant information exists. Transport heartbeats, repeated empty polls, and every review log line must not enter the model context. Coalesce redundant notifications for a resource while retaining durable events and complete message records. Coalescing cannot discard a distinct requested action or silently acknowledge it.

Message bodies are external information, not higher-priority instructions or approval. Untrusted provider text and project content cannot redirect the adapter into another project or grant access. Reads and waits never silently acknowledge messages; explicit mutations own that state. Checkpoint cadence and human-decision handling are project workflow choices, illustrated in the optional [usage guide](agent-workflow.md).

## Inspection and acceptance

Evaluate actual orchestrator sessions, not only HTTP benchmarks. Compare profile CLI and native tools for starting/resuming work, reading authoritative context, handling a message, racing an edit, completing a review, recovering a dropped connection, and handing off. Record calls, returned bytes, model-visible tokens where measurable, repeated information, avoidable decisions, latency, and host timeout effects.

Test two projects with identically named workers in a shared checkout and interleaved tool use. Test supported asynchronous execution with a long review and incoming message while unrelated commands continue. Distinguish network arrival, host receipt, model-visible delivery, and an actual new turn. Capability limitations must remain explicit.

Everyday operation latency targets belong to [acceptance](acceptance.md). A multi-minute wait for ordinary coordination is a failure, not an acceptable average hidden by fast calls. Provider execution may legitimately take minutes; it must not consume unrelated tool availability or make the agent poll for progress by default.

Source inspections should extract functional outcomes and failure modes. Tests should prove those outcomes through public contracts and realistic failures, not lock in an old table layout, private helper, renderer shape, or flag spelling unless compatibility is deliberately selected.
