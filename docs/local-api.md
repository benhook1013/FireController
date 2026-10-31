# Local Coordination API

## Implemented boundary

`fcd` serves scoped projects, workers, access grants, basic jobs and durable messages. The CLI implements local `version`/`context` and the profile-bound [read commands](development.md#read-only-agent-commands). The [explicit-key CLI mutations](development.md#explicit-agent-mutations) cover jobs and their current progress, checklists, focus, pause, notes and messages. The [stdio MCP adapter](mcp.md) exposes read and explicit-key mutation tools against these same endpoints. The [incoming-message event API](message-events.md) supplies scoped snapshot/replay, bounded waits and a persistent SSE transport. The [embedded browser](browser-guide.md) displays projects, jobs/checkpoint details, read-only checklists and narrative updates, workers, live incoming conversations and stored review queue, PR and run views. [Job checkpoints](job-checkpoints.md) provide immutable progress records through the same API and agent clients. The [review workspace](review-workspace.md) implements administrator observation and scoped read routes. Optional [native review execution](review-execution.md) and explicit [source refresh and observed policy](review-source.md) use separate project-scoped routes and owned background workers. [Deferred notes](notes.md) add explicit scoped reminder/source reads and guarded mutations. The scoped project feed updates current stored views; browser messaging and administrator appearance controls use credential-derived attenuated session authority; a backup API remains a separate boundary. The [public project website](public-site.md) uses administrator publication routes and independent anonymous reads/events.

The [architecture](architecture.md) owns target behavior. This page describes the current implementation and its limits.

## Start and connect

Build with `make build`, then run:

```sh
./bin/fcd --data-dir /private/firecontroller --listen 127.0.0.1:8080
```

The service creates an owner-only data directory when absent and stores its database and `bootstrap-admin.token` there. Existing Unix directories and credential files must have owner-only permissions. Startup prints one compact listening record with the address and instance ID; it never prints the credential. Ordinary restart preserves instance identity and retained state. Unknown newer database versions are refused before schema changes.

The daemon supports only loopback listeners. Remote HTTPS access through deployment-owned reverse proxies has [hosted proof](hosting.md#executed-deployment-proof); the daemon does not terminate TLS itself. SQLite must use local storage, with one active service process; network-mounted databases and multiple active replicas are unsupported.

Clients send `Authorization: Bearer <credential>` on API requests. Administrative project creation, project listing, and access management require the bootstrap administrator. Project clients receive explicit `read` and/or `write` grants. `X-FC-Worker` selects an existing worker in the request's project for attribution; it never grants permission. Sending messages and changing seen/ack state require that worker selection.

The authenticated `GET /api/v1/instance` returns the durable instance ID. Bound clients supply `X-FC-Instance` on every request; a mismatch returns `instance_mismatch` before domain reads, writes or provider work, including replacement after a client handshake. Missing headers remain supported for explicit administrative bootstrap. The local `fc context` command only reports configuration.

The global-administrator-only `/api/v1/operator/deployment-drain` supports GET status, POST `{}` to begin a process-local dispatch drain, and DELETE to abandon it. It is an operator deployment control, separate from project pauses and agent workflows; the [release guide](releases.md#bounded-kubernetes-rollout) owns its lifecycle and bounded rollout behavior.

### Browser sessions

The embedded browser derives an explicitly attenuated session after its bearer-authenticated instance/project handshake; browser actions derive automatically from the credential and current grants. CLI and MCP clients retain their existing bearer transport. `POST /api/v1/projects/{project_id}/browser-session` requires one bearer credential, the matching `X-FC-Instance` header, no browser-session selector and a strict body containing only `session_id` (32 lowercase hexadecimal characters). It returns `session_id`, `instance_id`, `project_id`, absolute RFC3339 UTC `expires_at`, `allow_writes` and `can_edit_presentation`, plus a renewable 24-hour encrypted and authenticated HttpOnly SameSite=Strict cookie. The cookie contains the originating credential hash rather than the raw credential. Session creation creates no domain receipt or event and calls no provider.

HTTPS deployments use a Secure, host-only `__Host-fc-browser-{session_id}` cookie at `/`. Only literal localhost or loopback HTTP origins use `fc-local-browser-{session_id}` without Secure. Forwarding headers never weaken cookie security. The deployment must preserve the public Host header; HTTPS termination belongs to [hosting infrastructure](hosting.md). Successful connection retains at most eight valid session cookies issued by this instance, within a 6 KiB cookie-header budget. The new connection is always kept; older retained sessions are expired as needed. Closing a tab without Disconnect therefore cannot grow this instance’s cookie header indefinitely. Other instances’ undecodable cookies and unrelated applications’ cookies are left alone. An evicted tab must connect again; ordinary reload preserves its retained session.

Session reads send exactly one `X-FC-Browser-Session` selector and its matching cookie, with the expected instance header and optional worker selector. Authorization plus a session selector is rejected; cookies without the selector do not authenticate requests. GET/HEAD requests retain project-read attenuation, including the verified instance handshake. Effective write access permits only message send/seen/ack and administrator presentation replacement, with a mandatory exact same-origin Origin. It never permits administrative listing, provider/review actions, publication, jobs or access changes. Encrypted initial write/admin allowances intersect current grants; later credential promotion does not broaden an existing session. `GET` on the browser-session endpoint requires the cookie plus selector and returns the same six current identity/capability fields for resume. Current credential revocation, grants, project binding and absolute expiry are enforced again during authorization, including active message streams. Durable instance signing material permits sessions to survive service restart; replacing the instance does not silently rebind them.

`POST /browser-session/renew` under the same project prefix takes exactly `{}` and the existing selector/cookie/instance binding, with mandatory exact same-origin Origin and no bearer credential. It rechecks read access, keeps the selector and original encrypted permission ceiling, replaces only its cookie and returns the same six identity/capability fields. It writes no domain state or receipt. The [renewal contract](browser-session-renewal.md) owns activity admission, informational expiry metadata and recovery.

`DELETE` on the same endpoint takes the selector header and exactly `{}`, with no bearer header, and expires only that selector's cookie. The response is `{"session_id":"<selected session>","logged_out":true}`. Clearing works for an expired or missing cookie and never revokes the original credential. POST and DELETE reject a supplied mismatched Origin or cross-site fetch metadata and do not enable cross-origin access. See [browser connection behavior](browser-guide.md#connect-and-navigate) for per-tab storage, reload and disconnect semantics.

Every mutation requires one `Idempotency-Key` of at most 128 bytes. Retain the same key, normalized request and worker attribution when retrying an uncertain outcome. Exact replay returns the original receipt after checking current access; it does not report current resource state. Changed request or worker under that key conflicts. A new key creates a new mutation and enforces the current revision where required. Revoked credentials cannot replay old receipts.

Receipt keys include their originating instance. Startup upgrades base schema version one to version two in one exclusive transaction, preserving existing receipt fingerprints and response bytes; retained restore metadata identifies the predecessor for supported inactive copies. Reads and writes use the current instance's receipt namespace. This does not activate an inactive restore or permit an adapter to relabel an old retry after rebind; [restore](backups.md#inactive-restore) owns that boundary.

## Project and worker setup

| Endpoint | Request or result |
| --- | --- |
| `POST /api/v1/projects` | Administrator sends `{name, repository}` using `owner/repository`; returns compact project receipt. |
| `GET /api/v1/projects` | Administrator reads a bounded project page. |
| `GET /api/v1/projects/{project_id}` | Scoped project detail. |
| `POST /api/v1/projects/{project_id}/workers` | Scoped write sends `{name}`; returns compact worker receipt. |
| `GET /api/v1/projects/{project_id}/workers` | Scoped bounded worker page, configured pins first then alphabetical name/ID order; see [worker ordering](project-presentation.md#worker-ordering). |
| `GET /api/v1/projects/{project_id}/workers/{worker_id}` | Scoped worker detail. |
| `POST /api/v1/projects/{project_id}/credentials` | Project manager or instance administrator sends `{token_sha256, permissions}`; returns credential/principal IDs and project ID. |
| `PUT /api/v1/projects/{project_id}/credentials/{credential_id}` | Project manager or instance administrator changes an existing active project grant with `{expected_permissions, permissions}` and an explicit idempotency key, preserving its credential and principal. |
| `DELETE /api/v1/projects/{project_id}/credentials/{credential_id}` | Project manager or instance administrator revokes only this project's access; requires an idempotency key. A credential with another active project grant remains usable there. |

For a project client, generate a high-entropy secret privately and register its lowercase SHA-256 hex digest, with permissions such as `["read", "write"]`. Keep the raw secret in the client's protected credential file. The service stores its hash; API receipts do not return the raw secret. Each registered credential currently establishes a separate principal.

Permission sets contain unique `read`, `write` and `manage` values. `manage` implies `read` and `write`; normalized sets use that order. PUT compares the complete normalized `expected_permissions` against the existing active non-administrator grant. A mismatch returns `credential_permissions_changed` with the current permission set only to an authorized manager of that project; a foreign, revoked or administrator credential is unavailable. An accepted change records the old and new permissions and returns `{id, project_id, outcome, permissions}`; exact-key replay retains the original receipt and still requires current management authority. No grant or change can make a project client an instance administrator.

Project creation explicitly verifies GitHub's stable repository ID with a five-second deadline, before opening a write transaction. One repository has one project owner within an instance, including aliases or renames. Verification failure leaves no project. A GitHub 404 returns `repository_not_found` because the repository is absent or inaccessible to the configured credential; transport failures, rate limits and other provider failures return `provider_unavailable`. Optional `--github-token-file PATH` supplies a private operator credential; public repository verification can use unauthenticated GitHub access. Exact retained project-create retries and changed-request conflicts avoid another provider request. Ordinary job/message operations never call GitHub.

## Basic jobs

Routes below are relative to `/api/v1/projects/{project_id}`.

| Endpoint | Request or result |
| --- | --- |
| `POST /jobs` | `{title, brief, worker_id?, status?}`; returns `{id, revision, outcome}`. |
| `GET /jobs` | Compact page; optional `worker_id` and `status` filters. |
| `GET /jobs/{job_id}` | Full current brief/progress, remaining checklist/counts and compact pending exact-job notes. |
| `PATCH /jobs/{job_id}` | Required `expected_revision` plus selected `title`, `brief`, `worker_id`, `status` or partial `progress`; compact receipt. |
| `GET /jobs/{job_id}/history` | Explicit bounded revision history with retained full job state. |
| `GET /jobs/{job_id}/history/{revision}` | One exact retained full job state, including its original brief, or not_found. No current context or history paging. |
| `GET /jobs/{job_id}/progress/history` | Explicit bounded history of progress changes. |

The list-only `status=open` filter selects `active`, `blocked` and `parked` jobs in that priority order, with IDs ordered within each status across pages. It supports the same optional worker filter; cursors remain bound to the selected filters. Omitted or specific-status filters retain ID ordering. `open` is not a stored job status and mutations refuse it. The CLI and `jobs_list`/`jobs_assigned` MCP reads accept the same filter.

Statuses are `active`, `parked`, `blocked` and `completed`; creation defaults to `active`. Title is bounded to 240 bytes and brief to 32 KiB. Assignments reference workers in the selected project. Creation starts revision 1. A guarded edit atomically changes the job, appends history/event metadata and stores its receipt; a losing revision race writes none of them. An empty worker ID clears an assignment on edit.

## Current job progress

[Job state](job-state.md) owns the current `summary`, `next`, `blocker` and `evidence` record, partial-update/clear semantics, limits and existing-data conversion. One guarded PATCH changes selected job fields and progress atomically. Historical checkpoint and narrative-update records remain read-only source archives, rather than alternative normal write surfaces.

## Job checklist

[Job checklists](job-progress.md) owns optional independently versioned checklist replacement/current/history and single-item completion routes. These deliberate changes preserve the brief and current progress, with explicit mutation keys and current revision guards.

## Durable messages

| Endpoint | Request or result |
| --- | --- |
| `POST /messages` | `{recipient_worker_id, body, reply_to_message_id?}`; compact `{id, conversation_id, outcome}` receipt. |
| `GET /inbox/{worker_id}` | Compact previews and seen/ack state; optional `unread=true` and `unacknowledged=true` filters; `newest=true` selects newest-first insertion order. Preserve filters/order with the cursor. |
| `GET /conversations/{conversation_id}` | Explicit chronological full-body page with root/reply identities. |
| `POST /messages/{message_id}/seen` | Strict `{}` body; only the selected actual recipient can mark seen. |
| `GET /messages/{message_id}` | One full immutable message in the bound project; no older conversation bodies or seen/ack changes. |
| `POST /messages/{message_id}/ack` | Strict `{}` body; only the selected actual recipient can acknowledge, atomically establishing seen. |

Sender, recipient and reply references must belong to the selected project. Body is nonblank UTF-8, bounded to 16 KiB. Inbox previews contain at most 160 Unicode runes with explicit `preview_truncated`. Replies preserve conversation identity, and pages use a durable insertion sequence rather than timestamp ordering alone.

Reads leave seen/ack state unchanged. New-key transitions that are already satisfied return `unchanged`; exact-key replays return their original outcome. Message text, recipient state, compact receipts and event metadata commit atomically. Event payloads do not include message bodies. Project read permission currently allows reading that project's inboxes/conversations; worker selection restricts attribution and recipient transitions, not visibility within an authorized project.

## Output, limits and errors

Lists return `{items, cursor?, has_more}` with default 20 and maximum 100 items. Job-state history additionally stops before its 4 MiB encoded response budget; a shorter nonfinal page retains complete entries and a continuation cursor. The cursor is omitted on the final page. Cursors are signed and bound to the instance, principal, project, route/filter and order. Job history and conversations require explicit reads; no mutation echoes the submitted brief or message body. Requests use strict JSON objects with declared, exactly cased fields, unique keys and valid Unicode, bounded to 256 KiB except the combined job PATCH's 384 KiB encoded-body limit.

Errors use compact JSON `{error: {code, message, details?}, request_id}`. Authentication/access failures are `unauthorized` or `forbidden`; malformed JSON, common query/cursor validation and inbox input use `validation_failed`. Jobs also return the specific input codes `invalid_title`, `invalid_brief`, `invalid_status`, `invalid_revision`, `empty_edit`, `invalid_filter`, `invalid_cursor`, `invalid_query` and `idempotency_key_required`. Unavailable resources/capabilities are `not_found`; retry-body and revision conflicts are `idempotency_conflict` and `revision_conflict`. Duplicate setup returns `repository_already_configured` (with `details.project_id` identifying the existing owner), `worker_already_configured` or `credential_already_configured`. SQL details and credentials are excluded. Callers should inspect the code and retain the same key after an uncertain write; do not blindly create a new key.

Writes wait at most two seconds for admission/transaction context. SQLite lock contention returns `overloaded` with no internal retry; retry using the same key after contention clears. Health/readiness are `/healthz` and `/readyz`; readiness depends on local storage rather than provider availability. These limits do not establish production throughput or a deadline for arbitrary filesystem I/O.

See [development proof](development.md) for executed checks and unproven boundaries.

## Domain schema startup

Jobs and inbox schemas have separate ordered migration versions stored in instance metadata. Startup applies only missing migrations under the same exclusive transaction as their version records and refuses a newer domain version before running its DDL. Failed migrations roll back their schema changes and version updates together. The original unversioned domain tables enter this sequence at version zero using their idempotent initial DDL; this does not validate an arbitrarily modified database. Use a compatible binary for a newer schema rather than deleting version records. Migration checks run before serving requests, never during ordinary reads or writes.

The [native review workspace](review-workspace.md) owns its administrator observation and project-scoped queue/PR/run read routes. [Prepared review import](review-import.md) adds an explicit administrator preview/apply flow against that same observation transaction. Queue/PR/run reads and import preview read indexed stored evidence; observation and apply are guarded writes. None of these handlers performs provider refresh or job/message effects.

The [source refresh contract](review-source.md) owns its explicit POST, operation read and observed-policy read. Its short default wait returns a completed result directly where possible, with a pending receipt only when the work remains active; ordinary review reads never trigger that refresh.

The [prepared coordination import](legacy-adoption.md) owns administrator preview/apply for selected jobs/checkpoints and historical messages. [Prepared operational adoption](review-round-controls.md#prepared-operational-adoption) owns the separate round-setting preview/apply routes and source provenance. These deliberate setup operations perform no provider work and do not manufacture review credit or historical delivery events.

## Worker primary job

[Worker primary focus](worker-focus.md) owns the scoped current, guarded replacement and explicit history routes. Focus is optional and independent of job status; reassignment clears the old worker’s selection atomically. These reads do not load the selected brief or a resume packet automatically.

## Worker pause

[Worker pause state](worker-pause.md) owns the scoped current, guarded replacement and explicit history routes. Pause is independent of job status, primary focus and execution. Reads never fetch jobs or history automatically.

## Deferred notes

[Deferred notes](notes.md) own `POST /notes`, `GET /notes`, `GET` and `PATCH /notes/{note_id}`, `GET /notes/{note_id}/history` and `GET /notes/{note_id}/history/{revision}`. Routes use the existing project prefix and authorization. Current/list reads do not load archives; compact history and selected full revisions require explicit reads. Status-only patches do not require resubmitting the body. Notes never consume themselves or change jobs, focus, pause or messages.

## Default job context

[Job state](job-state.md#default-reads) owns the complete working context returned by the ordinary job-detail route. It includes the current job's brief/progress, unfinished checklist items/counts and one compact pending-note page attached to that job. Required current text is never truncated. Further note pages, full note bodies, worker coordination and inbox reads remain deliberate. There is no separate `/jobs/{job_id}/context` route and no provider work during these reads.

## Sent handoff discovery

`GET /api/v1/projects/{project_id}/outbox/{worker_id}` reads one bounded page of that sender’s messages, including recipient identity and current recipient seen/acknowledged state. It shares inbox status/order selectors and preview bounds, but signed pagination cursors are direction-specific. `GET /api/v1/projects/{project_id}/messages/{message_id}/delivery` reads only `id`, `sender_worker_id`, `recipient_worker_id`, `seen` and `acknowledged`. Both require the existing project read authority and perform no writes or provider work; a missing receipt record is unavailable, never invented as unread. Only the recipient’s existing mutations can mark a message seen or acknowledged. Bound CLI/MCP tools select their own sender for discovery and do not require another worker’s connection.
