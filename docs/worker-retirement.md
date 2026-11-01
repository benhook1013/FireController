# Worker retirement

Retirement removes a worker from ordinary active lists, assignment choices and Delivery lanes while preserving its identity and history. Direct worker, completed-job, conversation and historical reads remain available. Worker credentials continue to use the existing project credential controls.

## Clear unfinished work first

The retirement preview identifies three kinds of blockers in the selected project:

- Assigned jobs whose status is active, blocked or parked. Reassign them or complete them first.
- Pending notes attached directly to the worker or to jobs still assigned to it. Move those notes, consume them or dismiss them with the normal note controls.
- Received messages that remain unacknowledged. Inspect and explicitly acknowledge them first.

Retirement never clears assignments, resolves notes or acknowledges messages automatically. Completed jobs, resolved notes, acknowledged conversations, focus and pause history remain retained. Review-operation worker IDs record attribution and do not add another retirement prerequisite.

The preview returns exact counts and up to twenty IDs per blocker kind. The mutation checks the blockers again in its write transaction, so a concurrent assignment or delivery either prevents retirement or is refused after retirement. New assignments, reopened work, pending-note targets and message delivery cannot target a retired worker. Historical references remain valid.

## Agent and browser operations

`workers retirement WORKER_ID` reads the preview; `workers retire WORKER_ID --key KEY` performs the guarded operation with normal project-write permission. MCP exposes `worker_retirement_read` and `worker_retire`. Ordinary worker lists select active workers; `workers list --include-retired` and the browser archive view explicitly include retired records. Keep the original key after an unknown mutation outcome.

The API uses `GET /api/v1/projects/{project_id}/workers/{worker_id}/retirement` for preview and `POST` to the sibling `/retire` route with an empty JSON object and explicit idempotency key. A successful retirement returns the retained worker ID, revision two and `retired`; another deliberate request for the same retired identity returns `unchanged`. A blocked response uses `worker_retirement_blocked` and includes the current blocker handles. Retired target admission returns `worker_retired`.

A project manager can inspect an inactive worker's inbox and acknowledge selected messages on its behalf using `inbox ack MESSAGE_ID --worker WORKER_ID --key KEY` or MCP `inbox_ack` with `worker_id`. The existing message-acknowledgement API accepts the optional explicit recipient in its JSON body. Selecting another recipient requires project-management authority checked again inside the mutation. The event retains the authenticated principal, actual acting worker and selected recipient; no identity switch is needed. Ordinary recipient acknowledgements retain their existing request shape and permission.

The Workers page shows blockers and links to the relevant jobs, notes and inbox. Acknowledging a selected message and retiring the worker are separate deliberate actions. Cookie mutations keep the existing same-origin requirements.

## Persistence and proof

The separately versioned `worker_retirements` domain retains the original worker table and foreign keys. Backups, inspection and inactive restore preserve retirement facts and reject malformed schema or retirement records. Existing bundles without that optional domain retain their original contents. Installed backup helpers must support the selected daemon's domain versions before it begins producing new bundles; the [offsite procedure](offsite-backups.md) owns that deployment requirement.

Focused proof exercises all blocker kinds, cleanup followed by retirement, assignment/delivery races, exact-key replay, retained history, cross-project authorization, owner acknowledgements and browser controls. Installed qualification is reported with the release rather than inferred from source tests.
