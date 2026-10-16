# Local Coordination API

## Implemented boundary

`fcd` serves scoped projects, workers, access grants, basic jobs and durable messages. The CLI implements local `version`/`context` and the profile-bound [read commands](development.md#read-only-agent-commands). The [explicit-key CLI mutations](development.md#explicit-agent-mutations) create/edit jobs and send/seen/ack messages. The [stdio MCP adapter](mcp.md) exposes read and explicit-key mutation tools against these same endpoints. The [incoming-message event API](message-events.md) supplies scoped snapshot/replay, bounded waits and a persistent SSE transport. The [embedded read-only browser](browser-guide.md) displays projects, jobs/checkpoint details, workers, live incoming conversations and stored review queue, PR and run views. [Job checkpoints](job-checkpoints.md) provide immutable progress records through the same API and agent clients. The [review workspace](review-workspace.md) implements administrator observation and scoped read routes. Optional [native review execution](review-execution.md) and explicit [source refresh and observed policy](review-source.md) use separate project-scoped routes and owned background workers. Browser mutations, a project/job event feed, backup API, primary-job selection, checklist and deferred-note API remain unavailable in this slice.

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

### Browser sessions

The embedded browser derives a read-only session after its bearer-authenticated instance/project handshake. CLI and MCP clients retain their existing bearer transport. `POST /api/v1/projects/{project_id}/browser-session` requires one bearer credential, the matching `X-FC-Instance` header, no browser-session selector and exactly `{"session_id":"<32 lowercase hexadecimal characters>"}`. It returns `session_id`, `instance_id`, `project_id` and the absolute RFC3339 UTC `expires_at`, plus a 24-hour encrypted and authenticated HttpOnly SameSite=Strict cookie. The cookie contains the originating credential hash rather than the raw credential. Session creation creates no domain receipt or event and calls no provider.

HTTPS deployments use a Secure, host-only `__Host-fc-browser-{session_id}` cookie at `/`. Only literal localhost or loopback HTTP origins use `fc-local-browser-{session_id}` without Secure. Forwarding headers never weaken cookie security. The deployment must preserve the public Host header; HTTPS termination belongs to [hosting infrastructure](hosting.md). Successful connection retains at most eight valid session cookies issued by this instance, within a 6 KiB cookie-header budget. The new connection is always kept; older retained sessions are expired as needed. Closing a tab without Disconnect therefore cannot grow this instance’s cookie header indefinitely. Other instances’ undecodable cookies and unrelated applications’ cookies are left alone. An evicted tab must connect again; ordinary reload preserves its retained session.

Session reads send exactly one `X-FC-Browser-Session` selector and its matching cookie, with the expected instance header and optional worker selector. Authorization plus a session selector is rejected; cookies without the selector do not authenticate requests. A session permits only GET/HEAD reads of its selected project and its verified instance handshake. It never permits administrative listing or writes, regardless of the originating credential's permissions. Current credential revocation, grants, project binding and absolute expiry are enforced again during authorization, including active message streams. Durable instance signing material permits sessions to survive service restart; replacing the instance does not silently rebind them.

`DELETE` on the same endpoint takes the selector header and exactly `{}`, with no bearer header, and expires only that selector's cookie. The response is `{"session_id":"<selected session>","logged_out":true}`. Clearing works for an expired or missing cookie and never revokes the original credential. POST and DELETE reject a supplied mismatched Origin or cross-site fetch metadata and do not enable cross-origin access. See [browser connection behavior](browser-guide.md#connect-and-navigate) for per-tab storage, reload and disconnect semantics.

Every mutation requires one `Idempotency-Key` of at most 128 bytes. Retain the same key, normalized request and worker attribution when retrying an uncertain outcome. Exact replay returns the original receipt after checking current access; it does not report current resource state. Changed request or worker under that key conflicts. A new key creates a new mutation and enforces the current revision where required. Revoked credentials cannot replay old receipts.

## Project and worker setup

| Endpoint | Request or result |
| --- | --- |
| `POST /api/v1/projects` | Administrator sends `{name, repository}` using `owner/repository`; returns compact project receipt. |
| `GET /api/v1/projects` | Administrator reads a bounded project page. |
| `GET /api/v1/projects/{project_id}` | Scoped project detail. |
| `POST /api/v1/projects/{project_id}/workers` | Scoped write sends `{name}`; returns compact worker receipt. |
| `GET /api/v1/projects/{project_id}/workers` | Scoped bounded worker page. |
| `GET /api/v1/projects/{project_id}/workers/{worker_id}` | Scoped worker detail. |
| `POST /api/v1/projects/{project_id}/credentials` | Administrator sends `{token_sha256, permissions}`; returns credential/principal IDs and project ID. |
| `DELETE /api/v1/projects/{project_id}/credentials/{credential_id}` | Administrator revokes access; requires an idempotency key. |

For a project client, generate a high-entropy secret privately and register its lowercase SHA-256 hex digest, with permissions such as `["read", "write"]`. Keep the raw secret in the client's protected credential file. The service stores its hash; API receipts do not return the raw secret. Each registered credential currently establishes a separate principal.

Project creation explicitly verifies GitHub's stable repository ID with a five-second deadline, before opening a write transaction. One repository has one project owner within an instance, including aliases or renames. Verification failure leaves no project. A GitHub 404 returns `repository_not_found` because the repository is absent or inaccessible to the configured credential; transport failures, rate limits and other provider failures return `provider_unavailable`. Optional `--github-token-file PATH` supplies a private operator credential; public repository verification can use unauthenticated GitHub access. Exact retained project-create retries and changed-request conflicts avoid another provider request. Ordinary job/message operations never call GitHub.

## Basic jobs

Routes below are relative to `/api/v1/projects/{project_id}`.

| Endpoint | Request or result |
| --- | --- |
| `POST /jobs` | `{title, brief, worker_id?, status?}`; returns `{id, revision, outcome}`. |
| `GET /jobs` | Compact page; optional `worker_id` and `status` filters. |
| `GET /jobs/{job_id}` | Explicit full detail, including brief. |
| `PATCH /jobs/{job_id}` | Required `expected_revision` plus selected `title`, `brief`, `worker_id` or `status`; compact receipt. |
| `GET /jobs/{job_id}/history` | Explicit bounded revision history with retained full job state. |

Statuses are `active`, `parked`, `blocked` and `completed`; creation defaults to `active`. Title is bounded to 240 bytes and brief to 32 KiB. Assignments reference workers in the selected project. Creation starts revision 1. A guarded edit atomically changes the job, appends history/event metadata and stores its receipt; a losing revision race writes none of them. An empty worker ID clears an assignment on edit.

## Job checkpoints

[Job checkpoints](job-checkpoints.md) add guarded progress records independently of job state. That page owns append/latest/archive endpoints, content limits, current integration status and client commands.

## Durable messages

| Endpoint | Request or result |
| --- | --- |
| `POST /messages` | `{recipient_worker_id, body, reply_to_message_id?}`; compact `{id, conversation_id, outcome}` receipt. |
| `GET /inbox/{worker_id}` | Compact previews and seen/ack state; optional `unread=true` and `unacknowledged=true` filters. |
| `GET /conversations/{conversation_id}` | Explicit chronological full-body page with root/reply identities. |
| `POST /messages/{message_id}/seen` | Strict `{}` body; only the selected actual recipient can mark seen. |
| `POST /messages/{message_id}/ack` | Strict `{}` body; only the selected actual recipient can acknowledge, atomically establishing seen. |

Sender, recipient and reply references must belong to the selected project. Body is nonblank UTF-8, bounded to 16 KiB. Inbox previews contain at most 160 Unicode runes with explicit `preview_truncated`. Replies preserve conversation identity, and pages use a durable insertion sequence rather than timestamp ordering alone.

Reads leave seen/ack state unchanged. New-key transitions that are already satisfied return `unchanged`; exact-key replays return their original outcome. Message text, recipient state, compact receipts and event metadata commit atomically. Event payloads do not include message bodies. Project read permission currently allows reading that project's inboxes/conversations; worker selection restricts attribution and recipient transitions, not visibility within an authorized project.

## Output, limits and errors

Lists return `{items, cursor?, has_more}` with default 20 and maximum 100 items. The cursor is omitted on the final page. Cursors are signed and bound to the instance, principal, project, route/filter and order. Job history and conversations require explicit reads; no mutation echoes the submitted brief or message body. Requests use strict JSON objects with declared, exactly cased fields, unique keys and valid Unicode, bounded to 256 KiB.

Errors use compact JSON `{error: {code, message, details?}, request_id}`. Authentication/access failures are `unauthorized` or `forbidden`; malformed JSON, common query/cursor validation and inbox input use `validation_failed`. Jobs also return the specific input codes `invalid_title`, `invalid_brief`, `invalid_status`, `invalid_revision`, `empty_edit`, `invalid_filter`, `invalid_cursor`, `invalid_query` and `idempotency_key_required`. Unavailable resources/capabilities are `not_found`; retry-body and revision conflicts are `idempotency_conflict` and `revision_conflict`. Duplicate setup returns `repository_already_configured` (with `details.project_id` identifying the existing owner), `worker_already_configured` or `credential_already_configured`. SQL details and credentials are excluded. Callers should inspect the code and retain the same key after an uncertain write; do not blindly create a new key.

Writes wait at most two seconds for admission/transaction context. SQLite lock contention returns `overloaded` with no internal retry; retry using the same key after contention clears. Health/readiness are `/healthz` and `/readyz`; readiness depends on local storage rather than provider availability. These limits do not establish production throughput or a deadline for arbitrary filesystem I/O.

See [development proof](development.md) for executed checks and unproven boundaries.

## Domain schema startup

Jobs and inbox schemas have separate ordered migration versions stored in instance metadata. Startup applies only missing migrations under the same exclusive transaction as their version records and refuses a newer domain version before running its DDL. Failed migrations roll back their schema changes and version updates together. The original unversioned domain tables enter this sequence at version zero using their idempotent initial DDL; this does not validate an arbitrarily modified database. Use a compatible binary for a newer schema rather than deleting version records. Migration checks run before serving requests, never during ordinary reads or writes.

The [native review workspace](review-workspace.md) owns its administrator observation and project-scoped queue/PR/run read routes. Those handlers read indexed stored evidence without provider refresh or job/message effects.

The [source refresh contract](review-source.md) owns its explicit POST, operation read and observed-policy read. Its short default wait returns a completed result directly where possible, with a pending receipt only when the work remains active; ordinary review reads never trigger that refresh.
