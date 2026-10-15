# Service Architecture

## Implementation status

This document owns the service contract. The coordination slice implements local SQLite storage, scoped project/worker/access endpoints, basic jobs/history and durable messages with atomic receipts/events. See the [local API](local-api.md) for that boundary. The [incoming-message feed](message-events.md) implements scoped snapshot/replay, bounded waits and browser SSE; the [embedded browser](browser-guide.md) provides read-only jobs, workers and live incoming conversations. [Job checkpoints](job-checkpoints.md) record immutable progress independently of current job state. The [native review workspace](review-workspace.md) adds explicit administrator observations and indexed stored review reads; optional [native review execution](review-execution.md) and [source refresh](review-source.md) supply separately owned provider workers. Explicit [required CI observations](review-ci.md) supply a separate retained assessment. Explicit [observed readiness](review-readiness.md) combines retained provider facts with current local policy without provider work in reads. Explicit [public checkpoint publication](review-publication.md) owns a separate bounded provider worker and retained posting intent. Local operator [backup bundles and inactive restore](backups.md) and managed scheduling/retention are implemented with full checks and packaged proof; activation and remaining advanced job features are unimplemented. Design decisions are recorded in [decisions](decisions.md).

## Boundaries

One Go service hosts the application API, event transport, and browser assets. A Go CLI and packaged MCP adapter call that API; the [client contract](agent-client.md) owns their supported transports and identity binding. Using MCP is optional for installations, while its integration is part of the AI-facing product scope. SQLite is the initial local storage engine. Provider refresh and review execution run as supervised background work with bounded concurrency; no network operation or review subprocess may occupy a database transaction while waiting.

Application domains are projects and access, workers and jobs, messaging, project maps and appearance, observations, and reviews. They share a transaction facility where a real invariant spans domains, but job or message reads do not construct a review controller, deserialize the full review state, or call providers. Provider adapters cannot call back into unrelated domains to build a response.

Server configuration selects listen addresses, private data directory, credentials, and worker limits. Project configuration and editorial content are persisted application records. Clients do not open the server's database directly. Each service instance has a durable instance identifier; profile selection includes that instance as well as the project.

## Projects and actors

Every stored project-owned resource has a project identifier. Names are unique only within their documented project scope. Worker identity is `(project_id, worker_id)`; connection identity is a separate `(instance_id, session_id)` used for delivery and attribution. Concurrent sessions for one worker share its inbox and job assignments, not its transport cursor.

For the initial GitHub integration, one project binds one repository and a repository may belong to only one project within an instance. Creation/binding verifies GitHub's stable repository ID and atomically enforces that uniqueness; a renamed repository or alternate remote URL cannot bypass it. A duplicate returns `repository_already_configured` with the existing project reference when the caller may see it, otherwise an opaque conflict. An interrupted or concurrent binding must not leave two owners. Separate service instances are independent; GitHub Projects boards do not define this application boundary. Broader repository groupings require a separately reviewed design.

Credentials bind an authenticated principal to allowed projects and operations. A supplied worker name is attribution metadata, not permission. Project creation, access management, provider credentials, and project policy changes require administrative permission. The initial access model supports instance administrators and project-scoped read/write clients; additional roles require their own reviewed contract.

Every read, write, pagination cursor, review task, and event subscription validates project scope at the server. Cross-project references are rejected before mutation. The first release does not route messages across projects; Recipient names resolve only within the selected project. A future cross-project route must state both destination project and authorization explicitly.

Local use binds to loopback by default and still uses a credential. Remote use requires scoped authentication and TLS, commonly provided by a reverse proxy. Client profiles contain credential references rather than secrets in command arguments. Shared filesystem access is not a security boundary between agents; independent credentials and server checks establish the application access boundary.

## Transactions and retries

Current-state edits use an expected resource revision. A stale edit returns a conflict containing the current revision and does not overwrite concurrent work. History and events are appended in the same transaction as the state change they describe.

