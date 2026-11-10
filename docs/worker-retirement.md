# Worker retirement

Retirement removes a worker from ordinary active lists and new coordination targets while preserving its identity and history. Completed jobs, conversations, focus and pause history remain readable. Fresh focus and pause mutations are rejected after retirement; exact earlier receipts remain replayable without another effect. Credentials remain separate project access controls; retirement never revokes them or cancels review operations.

## Clear unfinished work first

Preview reports exact totals and up to twenty IDs for unfinished assigned jobs, pending notes attached directly to the worker or to its assigned jobs, and unacknowledged incoming messages. Reassign or complete jobs, move/consume/dismiss notes and explicitly acknowledge messages first. Retirement performs none of that cleanup automatically.

The mutation rechecks blockers in the same transaction as retirement. Concurrent job assignment, reopened work, pending-note targeting and message delivery either prevent retirement or are rejected after it. New message sends also require an active sender. Historical references and exact prior receipt replay remain valid.

## Native operations

GET `/api/v1/projects/{project_id}/workers/{worker_id}/retirement` reads preview. POST to the sibling `/retire` route takes `{}` and an explicit key with ordinary project-write permission. A successful receipt contains the worker ID, revision two and `retired`; another deliberate retirement returns `unchanged`. `worker_retirement_blocked` includes current blocker handles; active-target admission returns `worker_retired`. Keep the exact key after an unknown outcome.

CLI exposes `workers retirement WORKER_ID`, `workers retire WORKER_ID --key KEY`, and `workers list --include-retired`. MCP exposes `worker_retirement_read`, `worker_retire` and the explicit `workers_list.include_retired` option. Cursors are bound to active versus archive views. Direct worker reads retain historical identities.

A manager may inspect another worker's inbox and explicitly acknowledge a selected message using `inbox ack MESSAGE_ID --worker WORKER_ID --key KEY` or MCP `inbox_ack` with `worker_id`. The selected recipient is in the request body; the authenticated principal and actual acting worker are unchanged. Current management is checked before receipt replay and inside the mutation. Ordinary empty-body acknowledgement keeps its original wire and receipt identity. Added durable acting-worker audit detail is not exposed through the legacy incoming-message feed.

## Persistence and scope

The additive `worker_retirements:1` domain retains worker-table identities and foreign keys. Backups and inactive restore preserve retirement facts and reject malformed physical schemas or records. Existing bundles need no inspection-time migration. Native tests cover blockers, races, historical reads, peer acknowledgement, exact-key recovery and backup compatibility. Browser retirement controls, Delivery lanes and automatic scheduling remain later capabilities rather than implied by this native prerequisite.
