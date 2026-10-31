# Messages and handoffs

Read for message selection/paging, outgoing delivery state, quiet waits or worker handoff.

Read `inbox list` for pending coordination: it defaults to unacknowledged messages, including seen-but-unhandled ones. `inbox snapshot` uses the same default and also supplies an event cursor. In MCP, `inbox_list` and `inbox_snapshot` default `unacknowledged` to true. Use CLI `--all` or explicit MCP `unacknowledged:false` to inspect handled history; history and pending cursors are distinct. Use `inbox list --newest` or MCP `inbox_list(newest:true)` to find the latest pending requests behind an older backlog, including seen-but-unhandled work. Preserve filters/order on continuation; older unresolved messages stay pending. Reuse an already saved immutable evidence result or completed operation result when it still answers the question; refresh mutable state deliberately. Reads never acknowledge messages. Send with the supplied bound launcher:

```sh
BOUND_LAUNCHER inbox send --to RECIPIENT_WORKER_ID --body 'Checked my job context. Next: ... Issues: ...' --key RETAINED_MESSAGE_KEY
```

The MCP equivalent is `inbox_send`; use its discovered schema for the exact fields. To rediscover a sent handoff after interruption, use `inbox sent` / MCP `outbox_list`; `--all` or `unacknowledged:false` includes handled history. These show the recipient and current seen/acknowledged state. Use `inbox delivery MESSAGE_ID` / `inbox_delivery_read` for one exact receipt without reading a body or changing identity. Sender reads never acknowledge for the recipient. Acknowledge only when handled according to the project's policy. Use one bounded `inbox wait` or `wait_for_messages` when the host supports background calls; it returns incoming messages or job-assignment changes without a separately maintained notification message. Read the indicated job before acting, and preserve human pause/authority. Do not assume automatic idle-chat wakeup or across-turn subscription lifetime.

Bootstrap a new worker with pasteable text containing its bound connection, matching instructions, first reads and reply destination. Once it understands the tool, its actual app reply can prove identity and context checks. An unread app message cannot teach an unonboarded worker how to access its inbox. Report mismatches honestly rather than requiring a positive acceptance reply.
