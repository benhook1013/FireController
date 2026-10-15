# Job Progress

## Implementation status

The API, typed client, CLI, MCP and read-only browser implement separate narrative updates and a guarded checklist. Full repository checks/build, focused API/client/adapter race tests and actual packaged CLI/MCP/browser proof pass. Basic jobs and [checkpoints](job-checkpoints.md) remain available. [Optional primary-job focus](worker-focus.md) has a separate API and proof boundary. [Worker pause](worker-pause.md) is a separate coordination boundary. Deferred notes and a combined resume read remain unfinished.

## Separate progress from standing instructions

A narrative update records an immutable observation about a job. It never rewrites its brief, assignment, status, revision or revision history. Corrections are later updates; earlier attribution remains intact. A checkpoint is the structured done/next/blocker record; an update is a single body with optional evidence references. Neither substitutes for the current brief.

A checklist is one bounded, explicitly ordered resource belonging to a job. Its own revisions and immutable history record changes independently of the job's brief revisions. Replacing the checklist requires the current job revision and checklist revision. This prevents a plan read against an obsolete brief from silently becoming current. Job edits and checklist edits never rewrite one another's history.

These operations use the existing project permissions, worker attribution, strict JSON, explicit idempotency keys and transaction limits in the [local API](local-api.md). They perform no provider work, send no messages, change no inbox handling state and schedule no work. Successful mutations return compact receipts without their submitted content. Reads are explicit and have no durable side effects.

## Narrative updates

Routes are relative to `/api/v1/projects/{project_id}/jobs/{job_id}`.

| Endpoint | Request or result |
| --- | --- |
| `POST /updates` | Required `expected_job_revision`, `body`; optional `evidence`. Returns HTTP 201 with `{id, sequence, job_revision, outcome:"created"}`. |
| `GET /updates` | Explicit newest-first page `{items, cursor?, has_more}` using `limit` and `cursor`. |

Each retained update contains `id`, `project_id`, `job_id`, `job_revision`, `sequence`, `actor_id`, `worker_id`, `body`, `evidence` and `created_at`. The service captures attribution and the current job revision. An operator may have an empty worker ID. Evidence is always an array. Update sequences, rather than wall-clock ordering, define archive continuation. A later append does not duplicate or skip older entries behind a cursor.

Body must be nonblank UTF-8 of at most 8 KiB. Evidence permits at most ten nonblank references of at most 1,024 bytes each. Combined body and evidence are limited to 16 KiB. Accepted text and evidence order are preserved. C0 controls except tab, carriage return and newline, and DEL, are rejected. Omitted or null evidence normalizes to `[]` at the HTTP boundary; agent adapters reject explicitly null arguments before service work.

A stale job revision returns `revision_conflict` with `details.current_revision`, leaving updates, receipts and events unchanged. Exact-key replay returns its original receipt after current authorization, even if the job has since changed. Changed-body/key reuse conflicts. The append, compact event and receipt commit atomically. Event payloads contain job ID and sequence, never narrative text or evidence.

## Checklist replacement and history

| Endpoint | Request or result |
| --- | --- |
| `GET /checklist` | `{job_id, job_revision, revision, items}` from one authorized read snapshot. |
| `PUT /checklist` | Required `expected_job_revision`, `expected_revision`, `items`. Returns HTTP 200 with `{id, revision, job_revision, outcome}`; `id` is the job ID. |
| `GET /checklist/history` | Explicit newest-first revision page `{items, cursor?, has_more}` using `limit` and `cursor`. |

A job without a stored checklist reads as revision zero with `items:[]`. A missing job returns 404. The first changed replacement creates revision one. Items are ordered `{id, text, done}` objects: callers choose stable ASCII IDs matching `[A-Za-z0-9_-]{1,128}` and retain them when editing or reordering an item. IDs must be unique within the checklist. Text is nonblank UTF-8 of at most 1,024 bytes, using the update control-character rules. A checklist has at most 100 items and 16 KiB of combined item ID and text bytes. An empty array deliberately clears the checklist.

Both revision guards are checked before comparing content. A stale job returns `revision_conflict`; a stale checklist returns `checklist_revision_conflict` with `details.current_revision`. An identical ordered replacement returns `outcome:"unchanged"` at its existing revision and retains only its replay receipt. The checklist guard permits revisions zero through `MaxInt64-1`; the job guard permits any positive signed 64-bit revision. A changed replacement returns `outcome:"updated"`, increments the checklist revision and atomically records current state, full immutable checklist history, one compact event and its receipt. It never changes the job revision or appends job history. Reordering, completing, reopening and removing an item are ordinary guarded replacements; history retains the preceding state. Revision overflow is rejected before any effects.

Checklist history records `revision`, `job_revision`, `actor_id`, `worker_id`, `items` and `created_at`. New checklist revisions remain distinguishable from the captured job revision. Cursors bind instance, principal, project, job, route and descending revision order; a later replacement does not alter continuation through earlier history. Current/detail reads do not load history automatically.

