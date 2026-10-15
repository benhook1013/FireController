# Worker Pause State

## Implementation status

The worker-pause API, storage, typed client, CLI and MCP adapters are implemented. Focused normal/race tests, full combined validation and actual packaged proof pass at the checkpoints below. Browser integration remains unfinished. [Primary focus](worker-focus.md) and ordinary job status already work independently.

## Explicit state

A worker has an explicit paused flag within its project, an optional short reason and an independent revision. The initial state is `{worker_id, revision:0, paused:false, reason:""}`. Pausing records a coordination choice; it does not stop an agent, deny mutations, change job status or assignment, clear primary focus, acknowledge messages, or pause review execution. New assignments, progress writes, reconnects and ordinary restarts preserve the pause. Resuming explicitly clears the flag and reason without changing jobs.

A reason is valid UTF-8, at most 1024 bytes and contains no control characters except newline and tab. A paused worker may have an empty reason. An unpaused replacement requires an empty reason. Reasons remain private project data and are not included in compact events. There is no heartbeat, inferred presence, automatic resume or combined checkpoint mutation. A caller can separately append an ordinary job checkpoint using its existing guarded API.

## API and history

Routes are relative to `/api/v1/projects/{project_id}/workers/{worker_id}`.

| Endpoint | Request or result |
| --- | --- |
| `GET /pause` | Current `{worker_id, revision, paused, reason}` only. |
| `PUT /pause` | Required `expected_revision`, `paused` and `reason`; returns `{id, revision, outcome}`. |
| `GET /pause/history` | Explicit newest-first `{items, cursor?, has_more}`, default 20 and maximum 100. |

Every field is explicit; missing/null fields, unsupported fields, malformed identities and guards outside zero through `MaxInt64-1` fail before mutation. Project read/write grants govern access; the target worker is independent of the authenticated worker attribution. Missing targets return `worker_not_found`. A stale guard returns HTTP 409 `worker_pause_revision_conflict` with `details.current_revision`, including zero.

A changed replacement atomically stores current state, advances its revision, appends immutable history and one compact `worker.pause_changed` event, and retains the explicit-key receipt. An identical replacement returns `outcome:"unchanged"` at the current revision with only a receipt. Current authorization governs exact-key replay; it returns the original receipt without repeating effects. Revision order defines history. History entries retain `worker_id`, `revision`, `paused`, `reason`, `actor_id`, `actor_worker_id` and `created_at`. Event data contains the worker and paused boolean, never the reason. Existing principal/project/instance/route/worker-bound paging applies.

This slice adds jobs schema five with `worker_pause_state` and `worker_pause_history`, retaining supported older job schemas in backup inspection and inactive restore. Exact column definitions, primary keys and foreign keys must be validated before restore writes. Startup migration creates empty tables without modifying existing jobs, focus, receipts, events or identity.

## Bound tools and presentation

Typed client operations read, replace and page history. Profile-bound CLI commands are `workers pause-state`, `workers pause-history`, `workers pause --reason TEXT --expect-revision N --key KEY`, and `workers resume --expect-revision N --key KEY`. `--reason` is explicit for pause, including an empty string. MCP tools are `worker_pause_read`, `worker_pause_replace` and `worker_pause_history`; replacement requires the same full state as the API. Tool calls inherit project/worker from the private profile. Strict validation precedes credential/network access, the existing five-second total budget applies, and uncertain responses recover with the original key. Reads perform no provider work, durable writes, automatic history reads or message acknowledgments.

A later browser view shows explicit pause and its reason for the selected worker without inferring agent presence or changing job presentation. Until that integration is implemented, the API and bound tools are the pause surface. Do not add one automatic pause request per worker to routine project lists.

## Required proof

Functional proof covers default state, pause/reason correction/resume, no-op, stale and racing replacements, changed-key conflict, original-key replay after later changes and restart, authorization/revocation/cross-project isolation, target versus actor attribution, bounded history and cursor binding, revision overflow, and atomic rollback. Jobs, primary focus, messages and review settings must remain unchanged. A new assignment and ordinary progress while paused must retain the pause. Backup/inspection/inactive restore preserve current state and history; malformed current/history schemas fail before writes. Actual CLI and official-SDK MCP calls must exercise the API, including a committed response lost across adapters and a zero-revision conflict. Browser, installed-site, model-host and fleet behavior require their separate proof.

## Focused proof status

Public API tests cover lifecycle and independent state, unchanged jobs/focus/inbox/review policy, paused assignments and progress, replay, races, rollback, guards, authorization, cursor binding and schema-four migration/restart. All jobs normal/race tests passed. Actual bundle tests preserve pause/history, accept older jobs schemas one through four, and reject sixteen rehashed malformed current/history table shapes before restore writes. Focused backup race tests and vet passed.

Typed-client, CLI and official-SDK MCP normal/race tests pass against the actual API, including zero-revision conflict details, a lost committed response with original-key recovery, strict pre-credential validation, bounded paging, cancellation and the aggregate five-second budget. Current reads have an 8 KiB wire budget to accommodate JSON escaping of a valid 1024-byte reason. Discovery exposes fifty-six tools. Packaged execution is recorded separately below; installed pause behavior, browser integration and model-host behavior remain separate.

## Packaged and combined proof

At clean source `c9df4624dd41e9b08fb7066d3548b59ebf9b1632`, full `make check build` passed: browser checks and 182 tests, all Go race tests, vet, documentation checks and 19 link-checker tests, generated assets and all three binaries. Their hashes exactly matched the independent private packaged build.

Six actual packaged API/CLI/official-SDK MCP groups passed on the first run in 2.698 seconds. They exercised false/empty defaults and no-op receipts, a 1024-byte escaped reason, correction/resume, revision-zero conflicts, original-key recovery across adapters after a committed response was damaged, ordinary assignments/progress/focus while paused, manual history paging, access and instance refusals, restart and backup/inspection/inactive restore preserving one pause row and six history rows. The run made 42 CLI calls, four operator calls and ten helper calls, plus SDK adapter children and two owned daemons; its gateway recorded 95 HTTP requests. No external provider or Git calls occurred, and owned processes, listeners and disposable state were removed.

These small local timings are not fleet or remote latency guarantees. The installed QA runtime remains the earlier job-progress checkpoint; this proof does not deploy pause state, establish browser presentation or prove host wakeup/resume. The retained old controller remains primary pending implementation completion and human GUI review.