All mutations, including revision-guarded edits, accept an idempotency key scoped to instance, principal, project, operation, and target. Repeating the same key and request returns the original receipt; changing the request under that key returns a conflict. The service stores the normalized request fingerprint, including any expected revision, and receipt. After checking current authorization and scope, an exact stored-key/request replay returns its original receipt before re-evaluating the revision; it does not claim to return current resource state. A new key still enforces the current revision before writing. Within the retained instance history, mutation keys remain until explicit project deletion, so a delayed client retry never silently becomes a new action. A disaster restore has the separate identity and retry boundary below. Provider-action keys and evidence identities are never pruned independently of their reviewed retention policy.

Commit succeeds before a mutation is acknowledged or an event is exposed. A disconnect after commit is recovered by retrying the same key. Revisions, transaction isolation, and uniqueness constraints remain the authority; a process-local mutex is not sufficient proof of correctness.

## Events and subscriptions

Domain changes append durable events in the same transaction. Events have a monotonic server sequence, project scope, event type, resource identity, resource revision, and commit time. The API filters delivery by the authenticated principal, project, and selected worker. A sequence gap caused by filtered events is valid and is not evidence of loss.

Delivery is at least once. Clients deduplicate event IDs and resume from the last processed cursor. A cursor never marks a message seen or handled. Revocation is checked on reconnect and before delivery; an existing connection cannot retain access indefinitely after permission changes.

Keep an initial seven-day event replay window. Events reference durable resources; they are not the only copy of messages or jobs. A cursor outside that window receives an explicit `resync_required` response. The client obtains a scoped snapshot with a cursor taken from the same database read transaction, then subscribes after it. Changes occurring during snapshot retrieval remain reachable through replay.

Subscriber buffers, connection counts, request bodies, and event batches are bounded. A slow subscriber is disconnected with its last confirmed cursor rather than causing unbounded buffering or blocking writes. Transport heartbeats stay below the model-facing tool layer. The API supports a bounded wait for events as well as a browser event stream; the [client contract](agent-client.md) owns model-facing behavior.

## Provider observations and refresh

An observation records provider, repository, object identity, observed head or anchor where relevant, fetched time, completeness, error, and the provider identifiers needed for attribution. A stored observation is a fact with a time, not a promise that the external world has not changed.

Ordinary views return stored observations immediately and show age, missing fields, and last refresh errors. They do not implicitly enqueue provider work. A refresh is explicit or scheduled by the service according to active work. Scheduling backs off when a project is idle or a provider is rate limited.

Coalesce concurrent refreshes only when their project, provider, repository, target, and required fact set agree. Coordinate account-wide quotas across projects using the same provider account without exposing one project's data to another. Provider deadlines and concurrency limits prevent one busy project from starving another.

Shared external-action reservations are instance-wide coordination records, distinct from project-owned attempt payloads. [Review admission](reviews.md) owns their identity and conflict semantics. Project isolation must not allow two projects to duplicate an action on the same external target, nor reveal another project's private records when returning a conflict.

Consequential review operations declare their required fresh facts in [review orchestration](reviews.md). Refresh the selected PR and proven dependencies rather than the entire queue. A changed anchor invalidates only evidence whose meaning depends on that anchor. Durable completed captures are not downloaded again merely to display them.

GitHub pagination must prove completeness for every collection used in an admission decision. Cursor loops, missing pages, partial identity batches, ambiguous provider responses, and incomplete evidence remain explicit errors or holds. A failed refresh preserves the prior observation and does not make it current.

## API shape

Use an HTTP JSON API under `/api/v1`, with project-owned paths under `/api/v1/projects/{project_id}`. Resource IDs are stable opaque identifiers. Display names are selectors resolved within project scope. Responses expose resource revisions, request IDs, and relevant freshness metadata. Errors have a stable code, concise message, request ID, and structured conflict or retry details.

