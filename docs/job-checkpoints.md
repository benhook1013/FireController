# Job Checkpoints

## Implementation status

The checkpoint API, CLI and MCP tools are implemented in [PR #7](https://github.com/benhook1013/FireController/pull/7). Focused public tests, the full repository checks/build and six actual packaged CLI/MCP groups pass. This page owns their selected contract. Checkpoints do not provide primary-job selection or a complete resume packet.

## Record progress without changing the job

A checkpoint is an immutable record of work completed, the next step, an optional blocker and evidence references. It captures the job revision and server-derived actor/worker attribution. Appending one never changes the current job brief, assignment, status, revision or revision history. The current brief remains the standing context; an older checkpoint cannot override it.

An append requires an explicit idempotency key and the current job revision. A stale revision returns `revision_conflict` with `details.current_revision`, without recording anything. The checkpoint, compact event and retained receipt commit atomically. Exact-key replay returns the original receipt without another checkpoint or event, after current authorization. Recover an uncertain write with the same key, request and binding; never silently create a new key.

The service performs no provider work for checkpoint operations. Events contain only job ID and checkpoint sequence, never checkpoint text or evidence. A checkpoint does not send a message, mark one seen or acknowledge one.

## API

Routes are relative to `/api/v1/projects/{project_id}/jobs/{job_id}` and use the existing [authentication, scope and idempotency rules](local-api.md#start-and-connect).

| Endpoint | Request or result |
| --- | --- |
| `POST /checkpoints` | Required `expected_job_revision`, `done`, `next`; optional `blocker`, `evidence`. Returns HTTP 201 with `{id, sequence, job_revision, outcome:"created"}`. |
| `GET /checkpoints/latest` | Returns `{job_id, job_revision, checkpoint}` from one authorized read snapshot. `checkpoint` is explicitly `null` when the job exists without checkpoints; a missing job returns 404. No query fields. |
| `GET /checkpoints` | Explicit archive page `{items, cursor?, has_more}`; optional `limit` and `cursor`, default 20 and maximum 100. Newest sequence first. |

Full checkpoint fields are `id`, `project_id`, `job_id`, `job_revision`, `sequence`, `actor_id`, `worker_id`, `done`, `next`, `blocker`, `evidence` and `created_at`. An operator can have an empty worker ID. Evidence is always an array, including `[]` when omitted. The latest envelope reports the current job revision; its checkpoint can capture an earlier revision. The service returns both faithfully rather than repairing or hiding that difference.

Archive cursors bind the instance, principal, project, job and descending sequence order. Appending between pages does not duplicate or skip the older entries behind the continuation boundary. Reads never advance handling state or write progress. Archives are fetched only on explicit request.

`done` and `next` must each contain nonblank UTF-8 text of at most 8 KiB. `blocker` is optional, at most 4 KiB. Evidence contains at most ten nonblank references of at most 1,024 bytes each. Combined text and evidence are limited to 16 KiB of UTF-8. Accepted text and evidence order are preserved. C0 control characters other than tab, carriage return and newline, and DEL, are rejected. Invalid content returns `validation_failed`; a revision below one returns `invalid_revision`. Unknown, duplicate or mis-cased input fields follow the strict JSON rules of the local API. At the HTTP boundary, an omitted or null optional blocker means no blocker, and omitted or null evidence is normalized to `[]`. CLI and MCP reject explicitly null arguments before service work.

## Agent interfaces

```sh
fc jobs checkpoint JOB_ID --key KEY --expect-revision 3 --done "Finished the API change" --next "Run the packaged workflow" --evidence "commit:EXAMPLE"
fc jobs checkpoint-latest JOB_ID
fc jobs checkpoints JOB_ID --limit 10
```

Only `--evidence` is repeatable, up to ten occurrences. Invalid arguments fail before profiles, credentials or network work. Appending returns only its compact receipt. Latest and archive reads explicitly return checkpoint content; neither fetches the full job brief automatically. Read the current job separately when needed. The client bounds latest responses to 128 KiB and explicit archives to 4 MiB, including JSON encoding overhead. These are transport ceilings; accepted checkpoint content has the smaller limits above. A large archive page can exceed the transport ceiling and returns an error rather than truncated records; request a smaller limit. The client does not automatically fetch additional pages.

MCP provides `job_checkpoint_append`, `job_checkpoint_latest` and `job_checkpoint_history` through the same typed client. They use the connection's immutable profile binding. Appending requires `job_id`, `idempotency_key`, `expected_job_revision`, `done` and `next`, with optional `blocker` and an `evidence` array. Latest requires `job_id`; history additionally accepts optional `limit` and `cursor`. The adapter never generates a key, retries automatically or writes a checkpoint as a side effect of another tool.

CLI and MCP retain the shared five-second identity-and-operation budget and normal cancellation/uncertain-write diagnostics. The service retains its two-second transaction bounds. Suggested checkpoint timing belongs in [optional agent workflow guidance](agent-workflow.md), rather than a tool-enforced end-of-work ritual.

## Browser reads

The [read-only browser](browser-guide.md) displays the latest checkpoint beside the explicit job brief. Checkpoint archives load only on request and page newest first. The browser preserves the 128 KiB latest and 4 MiB archive transport limits, validates parsed shape/scope/content and never writes progress or handling state. It retains the existing JSON parser convention; this does not claim raw duplicate-key rejection. Focused parsed-response tests and [actual packaged browser proof](browser-guide.md#checkpoint-browser-proof) pass, including lazy 20+1 paging and escaped evidence.

## Proof and limits

Public API tests cover immutable job/history state, attribution, authorization/revocation, stale revision with no effects, exact replay and changed-body conflict, stable archive continuation under a later append, rollback with same-key retry and ordinary restart retention. Boundary tests exercise exact and overflowing aggregate, blocker and evidence limits, control characters, accepted whitespace preservation and optional-field normalization. Client/CLI/MCP tests also exercise malformed receipts, nullable latest responses, complete scoped records and dispatched-write cancellation/uncertainty without automatic retry.

Before the checkpoint browser views were added, the integrated `make check build` passed Go race tests/vet, the then-current nineteen browser tests, TypeScript, documentation checks and reproducible embedded assets. The later twenty-five-test run is recorded in the [checkpoint browser proof](browser-guide.md#checkpoint-browser-proof). Six actual Linux packaged groups then used `fc`, `fcd` and `fcmcp` with two projects and isolated profiles. They verified nineteen discovered tools, CLI-to-MCP exact replay, compact receipts, captured/current revision differences, stable archive paging and scoped cursors, read-only permissions/revocation, unchanged job/history, six checkpoint/event/receipt effects, exact 16 KiB content, operator attribution and restart recovery. Thirty-seven invalid raw MCP requests and twenty-three invalid CLI invocations made zero HTTP requests; valid calls with unavailable credentials also failed before HTTP.

The 71-file Go/module source digest was `8dc0f26d33ff24fc9fb87b124f328a088c8e0a2f532cbe9081ffddf6a4377f92`. Source and all three executable hashes remained unchanged throughout packaged testing, and owned processes/state were cleaned. This is Linux local-stdio proof. It does not establish native platform checkpoint behavior, model-host scheduling/wakeup, fleet throughput, power-loss durability or a complete resume packet. Generic transaction deadline/cancellation tests remain separate; packaged checkpoint testing did not newly prove those internals.
