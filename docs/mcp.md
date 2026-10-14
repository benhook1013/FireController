# Local MCP Tools

## Implemented boundary

`fcmcp` exposes nine read tools and five explicit mutations over local stdio using the official [Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk), pinned to v1.8.0. Tools call the shared typed HTTP client. The service owns authentication, project scope, permission checks, cursors and storage; the adapter never shells out to `fc` or performs provider work. Both CLI and MCP job/message mutations use the same typed client and API. Subscriptions, remote MCP and automatic chat wakeups remain unimplemented.

## Configure one agent binding

Build with `make build`. Configure the host to launch this command with this agent's private profile:

```sh
/path/to/bin/fcmcp --profiles-file /private/profiles.json --profile project-builder
```

A repeatable process `FC_PROFILE` can replace the profile argument. The default profile file and credential-reference rules match the [CLI](development.md#private-profile-file). This connection keeps its startup instance/project/worker binding. There is no per-tool profile selector or workspace-wide current worker. Different grants or agent identities use separately bound processes/connections. Restart explicitly to select another binding.

Startup validates configuration without opening the credential or contacting the service. Initialization, discovery and local `context` also need no credential or network. Service calls load the selected protected credential and share a five-second budget across authenticated instance verification and the scoped operation. Argument validation precedes credential and network access.

## Tools and output

| Tool | Input and result |
| --- | --- |
| `context` | No fields; configured local identity, without service verification. |
| `doctor` | No fields; verified instance/project/worker identity. |
| `workers_list` | Optional limit/cursor; one compact worker page. |
| `jobs_list` | Optional worker_id/status/limit/cursor; one compact job page. |
| `jobs_assigned` | Optional status/limit/cursor; one compact page assigned to the bound worker. |
| `job_read` | Required job_id; explicit full job detail and brief. |
| `job_history` | Required job_id and optional limit/cursor; explicit full-state history page. |
| `inbox_list` | Optional unread/unacknowledged/limit/cursor; bound worker's compact preview page. |
| `inbox_thread` | Required conversation_id and optional limit/cursor; explicit full-body conversation page. |
| `jobs_create` | Required idempotency_key/title; optional brief/worker_id/status; compact job receipt. |
| `job_edit` | Required idempotency_key/job_id/expected_revision plus selected title/brief/worker_id/status fields; compact receipt. |
| `inbox_send` | Required idempotency_key/recipient_worker_id/body; optional reply_to_message_id; compact message receipt. |
| `inbox_seen` | Required idempotency_key/message_id; explicit recipient seen transition. |
| `inbox_ack` | Required idempotency_key/message_id; explicit recipient acknowledgment, also establishing seen. |

Limits use the API default when omitted and accept explicit values from 1 to 100. Reads never advance seen/ack state, change a job or write a checkpoint. Assigned jobs do not claim to contain primary-job or full resume context. Invalid, unknown, duplicate, exactly mis-cased, null or oversized tool fields fail before service work. Profile selectors cannot be supplied as tool arguments.

Mutations require the caller to retain an explicit key and request; the adapter never generates a replacement key or automatically retries. Job edits require the current expected revision; empty brief or worker clears that field. A dispatched write with a lost, canceled or invalid response returns `outcome_unknown` when a tool result can be delivered. Host cancellation may suppress that result, so absence of a reply is never proof of rollback. Recover using the original request, key and binding. The [MCP annotations](https://modelcontextprotocol.io/specification/2025-11-25/schema#toolannotations) distinguish reads from writes. Repeating identical arguments retains the same explicit key and has no additional effect; job edits can replace current fields, while creation/send/receipt transitions add records or handling state. These tools operate within the configured service and perform no external provider work. The [CLI recovery rules](development.md#explicit-agent-mutations) also apply.

Results contain structured data with a compatibility text representation. Tool failures use `isError` and safe code/message/action plus bounded request ID/details where available. Standard output contains only MCP protocol frames; startup/connection diagnostics use standard error. The adapter emits no progress stream, provider scans or heartbeat content into tool results. Explicit archives use the shared client's larger bounded response budget; routine lists stay compact.

## Proof and limitations

SDK transport tests cover four legacy initialize versions (2024-11-05, 2025-03-26, 2025-06-18 and 2025-11-25) and the 2026-07-28 discovery/metadata path. An actual Linux stdio subprocess passed 11 packaged check groups, including all nine read tools under 2025-11-25, strict arguments, cancellation, the total deadline and reconnect after service restart. The official SDK command transport verified newest-protocol discovery, list and local context; the newest version does not use the legacy initialize sequence. Tests against actual public service handlers cover scoped reads, strict arguments, complete pages, privacy errors, cancellation and the total deadline, with unchanged domain/events/receipts and provider counts. Linux build, race tests, vet and Windows amd64/macOS arm64 cross-builds passed for the adapter slice.

Six additional packaged mutation groups passed through actual Linux stdio binaries, covering all five write tools, sixteen invalid-input cases with zero HTTP, scope/revision/replay/recipient transitions and revocation. An owned proxy committed writes before dropping, holding or canceling the response; each original attempt dispatched once, and explicit same-key recovery retained one effect. The deadline case completed in 5.003 seconds. Mutation protocol proof uses 2025-11-25.

These checks establish the SDK/protocol and service boundary, not compatibility with every installed agent host. Whether a pending tool permits other model work, delivers completion across turns, or wakes an idle chat remains separate host proof. Native Windows initialization/list/local-context and safe missing-credential behavior passed. Native Windows service-backed MCP read and job-create/replay checks also passed against actual `fcd`; all five mutation tools have Linux packaged proof. Native macOS execution and live subscriptions remain unproven.
