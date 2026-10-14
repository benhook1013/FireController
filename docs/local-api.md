# Local Coordination API

## Implemented boundary

`fcd` serves scoped projects, workers, access grants, basic jobs and durable messages. The CLI implements local `version`/`context` and the profile-bound [read commands](development.md#read-only-agent-commands). The [explicit-key CLI mutations](development.md#explicit-agent-mutations) create/edit jobs and send/seen/ack messages. The [stdio MCP adapter](mcp.md) exposes read-only tools against these same endpoints. There is no standalone browser, live event stream, review executor, backup API, primary-job selection, checklist, checkpoint or deferred-note API in this slice.

The [architecture](architecture.md) owns target behavior. This page describes the current implementation and its limits.

## Start and connect

Build with `make build`, then run:

```sh
./bin/fcd --data-dir /private/firecontroller --listen 127.0.0.1:8080
```

The service creates an owner-only data directory when absent and stores its database and `bootstrap-admin.token` there. Existing Unix directories and credential files must have owner-only permissions. Startup prints one compact listening record with the address and instance ID; it never prints the credential. Ordinary restart preserves instance identity and retained state. Unknown newer database versions are refused before schema changes.

Only loopback listeners are supported in this slice. Network/TLS deployment needs later proof. SQLite must use local storage, with one active service process; network-mounted databases and multiple active replicas are unsupported.

Clients send `Authorization: Bearer <credential>` on API requests. Administrative project creation, project listing, and access management require the bootstrap administrator. Project clients receive explicit `read` and/or `write` grants. `X-FC-Worker` selects an existing worker in the request's project for attribution; it never grants permission. Sending messages and changing seen/ack state require that worker selection.

The authenticated `GET /api/v1/instance` returns the durable instance ID. Bound clients supply `X-FC-Instance` on every request; a mismatch returns `instance_mismatch` before domain reads, writes or provider work, including replacement after a client handshake. Missing headers remain supported for explicit administrative bootstrap. The local `fc context` command only reports configuration.

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

Project creation explicitly verifies GitHub's stable repository ID with a five-second deadline, before opening a write transaction. One repository has one project owner within an instance, including aliases or renames. Verification failure leaves no project. Optional `--github-token-file PATH` supplies a private operator credential; public repository verification can use unauthenticated GitHub access. Exact retained project-create retries and changed-request conflicts avoid another provider request. Ordinary job/message operations never call GitHub.

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

Lists return `{items, cursor, has_more}` with default 20 and maximum 100 items. Cursors are signed and bound to the instance, principal, project, route/filter and order. Job history and conversations require explicit reads; no mutation echoes the submitted brief or message body. Requests use strict JSON objects with declared, exactly cased fields, unique keys and valid Unicode, bounded to 256 KiB.

Errors use compact JSON `{error: {code, message, details?}, request_id}`. Authentication/access failures are `unauthorized` or `forbidden`; malformed input is `validation_failed`; unavailable resources/capabilities are `not_found`; retry-body and revision conflicts are `idempotency_conflict` and `revision_conflict`. SQL details and credentials are excluded. Callers should inspect the code and retain the same key after an uncertain write; do not blindly create a new key.

Writes wait at most two seconds for admission/transaction context. SQLite lock contention returns `overloaded` with no internal retry; retry using the same key after contention clears. Health/readiness are `/healthz` and `/readyz`; readiness depends on local storage rather than provider availability. These limits do not establish production throughput or a deadline for arbitrary filesystem I/O.

See [development proof](development.md) for executed checks and unproven boundaries.
