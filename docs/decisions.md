# Design Decisions

These decisions define FireController’s target design. The [product scope](product.md) defines its requirements, and the linked owning documents specify the detailed contracts. Design changes are reviewed through pull requests; implementation and proof status are tracked in [delivery](delivery.md).

## Selected design

| ID | Decision | Reason and consequence | Owner |
| --- | --- | --- | --- |
| D01 | Go service and CLI, embedded TypeScript UI | Simple executable deployment; build tooling stays separate from user runtime. Validate SQLite-driver and cross-platform packaging before finalizing dependencies. | [Architecture](architecture.md) |
| D02 | One instance supports multiple isolated projects | Avoid multiple servers for hobby projects; project scope must exist throughout storage, access, cursors, subscriptions, and provider operations. | [Architecture](architecture.md) |
| D03 | SQLite and one active service replica initially | Keep installation simple and transactional state local. Multiple replicas need a later database/lease/event design, not an unsupported flag. | [Architecture](architecture.md) |
| D04 | Durable events, at-least-once delivery, seven-day replay | Recover disconnects with deduplication; expired cursors require an atomic snapshot and resubscribe. Messages survive independently of events. | [Architecture](architecture.md) |
| D05 | Private profiles and verified connection binding | Reduce flags without shared mutable identity. Shared host connections retain an explicit selector. | [Agent contract](agent-client.md) |
| D06 | Bounded asynchronous waits over an adapter subscription | Quiet message delivery without model polling where host support exists. Host timeout, completion injection, and idle wakeup require separate proof. | [Agent contract](agent-client.md) |
| D07 | Stored status reads and explicit/scheduled scoped refresh | Centralize GitHub work and make freshness visible; consequential actions still require their exact fresh facts. | [Review contract](reviews.md) |
| D08 | Durable mutation keys and provider reservations | Prevent ambiguous retries from duplicating actions. Initial mutation-key retention is indefinite until project deletion. | [Architecture](architecture.md) |
| D09 | Live UI with per-project presentation and editorial records | Retire private static regeneration; classify legacy JSON before importing. Public projection remains separate. | [Browser](browser.md) |
| D10 | Preserve tested review safety; defer advanced policy as an optional layer | Deliver attribution, findings, identity, and recovery first; retain taper/allocations/periods in the evidence inventory without making them core-engine dependencies. | [Review contract](reviews.md) |
| D11 | Start with minimal scoped access and loopback credentials | Permit network-host deployment later through TLS without designing hostile public SaaS tenancy now. | [Architecture](architecture.md) |
| D12 | Calibrate concurrency, output, and latency before release budgets | Make throughput claims measurable and expose provider interference; candidate numbers are not gates before measured review. | [Acceptance](acceptance.md) |
| D13 | One optional primary job as resume focus | Preserve convenient authoritative context selection while allowing multiple concurrent active jobs. | [Product](product.md) |
| D14 | Canonical repository binding and external-action exclusion | One project owns a GitHub repository within an instance; action-class reservations also prevent concurrent duplicate posts and survive uncertain outcomes. | [Architecture](architecture.md), [review contract](reviews.md) |
| D15 | SDK-backed MCP interface alongside CLI and browser | A packaged local adapter uses the service API; remote MCP can follow without duplicating behavior. Verify supported protocol versions, caller isolation and host completion capabilities. | [Client contract](agent-client.md) |
| D16 | New instance identity and held provider recovery after disaster restore | Separate restart from rollback; require explicit rebind/resync, credential re-establishment and reconciliation of possible post-backup effects. | [Architecture](architecture.md) |
| D17 | Service-owned immutable workspaces for local reviews | Configure server sources administratively and prove the executor reads the admitted tree independently of developer changes. | [Review execution](reviews.md) |
| D18 | Explicit required channels for initial review sufficiency | One complete applicable capture per selected channel, with finding obligations met; missing policy never means success. | [Review policy](reviews.md) |
| D19 | Managed backups with optional destination adapters | Own scheduling, retention and failure status; start locally and preserve one restore format for later cloud or trusted custom-command destinations. | [Backup and recovery](architecture.md#storage-operations-and-growth) |

## Implementation selection and migration work

Select the SQLite driver only after packaging, concurrent workload, and crash/restart proof. Select browser and API schema libraries for small dependency footprint and maintainability, not framework popularity. Native agent adapter capabilities must be verified against actual hosts before enabling zero-argument tools or automatic delivery for an installation.

Legacy private website content and rights have not been exhaustively inspected. Retained JSON/editorial state, theme settings, provider captures, and historical migration requirements need an explicit migration inventory. No live legacy files are imported during design work.
