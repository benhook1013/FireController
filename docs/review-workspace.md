# Native Review Workspace

## Implemented boundary

The service stores project-scoped review observations and serves queue, PR history, run, finding, decision and fix-proof views from its own indexed database. Importing an observation is an explicit administrator action. Ordinary reads and browser refresh make no provider calls and do not construct another controller or renderer. Jobs and messaging do not depend on the reviews domain.

This slice provides the read model for the [review engine](reviews.md). Optional native execution and revision-bound policy admission are described in the [execution boundary](review-execution.md). Native queue selection and explicit source refresh publish selected current display facts through the guarded path below; browser reads never trigger that provider work. Explicit [captured-comparison policy assessment](review-controls.md#captured-comparison-assessment) is a separate provider-free read. Historical counts, a completed attempt, CI success and a matching head are separate facts. This API never returns a merge-ready verdict or authorizes a merge. Projects without observations return an explicitly unconfigured workspace.

## Optional PR display facts

PR observations, queue edits, additive transfers and queue/detail reads accept optional `diff: {files, additions, deletions}`, `is_draft` and `merged_at`. Diff counts form one atomic observation of nonnegative 32-bit integers; a known zero remains zero. An omitted diff or draft state is unknown. `merged_at` is an actual RFC3339 merge instant and belongs only to a merged PR. It never falls back to a review completion or observation time. These facts describe the PR, not review coverage, policy, CI readiness or permission to request work.

Review schema seven appends bounded canonical display metadata to immutable PR versions. Startup upgrades existing versions to unknown metadata, preserving their logical facts and pinned response behavior. Current native publication preserves metadata already known for the same head; changing the head discards older metadata unless the writer supplies fresh facts. Historical captured comparisons do not overwrite current PR metadata. Selected source preparation forwards known metadata explicitly; source collection retains provider facts, but does not by itself publish them into the review workspace.

The browser shows observed file and line counts, a Draft badge when known true on an open PR, and a bold relative age from the actual merge time. Older records with unknown metadata omit those details. Public projection retains its explicit allowlist; adding private display facts does not enroll them for publication.

## API and scope

All paths below are relative to `/api/v1/projects/{project_id}`. Existing instance authentication, current project grants, bounded transactions and safe error envelopes apply.

| Method and path | Result |
| --- | --- |
| `POST /reviews/observations` | Administrator-only, revision-guarded observation append with a required idempotency key. |
| `POST /reviews/import/preview` | Administrator-only, provider-free prepared observation preview; no receipt or write. |
| `POST /reviews/import/apply` | Administrator-only prepared observation apply bound to its preview, with the existing observation receipt semantics. |
| `POST /reviews/queue/edits` | Administrator-only bounded selected metadata, retirement and reactivation; preserves omitted history. |
| `GET /reviews/fronts` | Provider-free current per-channel assessment over a bounded verified source frame; see [fronts](review-fronts.md). |
| `GET /reviews/queue` | Compact ordered PR summaries, including merged and closed history. |
| `GET /reviews/pulls/{number}` | One compact PR summary, including latest attempt facts per channel. |
| `GET /reviews/pulls/{number}/runs` | Newest-first run summaries with cursor pagination. |
| `GET /reviews/pulls/{number}/attempts` | Newest-first attempt archive with original producer and cursor pagination. |
| `GET /reviews/runs/{run_id}` | One run with its recorded findings, decisions and fix evidence. |

Queue, run and attempt lists default to 20 items and accept `limit` up to 100 and an optional signed `cursor`. Pull and run detail accept an optional `snapshot`; run and attempt lists accept it too. Each configured response identifies its project, revision, snapshot, source, observed time and recorded time. An unconfigured queue has `configured:false`, revision zero, empty metadata and an empty item list. No empty result implies passing coverage. PR detail never embeds the attempt archive; use its explicit page route. Each archive item includes its original producer in `source`, distinct from the snapshot publisher. Attempt identity is `(source, source_attempt_id)` within a project; external IDs may coincide across producers.

Cursors bind the principal, project, route, order and retained snapshot. New imports cannot change a continuation page from an older snapshot. Detail links retain that snapshot for consistent drill-down. Access is checked again on every request, including historical reads and exact mutation retries. Unknown, duplicate and malformed query fields are rejected. PR numbers use canonical positive decimal values through 2147483647.

## Administrator observations

The [prepared import contract](review-import.md) owns guided preview/apply and profile-bound administrator CLI commands. These routes share the observation transaction below; preview does not reserve a workspace revision.

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

### Source metadata publication

Normal native queue selection and explicit source refresh reuse the verified PR facts they already read to update existing visible queue entries. Publishing these display facts does not spend a review, change round policy or grant dispatch permission. Browser reads and Refresh remain local stored reads; this publication path adds no GitHub calls, recurring crawl or background history import.

Before provider I/O, pin each selected current entry's display facts. At publication, update only entries whose facts still match that baseline. Preserve concurrent attempts, findings, decisions, queue order and all historical snapshots. A concurrent display edit wins over an older in-flight provider response; a retired or missing entry is never reactivated by this path. Unchanged facts produce no snapshot or event. A changed head clears inherited completeness. Changing the head or base makes previously stored CI and mergeability unknown until separately observed. Optional display facts preserve known same-head values when the provider did not supply a replacement.

Partial source publication does not advance the whole queue's observation timestamp. It is distinct from review history acquisition, explicit administrator queue edits and the initial missing-projection fallback. Review-front badges additionally require an authoritative per-channel selection projection; merge-head order and retained review counts cannot establish that permission.

Merged PRs remain in the queue as completed history. Removing any previous queue entry requires an explicit `superseded` or `defunct` retirement, through a complete observation or the selected queue edits below. Lifecycle changes never remove entries implicitly. Snapshots and retirement provenance remain retained; this slice does not introduce a pruning or project-deletion API.

The browser exposes Reviews, PR history and run-finding pages, with source timestamps, incomplete or missing data, head mismatches, historical counts and source coverage limitations visible. It escapes imported text and generates external PR/commit links only from the verified project repository. It does not render imported HTML or accept arbitrary link destinations. “Refresh visible data” rereads the stored snapshot; it neither refreshes GitHub nor starts a review. These pages have no timer-based polling or new live transport. The [browser guide](browser-guide.md) owns connection privacy and packaging.

An initial installation may populate this boundary through a small explicit import. Profile-bound CLI commands `reviews queue`, `reviews pull`, `reviews runs`, `reviews attempts` and `reviews run`, plus the five matching [MCP read tools](mcp.md), inspect these stored projections without provider work. Queue/run/attempt tools default to five entries, while explicit run detail loads full findings. Snapshot selectors retain historical consistency. A general migration framework and continuous legacy compatibility service remain separate deliveries. Optional [native executors](review-execution.md) publish retained native evidence through their own trusted path. Review observation writes are not exposed as worker tools.

## Selected queue edits

Administrator queue edits are implemented through the API, typed client and bound CLI. Their focused and packaged proof is recorded below; the earlier complete-observation proofs remain separate.

Complete observations replace a small workspace and retain their per-request bounds. A large workspace uses additive transfer for selected history and `POST /api/v1/projects/{project_id}/reviews/queue/edits` for selected metadata, retirement and reactivation. Neither operation requires a complete project export or a lifetime PR ceiling.

Queue edits require administrator access, an explicit idempotency key, the current `expected_revision`, the configured `repository_id` and a bounded `source` label for administrator provenance. The strict JSON document is limited to 256 KiB. Its `queue` and `retired` arrays select between one and one hundred entries in total, with unique, disjoint PR numbers. Omitted arrays become empty; explicit null fields, unknown fields and duplicate keys are refused.

Each `queue` entry contains `number`, `title`, `state`, `head_sha`, `base_ref`, `base_sha`, `ci_state` and `mergeability`, with optional `diff`, `is_draft` and `merged_at` as defined above. Omitted optional SHA/base strings become empty. Runs, attempts, findings, counts and completeness cannot be supplied. An existing PR keeps its position and retained producer histories; changing its head clears completeness. A new or previously absent PR appends with partial coverage. Explicitly selecting a retired PR reactivates it without inventing review evidence.

Each `retired` entry names a currently visible PR and its `superseded` or `defunct` reason. Retirement changes queue visibility, preserving retained parent records and review histories. Later native publication can update retained facts without making the PR reappear. If complete observations have removed its current parent and retirement marker, native reintroduction preserves the latest retained retirement reason; unrelated replacement observations do not authorize reactivation. Historical transfer refuses a marked PR until explicit reactivation. Explicit retirement also governs a retained snapshot containing both a pull record and its retirement; the presence of the pull record alone cannot make it visible. Retirement does not cancel review operations, change execution policy or unpublish an explicitly enrolled public PR.

Omitted PRs and records remain unchanged. Revision admission, the new snapshot, history references, compact summaries, receipt, event and public projection refresh commit together. A queue edit inherits the prior observation time and does no provider work; it cannot claim freshly collected source facts. Exact original-key replay returns its original receipt after later changes or restart, subject to current authorization. A changed request under the same key conflicts. Successful HTTP 201 returns only the snapshot `id`, new `revision` and `outcome: queue_edited`.

The bound administrator CLI is:

```sh
fc reviews queue-edit --file /private/queue-edit.json --key retained-action-key
```

It validates a bounded regular file before credentials or network access, retains the ordinary five-second budget and never generates a key or retries automatically. A lost or invalid mutation response requires recovery with the original request, key and binding. This administrator maintenance action is not a worker MCP tool.

### Queue edit proof

Focused normal and race checks exercise a workspace grown through bounded transfers to 202 PRs, selected metadata and visibility changes, mixed original producers, native advancement while hidden, native reintroduction after complete observations remove the current parent and marker, explicit reactivation and transfer, retained merged PRs, and unchanged valid historical cursor/detail bytes. They also cover administrator admission, strict input bounds, revision conflicts, exact replay after later writes, lost responses, cancellation and event-failure rollback. History and current-summary validators pass; scoped vet, formatting and documentation checks pass. Independent source inspection found no additional implementation defect.

An actual packaged `fc`/`fcd` workload passed in 1.530 seconds on a private source candidate. Three bounded transfers admitted 202 PRs; selected edits and restart replay preserved original review detail, observation time, omitted merged history and old cursor bytes. Canonical backup, inspection and inactive restore preserved retained state, rotated recovery identity and revoked copied credentials; the restored daemon refused startup before activation. The workload used 28 CLI calls and four operator invocations, with zero external requests or surviving owned processes. Supplied source and binary hashes remained unchanged. This is local synthetic Linux proof, separate from full CI, browser rendering, installed deployment, native-platform execution, provider qualification and operational cutover.

## Validation status

Focused public API tests exercise scoped reads/admin admission, strict input bounds, exact replay, revision and repository conflicts, explicit retirement, stable snapshot pages, immutable source evidence, transactional rollback/retry and revocation. The integrated `make check build` passed Go race tests/vet, TypeScript, thirty-two browser tests, documentation checks and embedded asset generation. Four actual packaged API/integration groups passed, covering administrator-only import, replay/CAS/stale/source conflicts, retained merged history, unknown durations, snapshot-stable cursors and corrected fix details, explicit retirement, failed-key retry and restart retention. Three browser groups passed across empty and populated workspaces, PR/run/finding views and 390px layouts. Seven screenshots were inspected.

The empty and populated browser sessions made eight and twelve GETs respectively, with zero mutations, external requests, page/console/CSP errors, horizontal overflow or credential persistence. Independent read-only database counts showed no browser or rejected-request effects. Setup explicitly verified one public repository; a direct service outbound-provider counter was not instrumented, so provider-free reads are also supported by inspection of their local SQL handlers.

The 98-file Go/browser/module/config/asset manifest stayed unchanged: SHA-256 `b0f440033d41aae6639c6ac9bc0aff267b0afd6a32c3d0755c243fb804ee9d02`. The actual service executable and its private copy retained SHA-256 `3e9c1d45478cd5b48adcdc001f266ea6c62b24da1d0a12282475ee189fbe0bc0`. These identify the exact runtime source tested, excluding documentation-only changes. Disposable state and owned processes were cleaned.

These checks use synthetic observations on Linux/WSL Chromium. They do not establish native execution, a complete migration product, populated multi-project browser coverage, remote deployment, fleet throughput or installed-agent behavior. Existing authentication/revocation and strict-parser tests provide separate focused evidence.

## Agent reads and restored review history

The bound CLI and MCP review reads passed full repository checks and six actual packaged transport groups. Forty-nine malformed raw MCP requests, thirty-two invalid CLI requests and all four valid reads with missing credentials produced zero HTTP calls. Discovery exposed twenty-three tools: seventeen reads and six writes. Actual reads preserved default-five pagination, historical snapshot continuation after a newer import, full finding/fix details, null durations and profile/header scope, with unchanged durable counts. The CLI and MCP five-second deadline observations were 5.004 and 5.002 seconds; explicit cancellation closed the forwarded HTTP request in 0.001 seconds. These are local observations, not a fleet or installed-host scheduling guarantee.

The tested 83-file Go/module manifest remained SHA-256 `584f6a065f857777c19d0936e6c366a3d393f5e5982d799ed3f5112570d8de62`. Full run detail has a 4 MiB response budget; queue, PR and run-summary reads use 1 MiB. Source counts remain distinct from effective corrected finding/fix state. Omitted snapshot selectors read the current snapshot; explicit selectors remain bound to their historical response.

The browser restores separate Hosted and CLI rows with loaded retained rounds ordered oldest to newest, compact found/accepted/routed pills and ages. The original mixed last-three summary hid the independent review progression and was removed. Queue cards read the first twenty native run summaries for nonempty histories and expose an explicit Load more continuation; empty histories need no extra request, and finding details remain explicit. Manual/subagent groups remain separate. This restores the useful trend without inventing taper or admission state. The broader [browser presentation contract](browser.md#project-presentation) requires continuity with the refined dashboard defaults.

Four packaged API/integration groups and three browser groups passed again after this restoration, including safe text, links, empty state, grouped channel rows and unchanged read state. The 104-file Go/browser/module/config/asset manifest was `745303f3a2a79a3f91807cc00cd48d76ef5daf572d6ee9a8b30d6e36ec968d68`; the tested service binary was `45901e044fbd3463fff7a5c0a5d896d2b232b40a9ea2f0266404e4e3241e0234`. Synthetic desktop and phone captures were inspected. Native policy indicators, a selected review front and routed/orphan record views still require their corresponding native data; presentation does not infer them from raw counts.

## Bounded archive implementation

Run and attempt archives use explicit snapshot-bound pages. The browser initially requests twenty summaries; each Load more action requests one additional page, with no automatic archive drain. Current snapshot refresh resets loaded continuations; explicitly selected historical snapshots remain fixed. Summary totals describe retained records, while the shown count and continuation describe loaded records. Complete observations retain per-operation count limits; the [history growth contract](review-history-growth.md) owns versioned storage, additive transfer and native publication beyond retained-total boundaries. Validation of this addition is recorded after the integrated checks; earlier proof above refers to its named historical checkpoints.

At clean combined checkpoint `769a5b8b65e2420a7a2b726215d75c8e37ab031e`, `make check build` passed all 296 browser tests, Go race tests, vet, workflow/formatting/document checks, embedded assets and all three binaries. The packaged API/CLI/MCP, upgrade, publication and map-import groups passed in 1.618 seconds; private/public/archive Chromium groups passed in 9.547 seconds against those same outputs. The new archive workload uses a controlled retained fixture with 123 runs and 123 attempts, including the same external attempt ID under two producers. It proves default-five agent pages, explicit next pages without automatic draining, twenty-item browser requests, continuation through the complete original snapshot after a newer observation, current-view reset and preservation of unrelated retained state. It makes one separately counted fixture observation, rejects external/provider traffic and cleans up its owned sessions/processes. This is local executable proof; it does not establish installed deployment, human GUI acceptance, lifetime-cap-free writes or complete historical collection.

## Optional display metadata proof

Focused normal and race checks cover optional bounds and known zero values, immutable pinned metadata, same-head preservation and changed-head clearing, native publication from current facts, migration from genuine schema six, and canonical backup creation/inspection/restore with genuine older schemas. An independent backend review found no additional defect. The clean packaged candidate passed private browser, retained archive, refresh, public browser, editorial browser, CLI/MCP smoke and upgrade checks. The archive verifies current metadata changes while pinned detail stays unchanged, and only two selected finding-detail reads are added to its existing twenty-one archive reads. These checks do not establish installed schema-seven migration, automatic provider-to-workspace metadata publication or human visual acceptance.