Stable error codes include `unauthorized`, `forbidden`, `not_found`, `revision_conflict`, `idempotency_conflict`, `validation_failed`, `provider_unavailable`, `review_held`, `resync_required`, and `overloaded`. Absence and forbidden access must not disclose another project's resource contents. List responses use bounded cursor pagination; page cursors are bound to project, principal scope, filter, and order.

Request cancellation stops waiting and cancels work that is safe to cancel. It does not roll back an already committed action or imply a provider request was not posted. Variable-duration work establishes a durable operation independently of how its result is collected. Fast completion returns its useful result directly; a short bounded wait falls back to an operation receipt when work remains pending, and callers can select immediate return or a longer explicit wait. Changing a wait preference never creates a different action under the same idempotency key. The adapter may subscribe and deliver a final asynchronous tool result without making the model track that identity or poll. Explicit detach, recovery, and diagnosis expose the receipt. [AI usability](ai-usability.md) owns the model-facing interaction.

## Storage, operations, and growth

SQLite lives on the server's local filesystem, never on a shared network mount. Keep write transactions short, use indexes for scoped reads, and measure concurrency with the chosen driver. Versioned migrations run under an exclusive startup maintenance boundary before accepting requests. Startup refuses unknown newer schemas and reports recovery instructions without modifying them.

Backup is an application-owned operation with explicit destination configuration, scheduling, retention, last success, and actionable failure status. The first delivery supports a managed local destination. It captures a consistent database snapshot and an inventory of referenced immutable artifacts into an immutable, checksummed backup bundle; copying an open SQLite file is not a supported backup mechanism. Restore validates integrity, schema compatibility, references, and privacy in a new destination. Messages, job history, receipts, observation state, and review evidence must survive the complete restore. Retention and project deletion are explicit maintenance operations, not side effects of a read.

Optional cloud destinations and an operator-configured command hook can consume the same bundle later without changing its restore format. Cloud credentials remain private, and remote copies require encryption with a separately recoverable key. A custom hook is an administrator-trusted local executable with explicit arguments, a minimal environment, a deadline and bounded logs; imported project content cannot configure or execute it. Transfer completion, verified destination durability and an actually exercised restore are distinct evidence. A hook exit code alone proves only its reported outcome. Failed or uncertain transfer preserves the local bundle and reports the uncertainty; retention never removes the last usable copy merely because an upload was attempted. Provider-specific integrations are optional rather than first-release dependencies.

A disaster restore starts a new instance identity while preserving restored resource IDs. Ordinary restart keeps the existing identity. Restore reports its backup recovery point and possible lost interval; it does not promise recovery of actions committed after that backup. Restored credentials are rotated and access grants re-established before clients are admitted, so a revoked post-backup credential is not resurrected silently.

Profiles and cursors bound to the predecessor cannot transparently continue on the restored instance. Explicit rebind and scoped snapshot/resubscribe are required; event deduplication identity includes the instance. Pending requests and receipts retain their originating instance, and the adapter never relabels an old retry as a new-instance mutation. Missing post-backup receipts mean recovery is unresolved, not proof that an action never happened.

Restore produces an inactive destination; cutover fences the predecessor before mutation traffic starts. Provider mutations remain held until relevant external targets and possible post-backup effects are reconciled. Lost or ambiguous intents stay uncertain rather than being reposted automatically. Restoration of local data and permission to resume external side effects are separate proof boundaries.

The binary embeds the built browser assets and exposes health and readiness separately from provider availability. A provider outage does not fail basic service readiness while local coordination remains usable. A stopped database or failed required migration does. Structured logs record request scope, timings, provider-call counts, and error codes without message bodies or credentials by default.

One service replica is the initial supported deployment. Project-scoped storage interfaces, durable operation IDs, and replayable events permit later PostgreSQL and distributed executors. Multiple active replicas would additionally require cross-process admission, operation leasing, and shared event delivery; it is not a supported configuration merely because the API uses HTTP.
