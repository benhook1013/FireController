# Native Review Workspace

## Implemented boundary

The service stores project-scoped review observations and serves queue, PR history, run, finding, decision and fix-proof views from its own indexed database. Importing an observation is an explicit administrator action. Ordinary reads and browser refresh make no provider calls and do not construct another controller or renderer. Jobs and messaging do not depend on the reviews domain.

This slice provides the read model for the [review engine](reviews.md). Optional native execution and revision-bound policy admission are described in the [execution boundary](review-execution.md); automatic provider refresh, fresh-source aggregate policy and merge-readiness assessment remain unimplemented. Explicit [captured-comparison policy assessment](review-controls.md#captured-comparison-assessment) is a separate provider-free read. Historical counts, a completed attempt, CI success and a matching head are separate facts. This API never returns a merge-ready verdict or authorizes a merge. Projects without observations return an explicitly unconfigured workspace.

## API and scope

All paths below are relative to `/api/v1/projects/{project_id}`. Existing instance authentication, current project grants, bounded transactions and safe error envelopes apply.

| Method and path | Result |
| --- | --- |
| `POST /reviews/observations` | Administrator-only, revision-guarded observation append with a required idempotency key. |
| `GET /reviews/queue` | Compact ordered PR summaries, including merged and closed history. |
| `GET /reviews/pulls/{number}` | One PR summary and its retained attempts. |
| `GET /reviews/pulls/{number}/runs` | Newest-first run summaries with cursor pagination. |
| `GET /reviews/runs/{run_id}` | One run with its recorded findings, decisions and fix evidence. |

Queue and run lists default to 20 items and accept `limit` up to 100 and an optional signed `cursor`. Pull and run detail accept an optional `snapshot`; run lists accept it too. Each configured response identifies its project, revision, snapshot, source, observed time and recorded time. An unconfigured queue has `configured:false`, revision zero, empty metadata and an empty item list. No empty result implies passing coverage.

Cursors bind the principal, project, route, order and retained snapshot. New imports cannot change a continuation page from an older snapshot. Detail links retain that snapshot for consistent drill-down. Access is checked again on every request, including historical reads and exact mutation retries. Unknown, duplicate and malformed query fields are rejected. PR numbers use canonical positive decimal values through 2147483647.

## Administrator observations

The strict JSON body requires `expected_revision` (zero for the first observation), the configured stable GitHub `repository_id`, a bounded `source`, an RFC3339 `observed_at`, ordered `queue` entries and explicit `retired` entries. Omitted arrays normalize to empty arrays. The body and its normalized form are bounded to 256 KiB. A changed request under an existing idempotency key conflicts; an exact retained replay returns the original receipt before re-evaluating the revision. Replay still requires current authorization.

The repository identity is checked inside the write transaction. A stale expected revision, an observation older than the current snapshot, or immutable source evidence conflicts rather than overwriting history. An observation more than five minutes in the future is invalid. The snapshot, indexed projections, current pointer, compact `review.observed` event and receipt commit together. Failed writes leave none of those effects. The event contains only the snapshot identity and queue count; findings and proof text stay out of event notifications.

An observation can contain up to 100 unique PRs; each has identity, lifecycle, head/base, observed CI and mergeability, a structured completeness flag, and ordered runs and attempts. Missing provider facts remain unknown. Input run and attempt arrays are oldest first; retained producer order resolves equal timestamps. A source is responsible for honest observation time and completeness. The service does not verify imported provider assertions by contacting GitHub.

Nonempty commit hashes use exactly 40 lowercase hexadecimal characters across observations, API responses, agent clients and the browser. Empty values remain unknown where permitted. Imports reject uppercase or mixed-case values before persistence; they do not normalize immutable source identities after admission.

Runs preserve their source ID, channel, head, reviewer, scope, outcome, attribution, structured completeness, timestamps, finalized counts and source coverage limitations. Channels include CLI, hosted, manual and subagent history; supporting historical channels does not install an executor. At most 100 runs per PR and 200 per observation are accepted. Counts are nonnegative, accepted plus routed cannot exceed found, and a structurally complete run includes exactly its reported finding count. A completed run requires ordered start and finish times. Completeness describes retained data; it does not establish successful review coverage or applicability to a current patch.

Every run summary also carries its original producer in `source`. The run identity is `(project, source, source_run_id)`; two producers can use the same external run ID without becoming the same run. Top-level `source` describes the snapshot publisher and does not relabel its historical runs. Older compact projections obtain their original source during reads without modifying retained bytes; new mixed projections store the actual source of every run. Full run detail shows that origin explicitly.

Findings preserve source identity, title, plain detail, severity, disposition, decision reason/actor/time, effective fix SHA and proof/time. Limits are 100 findings per run and 1000 per observation; detail is at most 16 KiB, decision/proof text 2 KiB each. Source coverage limitations are at most 16 strings of 1024 bytes each. Text must be valid UTF-8 without unsupported control characters. Source fix evidence belongs to an accepted finding. Native routed findings additionally expose a compact [receiving-route projection](review-routes.md), including its separate disposition, revision and fix proof; that proof never increments the source run’s fixed count. Raw provider captures, diagnostics and credential material are outside this read model.

Finalized source counts and source run/finding identity are immutable across observations. A later observation may add missing structured evidence or record corrected decision/fix evidence while retaining earlier snapshots; it cannot move the same finalized run to another PR, channel, head or scope, or replace known finding text under unchanged counts. Native run IDs derive from project, source and source run identity, rather than exposing filesystem paths.

Attempts retain source IDs, channel, head, state, timing, nullable duration and optional source-run/provider references, bounded to 100 per PR and 200 per observation. Failed, rate-limited, uncertain and unknown attempts remain visible. An unknown duration is null and displays as “Unknown”; measured zero remains zero. A process exit code alone does not establish completed coverage. Queue summaries contain only the three most recent runs and latest attempt per channel; full findings load explicitly on a run page.

## Retention and browser behavior

Merged PRs remain in the queue as completed history. Removing any previous queue entry requires an explicit `superseded` or `defunct` retirement in the new observation. Lifecycle changes never remove entries implicitly. Snapshots and retirement provenance remain retained; this slice does not introduce a pruning or project-deletion API.

The browser exposes Reviews, PR history and run-finding pages, with source timestamps, incomplete or missing data, head mismatches, historical counts and source coverage limitations visible. It escapes imported text and generates external PR/commit links only from the verified project repository. It does not render imported HTML or accept arbitrary link destinations. “Refresh visible data” rereads the stored snapshot; it neither refreshes GitHub nor starts a review. These pages have no timer-based polling or new live transport. The [browser guide](browser-guide.md) owns connection privacy and packaging.

An initial installation may populate this boundary through a small explicit import. Profile-bound CLI commands `reviews queue`, `reviews pull`, `reviews runs` and `reviews run`, plus the four matching [MCP read tools](mcp.md), inspect these stored projections without provider work. Queue/run tools default to five entries, while explicit run detail loads full findings. Snapshot selectors retain historical consistency. A general migration framework and continuous legacy compatibility service remain separate deliveries. Optional [native executors](review-execution.md) publish retained native evidence through their own trusted path. Review observation writes are not exposed as worker tools.

## Validation status

Focused public API tests exercise scoped reads/admin admission, strict input bounds, exact replay, revision and repository conflicts, explicit retirement, stable snapshot pages, immutable source evidence, transactional rollback/retry and revocation. The integrated `make check build` passed Go race tests/vet, TypeScript, thirty-two browser tests, documentation checks and embedded asset generation. Four actual packaged API/integration groups passed, covering administrator-only import, replay/CAS/stale/source conflicts, retained merged history, unknown durations, snapshot-stable cursors and corrected fix details, explicit retirement, failed-key retry and restart retention. Three browser groups passed across empty and populated workspaces, PR/run/finding views and 390px layouts. Seven screenshots were inspected.

The empty and populated browser sessions made eight and twelve GETs respectively, with zero mutations, external requests, page/console/CSP errors, horizontal overflow or credential persistence. Independent read-only database counts showed no browser or rejected-request effects. Setup explicitly verified one public repository; a direct service outbound-provider counter was not instrumented, so provider-free reads are also supported by inspection of their local SQL handlers.

The 98-file Go/browser/module/config/asset manifest stayed unchanged: SHA-256 `b0f440033d41aae6639c6ac9bc0aff267b0afd6a32c3d0755c243fb804ee9d02`. The actual service executable and its private copy retained SHA-256 `3e9c1d45478cd5b48adcdc001f266ea6c62b24da1d0a12282475ee189fbe0bc0`. These identify the exact runtime source tested, excluding documentation-only changes. Disposable state and owned processes were cleaned.

These checks use synthetic observations on Linux/WSL Chromium. They do not establish native execution, a complete migration product, populated multi-project browser coverage, remote deployment, fleet throughput or installed-agent behavior. Existing authentication/revocation and strict-parser tests provide separate focused evidence.

## Agent reads and restored review history

The bound CLI and MCP review reads passed full repository checks and six actual packaged transport groups. Forty-nine malformed raw MCP requests, thirty-two invalid CLI requests and all four valid reads with missing credentials produced zero HTTP calls. Discovery exposed twenty-three tools: seventeen reads and six writes. Actual reads preserved default-five pagination, historical snapshot continuation after a newer import, full finding/fix details, null durations and profile/header scope, with unchanged durable counts. The CLI and MCP five-second deadline observations were 5.004 and 5.002 seconds; explicit cancellation closed the forwarded HTTP request in 0.001 seconds. These are local observations, not a fleet or installed-host scheduling guarantee.

The tested 83-file Go/module manifest remained SHA-256 `584f6a065f857777c19d0936e6c366a3d393f5e5982d799ed3f5112570d8de62`. Full run detail has a 4 MiB response budget; queue, PR and run-summary reads use 1 MiB. Source counts remain distinct from effective corrected finding/fix state. Omitted snapshot selectors read the current snapshot; explicit selectors remain bound to their historical response.

The browser restores separate Hosted and CLI rows with all retained rounds ordered oldest to newest, compact found/accepted/routed pills and ages. The original mixed last-three summary hid the independent review progression and was removed. Queue cards read bounded native run summaries for nonempty histories; empty histories need no extra request, and finding details remain explicit. Manual/subagent groups remain separate. This restores the useful trend without inventing taper or admission state. The broader [browser presentation contract](browser.md#project-presentation) requires continuity with the refined dashboard defaults.

Four packaged API/integration groups and three browser groups passed again after this restoration, including safe text, links, empty state, grouped channel rows and unchanged read state. The 104-file Go/browser/module/config/asset manifest was `745303f3a2a79a3f91807cc00cd48d76ef5daf572d6ee9a8b30d6e36ec968d68`; the tested service binary was `45901e044fbd3463fff7a5c0a5d896d2b232b40a9ea2f0266404e4e3241e0234`. Synthetic desktop and phone captures were inspected. Native policy indicators, a selected review front and routed/orphan record views still require their corresponding native data; presentation does not infer them from raw counts.
