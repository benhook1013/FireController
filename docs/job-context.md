# Job Resume Context

## Implementation status

The combined API, typed client, CLI and MCP read implement the contract below. Focused public-handler, typed-client and official-SDK adapter tests pass, including race checks. Full combined validation and actual packaged CLI/service/official-SDK proof pass at the source checkpoint recorded below. The individual current-state and explicit archive reads remain independent.

## Deliberate selection

A context read selects one explicit job in the bound project and the caller's explicitly bound worker. It provides the authoritative current brief, latest checkpoint and selected pending coordination in one authorized database snapshot. It does not select a job automatically, change focus, resume execution or treat message/note content as authorization. A project reader can select another worker's job under the existing job-read permissions; the pending coordination still belongs to the bound caller, whose identity is returned explicitly.

`GET /api/v1/projects/{project_id}/jobs/{job_id}/context` requires the existing authenticated worker header and accepts only an optional `limit`, default 20 and maximum 100. Missing worker binding is refused with `worker_required`; ordinary scoped authorization, expected-instance checks and missing-resource behavior apply. The limit bounds each pending page independently. A context call never paginates automatically or fetches providers, review state, histories or conversation archives.

The response contains:

| Field | Content |
| --- | --- |
| `project_id`, `worker_id` | Explicit project and bound caller identities. |
| `job` | One complete current job, including its untruncated brief and revision. |
| `checkpoint` | The full latest checkpoint, or explicit null when absent. Its captured job revision may precede the current job revision. |
| `checklist` | Complete current checklist, with the selected job's current revision and the independent checklist revision. |
| `primary` | Bound worker's compact current primary selection, including explicit absence. It need not identify the selected job. |
| `pause` | Bound worker's current pause state, reason and independent revision. Reading never clears it. |
| `notes` | One compact pending-note page for the bound worker OR jobs currently assigned to that worker. |
| `inbox` | One compact incoming-message page for the bound worker, restricted to unacknowledged messages, including seen but unhandled messages. |
| `event_cursor` | Incoming-message event watermark taken in the same snapshot. |
| `complete`, `omitted` | Exact completeness disclosure described below. |

Notes retain the selection, ordering, preview and cursor rules of [deferred notes](notes.md): `worker_id` is the bound worker, `include_assigned_jobs=true`, `status=pending`, with no other filters. This selection is worker coordination, not every note attached to the selected job or an implicit inclusion of phase-only/project-wide notes. Explicit ordinary note reads cover other scopes. Inbox selection uses `unread=false` and `unacknowledged=true`, with the existing oldest-first sequence ordering and bounded previews. The [message feed](message-events.md) owns replay and retention-expiry behavior.

## Completeness and continuation

The brief, latest checkpoint and current checklist are always complete on a successful response. They are never silently shortened to fit pending coordination. The two pending sections contain selection previews, not full authoritative bodies. `omitted` is a required array in this canonical order, containing only the applicable values:

1. `note_bodies` when at least one note is present, including a short preview that happens to contain the complete text.
2. `additional_notes` when the note page has more results.
3. `message_bodies` when at least one message is present.
4. `additional_messages` when the inbox page has more results.

`complete` is true exactly when this array is empty. It describes the selected packet, not all project knowledge or archived work. Unselected archives and other workers' coordination are outside this operation's scope. A caller must not interpret a partial packet as containing every standing instruction or full pending request. The current brief remains authoritative; previews are useful for selecting explicit full reads, not for reconstructing omitted content.

Note cursors continue through the ordinary notes list with the same bound-worker/assigned-job/pending filters. Inbox cursors continue through the ordinary inbox list or snapshot with `unread=false`, `unacknowledged=true`. Neither continuation reloads the job brief. Full note content uses a selected current note read; full message content uses an explicit conversation read. Existing cursor binding prevents changing project, worker, principal or filters during continuation. The event cursor starts the bound incoming feed without acknowledging messages or discarding unhandled work from the snapshot.

