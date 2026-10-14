# Decision Record

## Status and authority

Status: **Proposed - Pending Human Review** for technical decisions below. The human approved the independent public rebuild, multi-project scope, explicit per-agent context, live browser, and prioritizing dependable operation in the October 14, 2026 conversation. That approval defines the initial requirements in [product scope](product.md); it is not a fabricated review of this written architecture.

CodeRabbit is a defect reviewer. Human design disposition and its source must be recorded explicitly before marking a consequential decision accepted. No proposed decision overrides a contrary existing accepted contract.

## Decisions to review

| ID | Proposed choice | Reason and consequence | Owner |
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

## Confirmed project choices

- Public repository `benhook1013/FireController`, independent WSL checkout, and Apache-2.0 for new material were authorized in this conversation. Apache-2.0 was recommended after the human selected broad open reuse and no financial goal. Historical source material retains its original license unless a separate rights decision establishes otherwise.
- Require PRs to `main`, disable force pushes and deletion, enforce conversation resolution, and use deliberately requested CodeRabbit reviews. AI workers never merge.
- Existing deployments require explicit migration and cutover; publishing this repository does not change their installed versions or private state.
- The October 15 discussion confirmed the initial one-project/one-GitHub-repository boundary, with duplicate bindings rejected inside an instance and stable repository identity across renames. GitHub Projects boards are separate from this identity.

## Open questions

Select the SQLite driver only after packaging, concurrent workload, and crash/restart proof. Select browser and API schema libraries for small dependency footprint and maintainability, not framework popularity. Native agent adapter capabilities must be verified against actual hosts before documenting zero-argument tools or automatic delivery as supported.

Legacy private website content and rights have not been exhaustively inspected. Retained JSON/editorial state, theme settings, provider captures, and historical migration requirements need an explicit migration inventory. No live legacy files are imported during design work.