## Agent clients and proof

CLI commands are `jobs update`, `jobs updates`, `jobs checklist`, `jobs checklist-set` and `jobs checklist-history`. Every mutation requires an explicit key and both applicable revision guards. Checklist input is a JSON array supplied with exactly one of `--items JSON` and `--items-file PATH`. The checklist guard is `--expect-checklist-revision`; the job guard is `--expect-revision`, shared with update/checkpoint commands. Input files must be regular files and are limited to 256 KiB before decoding; inspection before and after opening avoids accidental device/FIFO waits, without claiming protection against hostile filesystem races. Standard input is not implemented in this slice. Local validation precedes credentials, profile/network work and identity handshake. The typed client and bound MCP tools expose these same operations without deriving identity from a global current worker.

MCP names are `job_update_append`, `job_update_history`, `job_checklist_read`, `job_checklist_replace` and `job_checklist_history`. Update/checklist history defaults to 20 records and caps at 100; no adapter automatically fetches further pages. Current checklist reads have a 128 KiB transport ceiling and explicit archives 4 MiB, including JSON overhead. A service page above its encoded ceiling returns HTTP 413 `response_too_large`; clients also enforce their transport ceilings. A page above its ceiling fails explicitly; callers may request fewer records. Mutations and reads retain the existing five-second client budget, cancellation and uncertain-write diagnostics, with no automatic retries.

Required proof covers public scoped routes, strict content and aggregate limits, read-only/revoked/cross-project access, guarded races, exact replay after later changes, changed-body conflict, current/history agreement, sequence-based pagination during appends, rollback with original-key recovery and restart. Existing job/history/checkpoint bytes and message handling must remain unchanged. Backup inspection and inactive restore must support the new domain schema without changing older history. Actual packaged CLI/MCP and browser reads must demonstrate the same contracts before claiming those integrations complete; implementation tests alone do not prove host wakeup or fleet throughput.

## Executed proof

Clean checkpoint `b272698e550b767265d07ee58493adeadd4e612c` passed five actual packaged CLI/MCP groups in 2.287 seconds. Private `fc`, `fcd` and `fcmcp` executables exercised exact update text and attribution, original-key replay after brief edits, explicit paging, checklist completion/reopen/reorder/removal, independent history, stale guards, restart, read-only/revoked access and project/instance isolation. A controlled gateway damaged a successful append response after commit; the CLI reported uncertainty and explicit original-key recovery added no effect. Fifty tools were discovered through the official MCP SDK. Invalid CLI/file and raw nullable MCP inputs performed zero HTTP requests.

The run made 38 CLI commands, four fixture/SDK helper invocations and 83 daemon HTTP requests, with two daemon lifetimes and three MCP subprocesses. Synthetic setup used two controlled repository lookups; the workload made zero external HTTP, Git or CodeRabbit calls. The slowest CLI command took 69.145 ms and the slowest proxied HTTP request 23.789 ms. Source and executable hashes remained unchanged, and owned processes, listeners, credentials and database fixtures were cleaned. This proves local executable behavior, rather than fleet throughput or model-host wakeup. Backup/restore has separate focused API proof.

TypeScript and all 182 browser tests passed. Separate Chromium checks at 1440px and 390px demonstrated escaped text, read-only ordered completion, lazy archives, manual paging, close/reopen, refresh and disconnect behavior without overflow or external requests. That browser check used the development UI and controlled API responses; it is not packaged service/browser integration or physical-phone proof.

Clean checkpoint `99e92ec49c642278fb799feb10fe269a61f575f8` passed actual embedded-service browser checks at 1440px and 390px in 7.117 seconds. Real SQLite fixtures exercised separate standing briefs and checkpoints, ordered current checklists, lazy archives with explicit 20+1 paging, active-only refresh, close/reopen freshness, escaped text, session reload and disconnect. The browser made 62 API GETs and only session POST/DELETE writes; logical domain data remained unchanged. Source/assets/executable hashes stayed exact and owned processes/state were cleaned. The service and fixture helpers made no provider requests; 30 browser-engine background connection attempts were blocked separately, with no page-origin external requests.

Screenshot inspection found checklist content against the card edge. Reusing the existing padded content wrapper corrected it; focused actual packaged checks at both widths passed with the same content, ordering, archive and read-only assertions. Clean checkpoint `ddd9ad1646c4f2d2b9e7bc0cbd78283aa7352cfa` then passed full `make check build`: 182 browser tests, all Go race tests/vet, documentation checks, generated assets and three binaries. An earlier combined run failed because an inherited capture-store test had not been adapted to its newer record type; that fixture was corrected before the passing run. These results do not establish installed deployment, physical-phone behavior, accessibility conformance, model-host wakeup or fleet performance.