The entire encoded response has a 1 MiB budget; each section retains its existing content limits. An unexpectedly oversized packet fails with `context_too_large` and no successful partial object. Its safe error directs the caller to the ordinary job, latest-checkpoint, checklist, worker, note and inbox reads. Database errors and malformed retained state likewise fail the operation rather than inventing empty sections. No history is decoded to corroborate an ordinary current read.

## Agent interfaces

The typed client method is `JobContext(ctx, jobID, limit)`; zero selects the default limit of 20, while explicit CLI/MCP limits must be 1–100. CLI is `fc jobs context JOB_ID [--limit N]`. MCP adds the read-only, closed-world `job_context` tool with required `job_id` and optional `limit`; discovery then exposes 63 tools, 41 reads and 22 mutations. Each interface inherits the private profile's instance/project/worker binding, validates arguments before credentials/network and uses the existing total five-second client budget. A normal call returns directly, without an operation receipt, automatic retry or background job.

Strict response validation requires the nullable checkpoint field to be present, all other required fields including false/zero/empty-array values, the exact bound identity and requested job, consistent current job/checklist revisions, structurally valid timestamps, current independent worker states, valid compact pages and a nonempty event cursor. A checkpoint's captured revision may be older but not newer than the selected job. Unknown or duplicate fields, null required fields other than the explicitly nullable checkpoint, invalid previews, mixed targets and a completeness declaration inconsistent with the actual pages are invalid responses.

The operation makes no durable write, note consumption, message seen/ack transition, checkpoint, job/focus/pause edit or provider call. It adds no end-of-work policy: projects choose when to use it through the optional [agent workflow](agent-workflow.md).

## Proof boundary

Required proof covers complete empty coordination, latest versus absent checkpoints, authoritative large escaped briefs, current checklist/focus/pause, worker and assigned-job note scope, seen-but-unacknowledged messages, exact omission disclosure, manual continuation without repeating briefs, and snapshot-to-event replay. Verify same-snapshot consistency under concurrent reassignment/coordination changes, project isolation, revocation, instance replacement, strict missing/null responses, invalid local input before credentials/network, bounded cancellation/deadlines and zero durable/provider effects. Current reads must not expand archives. Actual packaged CLI and official-SDK MCP calls must exercise the combined service endpoint before provider review. This does not establish model-host wakeup, browser integration or operational cutover.

## Executed proof

At clean source `9a262f8c75d81dbeccd58a95abb36e79481a7bb7`, full `make check build` passed: 189 browser tests, all Go race tests and vet, formatting, documentation and 19 link tests, generated assets and all three release binaries. The independently built packaged binaries match those full-build binaries byte for byte.

Four actual packaged groups passed on their first run in 9.200 seconds. Actual CLI/service and official Go MCP SDK processes proved complete empty coordination, explicit null and older checkpoints, current checklist/focus/pause, exact 32 KiB escaped briefs, bound-worker note/message scope, the four omission markers, manual pending-page continuation, selected full reads and snapshot-to-event replay. Restart, scoped access/revocation, invalid local inputs before credential/network access and read-only logical-state comparisons passed. Ordinary logged subprocesses took at most 87.458 ms on this developer machine; that small sample is not a fleet latency guarantee.

A gateway separately held the response of an already completed context read: cancellation returned in 8.916 ms and the existing total deadline returned `deadline_exceeded` in 5006.721 ms, each with one domain read, no retry and no durable effect. These cases prove response-transport cancellation/deadlines, not database fault injection. All owned processes and ports closed after proof. Provider and review executable traps stayed at zero.

This is local Linux packaged proof with controlled identities. Model-host wakeup/resume, browser integration, installed-service deployment, physical-phone use, fleet throughput and operational cutover are not established by these checks. The installed QA site and primary development controller are unchanged.

## Transient storage error proof

Focused HTTP/SQLite checks after the executed checkpoint above separately lock each checklist, checkpoint, primary-focus and pause helper table in an attached disposable database while the initial job and authorization remain readable. The combined route now returns retryable `overloaded` (503) for the verified SQLite busy error and recovers after unlock. Existing malformed-retained-section cases still return `storage_unavailable` (500). Focused normal/race checks and vet pass. This does not separately force SQLite locked errors or cancellation inside a helper.
