# Quiet Agent Message Wait

## Implementation status

This slice adds a bounded CLI/MCP wait over the existing persistent incoming-message SSE transport. The typed client and CLI/MCP adapters implement this contract. Focused normal/race typed-client, public-handler, CLI and official-SDK adapter tests pass. Full combined validation and actual packaged proof are pending. The server's existing snapshot, replay, authorization and capacity rules remain owned by [incoming message events](message-events.md).

## Selection and result

`fc inbox wait --cursor CURSOR [--limit N] [--wait-seconds N]` and the read-only MCP tool `wait_for_messages` select the profile-bound worker's incoming feed. Cursor is required. Limit defaults to 20 and accepts 1–100; wait defaults to 25 seconds and accepts 1–60. Explicit zero, null, unknown, duplicate or malformed fields are refused before credentials/network work. The typed client is `WaitForMessages(ctx, cursor, limit, waitSeconds) (MessageEventPage, error)`; typed zero limit/wait selects these defaults.

Relevance means new `message.sent` events after the supplied cursor, including messages another connection may already have seen. It is an event notification, not an unread/unacknowledged inbox query or a complete resume packet. Pending inbox reads and [job context](job-context.md) remain explicit. Results contain bounded event metadata and conversation IDs; full bodies require a selected conversation read.

One call returns the first valid event page containing new incoming messages, filtered to `message.sent`, with its cursor, `has_more` and `timed_out:false`. It does not wait to fill the limit or load additional pages once a relevant page is available. `has_more` conservatively retains the server page's backlog indicator, which may include handling events. Replay with the returned cursor preserves the ordinary at-least-once contract; the caller deduplicates instance/sequence. The initial readiness page can itself contain relevant events.

Empty readiness pages, transport comments and pages containing only seen/acknowledged events remain inside the adapter. Their validated cursors advance only this call's local read position. At the selected wait's expiry, return one compact `{items:[],cursor,has_more:false,timed_out:true}` using the last validated cursor. No heartbeat or intermediate progress becomes a tool result. A timeout result requires successful stream readiness; connection establishment failure or a caller deadline remains an error. Reads never mark messages seen/acknowledged, consume notes, change focus/pause/jobs, dispatch providers or write durable client state.

## Lifetime and bounds

The adapter owns one SSE connection for this call and closes it on result, cancellation, error or expiry. It does not share a global subscription, store a cursor, automatically reconnect, resnapshot or retry. Subsequent waits are explicit calls. An expired cursor returns `resync_required`; the caller obtains a fresh scoped snapshot and retains its pending inbox pages. Lost transport/error results leave the original caller cursor available for safe replay; successful socket writes never acknowledge handling.

The existing identity handshake and normal reads keep their five-second limits. This explicit wait alone may run longer, with an outer total budget of selected wait plus five seconds including adapter identity verification. Stream headers and initial readiness have a five-second establishment bound, further constrained by the outer caller deadline. After readiness, the selected wait timer bounds quiet waiting; an earlier caller cancellation/deadline returns the existing safe read error. A per-call HTTP override must not change another concurrent call's client timeout or transport. Cancellation closes the forwarded stream promptly without acknowledging messages.

Validate content type, initial readiness, SSE frame shape and cursor identity, exact event fields, bound project/recipient, strictly increasing sequences across pages and consistent page state before returning or advancing the local cursor. Each frame is at most 1 MiB and total consumed wire data is at most the existing 4 MiB client response budget, including ignored comments/pages. Oversize, malformed, duplicate/unknown fields, unsupported events or premature EOF fail safely without a partial success. The stream's explicit safe error event retains scoped service error meanings. Do not echo untrusted raw transport or error content. Persistent transport is not permission to keep unbounded buffers or threads.

MCP discovery adds one read tool, bringing the total to 64 tools: 42 reads and 22 mutations. No protocol notifications or task extensions are required. Another tool on the same SDK session must remain usable while the wait is pending. CLI emits one final structured result; stderr contains only safe diagnostics.

## Host capability and proof

A pending call can be placed in the background only when the actual host supports that execution mode. This implementation establishes neither unrelated model sampling during a pending MCP call, result delivery across turns nor idle-chat wakeup. Quiet service/SDK transport is useful independently of those capabilities; explicit inbox reads remain available. The adapter never claims a host capability from protocol support alone.

Focused proof covers initial/backlogged and later delivery, filtered handling-only pages, exact timeout cursor, no intermediate output, cancellation/release, revocation/isolation/instance checks, expiry, malformed/oversize/error/EOF streams, default/explicit bounds and invalid local arguments before credential/network access. Verify same-session concurrent MCP reads, no hidden retries or repeated idle replay/provider work, normal-read deadlines unchanged and zero durable message effects. Actual packaged CLI/service/official-SDK proof is required before provider review, including a wait longer than the former four-second boundary, message delivery and cleanup. Model-host behavior and fleet throughput require separate evidence.
