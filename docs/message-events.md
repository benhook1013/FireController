# Incoming Message Events

## Implemented boundary

The loopback API provides a selected worker's compact inbox snapshot and durable incoming-message event replay, with a bounded wait or persistent SSE stream. The CLI exposes finite snapshot/replay and bounded wait calls. MCP subscriptions, idle-chat wakeup and job/project event feeds remain unimplemented. The [architecture](architecture.md#events-and-subscriptions) owns the broader delivery contract.

## Snapshot and replay

Call `GET /api/v1/projects/{project_id}/inbox/{worker_id}/snapshot` with the authenticated binding and matching `X-FC-Worker`. It returns `{inbox, event_cursor}`. The inbox is the existing compact page, with explicit `has_more` and pagination cursor, accepting the same unread/unacknowledged/limit/cursor filters. It is not a complete resume packet. Read older or additional pages explicitly.

Authorization, inbox projection and event high-water mark share one read transaction. A concurrent message committed after that snapshot remains reachable through replay. Inbox pagination and event cursors serve different purposes and cannot substitute for one another.

Call `GET /api/v1/projects/{project_id}/message-events?cursor=CURSOR` with the same binding. Optional `limit` is 1–100 (default 20); optional `wait_ms` is 0–4000 (default 0). Query fields occur once each. The response is `{items, cursor, has_more, timed_out}`. Retain the last processed cursor and deduplicate by instance plus event sequence. Repeating a cursor can repeat delivery; it never marks a message seen or acknowledged.

Items contain sequence, project ID, type, resource ID, revision, principal actor, compact payload and timestamp. The feed includes `message.sent`, `message.seen` and `message.acknowledged` for the selected recipient. It omits message bodies and other workers' events. Full bodies require an explicit conversation read. Project sequence gaps caused by filtering are valid. A completed page can advance its cursor across those gaps.

Cursors are signed against instance, principal, project and selected-worker feed. Other bindings or malformed/ahead positions fail. A cursor older than the seven-day replay floor returns `resync_required`; obtain a fresh scoped inbox snapshot and explicitly retrieve remaining pages. Durable messages and receipt state remain available. This slice enforces replay eligibility without physically pruning old event rows.

## Agent CLI

The private profile fixes instance, project and recipient worker for these calls:

```sh
fc inbox snapshot --unread
fc inbox events --cursor CURSOR --wait-ms 4000
```

Snapshot accepts the existing inbox filters and page options. Events requires an explicit snapshot/replay cursor and accepts limit plus wait-ms. One call returns one bounded page; no cursor is saved and no automatic polling, seen or acknowledgment occurs. A five-second total budget covers identity verification and the request. The typed client rejects incomplete or mis-scoped event pages, unexpected payload fields, invalid ordering and inconsistent timeout/page state.

## Waiting and bounds

Waiting subscribes before the initial durable read, avoiding a lost notification between read and registration. Writes publish coalesced, worker-scoped hints only after commit. Hints trigger another authorized durable read; they do not own delivery state. Exact mutation replay and failed transactions publish no new hint or event. There is no repeated idle SQL or provider polling.

No database transaction remains open while waiting. A hint buffer contains one coalesced signal; slow callers do not block writes or accumulate an event queue. Capacity is shared by waits and streams: 1024 concurrent subscriptions per process and 16 per principal; exhaustion returns `overloaded`. Cancellation releases the wait without changing message state. Each database read has a two-second budget, and the endpoint has a five-second total budget. An expired wait returns a compact empty page with `timed_out: true`, rather than a transport heartbeat.

Credential revocation wakes current waits and causes authorization to be checked again before returning data. Authorization is also rechecked in every replay transaction. One active service process is supported; these in-memory notifications do not establish multi-replica delivery.

## Persistent transport

`GET /api/v1/projects/{project_id}/message-events/stream?cursor=CURSOR` accepts an optional limit and the same authenticated worker binding. `HEAD` is rejected promptly. Authentication and validation failures return ordinary JSON before streaming begins. The initial `message-events` SSE frame confirms readiness, including a possibly empty page; later frames carry bounded pages. Each frame's `id` is its replay cursor. Resume with the last processed cursor, rather than treating successful socket writing as application acknowledgment.

The connection registers once and reads durable changes only on relevant commit notifications. Fifteen-second keepalive comments recheck authorization without scanning message/event history. They stay in the transport layer. A permission, retention or storage failure sends one safe `error` event and closes; cancellation closes quietly. The service shutdown signal closes persistent streams promptly without changing message state, while finite requests retain their bounded graceful drain. Clients reconnect using their last processed cursor after restart. Frames are bounded to 1 MiB, with a two-second deadline covering each write/flush. Idle connections override the normal whole-response write timeout without leaving writes unbounded. A slow consumer cannot block storage transactions or build an unbounded queue.

This endpoint is available to HTTP clients; persistent typed-client, CLI and MCP subscription adapters remain later work. The API does not assert that an installed host can wake an idle agent.

## Proof and limits

Focused race tests cover scoped cursors, filtered gaps, pagination, expiry, commit/wait registration order, failed writes, exact replay, cancellation, capacity release and revocation. A deterministic interleaving commits a public API message while a read snapshot remains open, verifies inbox and watermark exclude that commit, then retrieves it through replay. Actual Linux `fcd` HTTP checks cover project/worker/grant bootstrap, snapshot, send/replay and wait delivery. Provider bootstrap uses a read-only GitHub repository lookup; message events perform no provider work.

An isolated SQLite probe with one million synthetic project events measured a current-cursor store replay median of 0.092 ms after replacing the full-history retention aggregate (370.838 ms before). Replaying a large recent backlog still cost 161.588 ms in that probe. These are store-call measurements on one machine, excluding HTTP/client costs; they do not establish fleet throughput.

Six packaged CLI groups also passed, including twelve invalid-flag cases with zero HTTP, scope/filter/paging/restart replay, cancellation and a measured 5.005-second shared deadline. Four actual `fcd` SSE groups passed for readiness/commit delivery, replay reconnect, revocation/error/EOF and prompt HEAD rejection. Public stream tests additionally cover shutdown cancellation, unchanged recipient state and replay after reattachment, idle write-timeout override, a non-reading native socket, frame bounds and shared capacity.

These checks do not establish fleet throughput, physical event retention, persistent client adapters, network/TLS deployment, browser delivery or native agent-host wakeup. The performance acceptance scenarios remain required before those capabilities are claimed.
