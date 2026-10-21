# Acceptance and Performance

## Proof status

Implemented service/client boundaries have focused public tests and packaged checks recorded in [development](development.md), [message events](message-events.md), [MCP](mcp.md) and the [browser guide](browser-guide.md). These records identify tested source and limitations; they do not establish every scenario below. [Job checkpoint proof](job-checkpoints.md#proof-and-limits) covers guarded progress records, explicit reads and packaged CLI/MCP behavior. [Review workspace proof](review-workspace.md#validation-status) covers stored observation import and provider-free review reads; it does not establish native execution or the R-series admission scenarios. Fleet throughput and installed-host wakeup behavior remain unproven. Inherited test names identify source scenarios, not passing proof for this rebuild. Each delivered slice must link runnable tests and actual results here or in its PR, without claiming unfinished scenarios.

## Coordination scenarios

| ID | Scenario and required result |
| --- | --- |
| P01 | Two projects have identically named workers: lists, writes, aliases, and conversations resolve only within the selected project. |
| P02 | Guess another project's resource or pagination cursor: no contents or mutation are exposed. |
| P03 | Revoke a credential during subscription: delivery stops and reconnect is refused. |
| P04 | Two agents share a checkout and different profiles: interleaved commands keep the correct project and worker. |
| P05 | Import or create project work without public opt-in: no imported content is emitted through public projections, events, or exports. |
| J01 | Race two guarded brief edits: exactly one succeeds, the other conflicts without changing history. |
| J02 | Lose a successful creation or guarded-edit response, then retry its exact key/body after another edit: return the original receipt without extra history/event; a new key with the stale revision still conflicts. |
| J03 | Reuse a key with a different body: conflict without additional write or event. |
| J04 | Change a checklist, pause, block, or complete work: current state and history agree; one optional primary identifies resume focus without restricting other concurrent active jobs. |
| J05 | Append checkpoints and change note scope/status: current brief stays authoritative and earlier attribution remains available. |
| J06 | A routine job or note read, write, list, or inbox operation: zero provider calls and no whole-review-state decoding. |
| M01 | Send, reply, list, and open a conversation: listing and subscription do not mark it seen or handled. |
| M02 | Explicit seen and acknowledgment transitions: acknowledgment leaves seen state established atomically and does not complete the job; an unread item can be acknowledged in one deliberate action. |
| M03 | Filter unread/unacknowledged before paging; same-time replies retain deterministic chronological order. |
| M04 | Lose connection after commit and retry: one message, one event identity, and retained receipt. |
| M05 | Reconnect with processed cursor: relevant retained events replay at least once, without loss; duplicates are detectable. |
| M06 | Replay cursor expires: explicit resync, consistent snapshot cursor, and no change lost between snapshot and subscription. |
| M07 | Slow subscriber: bounded memory, disconnection/resume semantics, and no blocked message writes. |
| M08 | Cancel a wait or end a chat: no message is acknowledged and later resume still finds it. |
| C01 | Native connection binding is isolated or requires explicit profile: identical worker names never collide. |
| C02 | Host asynchronous wait overlaps ordinary commands: record delivery timing, token/output activity, timeouts, and tool-slot effects. |
| C03 | Unsupported host behavior: explicit degraded capability, no claim of automatic wakeup or hidden polling. |
| C04 | Routine output is bounded structured data without unrelated histories or notices; resume context explicitly declares any omitted standing instructions. |
| C05 | A native asynchronous long action delivers its final result without requiring operation-ID bookkeeping or polling by the model; detach and recovery still expose durable identity. |
| W01 | Browser shows changes without static rebuild or implicit GitHub work; disconnect, resync, and stale data are visible. |
| W02 | Project appearance edits affect only that project and cannot inject CSS/HTML/script or erase focus/status accessibility. |
| W03 | Markdown and public export refuse unsafe destinations and exclude private fields, message bodies, session IDs, and captures. |
| W04 | Preview legacy editorial import does not mutate; apply is idempotent and conflicts with newer revisions visibly. |
| W05 | Edit map content, phase state, or a return point: project scope, revision conflicts, and history preserve concurrent work and leave another project's map unchanged. |
| W06 | Browser attribution links to the FireController repository while project branding remains primary; agent JSON contains no platform footer or repetitive attribution. |

## Review and operations scenarios

| ID | Scenario and required result |
| --- | --- |
| R01 | Change child or parent head during admission: hold/reselect before posting; never silently target a different PR. |
| R02 | Change head immediately after final identity read: capture remains attached to admitted target, not the new head. |
| R03 | Concurrent callers request one review: one reservation/provider action; authorized callers can inspect the shared operation, while callers without inspection permission receive an opaque conflict with no private attempt details. |
| R04 | Crash at each pre-post/post/capture/publication boundary: recover original attempt or retain uncertainty without duplicate posts. |
| R05 | Provider pagination is partial or loops: evidence is incomplete and cannot authorize review/readiness. |
| R06 | Inspect status repeatedly: zero provider calls; explicit refresh is bounded to selected target/dependencies and coalesced. |
| R07 | Provider rate limit or GitHub outage: coordination still works; retry scheduling honors account-wide limits across projects. A channel-specific cooldown or active attempt does not block an independently admissible other channel; shared quota or executor exclusion blocks only affected actions. |
| R08 | Incomplete/failed/rate-limited evidence: no completed review coverage; if optional policy credit is later enabled, these results still cannot count as completed rounds. |
| R09 | Stop a review, or exhaust an optional configured channel budget: accepted finding obligations and CI requirements remain enforced. |
| R10 | Correct a finding decision or fix proof: immutable source identity, revision guard, and audited prior values survive. |
| R11 | Concurrent checkpoint publishers or lost response: one attributable public checkpoint or an explicit unresolved intent. |
| R12 | Stack parent movement: only affected evidence is invalidated, with unknown applicability held. |
| R13 | Required-check authority is missing, a same-named foreign app succeeds, or an obsolete retry passes: none can satisfy the exact candidate's current required CI. |
| R14 | Review coverage is complete but mergeability is conflicting or unknown: show review completion separately and hold merge readiness. |
| R15 | Concurrent actions target the same provider/repository/PR/action class, including changed heads: one external action and no exposure of another principal's private attempt state. |
| R16 | A developer switches branches or edits their checkout while a local review runs: its verified service-owned tree/comparison and captured evidence remain unchanged; missing server source produces an explicit capability hold. |
| R17 | Review-channel requirements are absent, or only one of two required channels has complete current-target evidence: aggregate review sufficiency remains held despite completed individual attempts or green CI. |
| P06 | Two project creations bind the same GitHub repository concurrently or via renamed/alternate URLs: one owner, explicit duplicate error, and an existing-project pointer only for authorized callers. |
| C06 | An MCP caller names a local profile outside its authorized set: reject before loading its credential or making a service call. Shared indistinguishable callers cannot claim separate grants or impersonate identity through labels. |
| O01 | Fresh install and restart with committed activity: startup, private state, profiles, and durable receipts work as documented. |
| O02 | Backup while writing, restore to new destination: integrity, references, all project state, and retained artifacts validate. |
| O03 | Unsupported newer schema or failed migration: refuse readiness without partial writes or silently rewriting live state. |
| O04 | Cancel or shut down during busy provider tasks: reservations and coordination data remain recoverable. |
| O05 | Windows/Linux packaged smoke: embedded UI and CLI need no source checkout; provider executor requirements remain explicit. |
| O06 | Restore a backup after later events, receipts, credential revocations and provider posts: predecessor profiles/cursors/retries cannot continue silently; revoked grants are not revived, lost-window effects are uncertain, and provider mutations remain held until recovery. |
| O07 | A scheduled backup fails, or its optional destination transfer times out or reports unverified success: show last usable backup and failure/uncertainty, preserve the bundle, and do not present transfer status as restore proof. |

## Result collection

Fast reads and local mutations return their useful completed outcome directly. For variable-duration source refresh, prove that fast completion returns the terminal observation in the original call, a blocked provider returns a pending receipt after the short wait, and an explicit longer wait can return the useful result while unrelated coordination continues. Immediate return, a changed wait preference and original-key recovery must retain the same durable action. Waiting uses completion notification; cancellation or timeout releases the wait without cancelling queued work, acknowledging messages or causing another provider action. Reauthorize a delayed result and prove post-commit uncertainty is retained when its response is lost or access changes. This is tool behavior rather than a project workflow checklist.

## Performance proof

Initial calibration candidates are 1,000 quiet subscriptions and 100 committed message sends per second for ten minutes on a recorded single-host reference machine, with concurrent job reads and simulated slow provider work. These are unmeasured workload candidates, not release gates or supported capacity claims. Measure representative AI sessions first, then set explicit release budgets in a reviewed benchmark plan.

Candidate latency budgets are local API send/read p95 below 200 ms and commit-to-client event p95 below 500 ms. These require calibration before becoming release gates. Measure browser transport receipt separately from model-visible delivery, which depends on the host. Record p99, errors, saturation, memory, database time, fsync/storage characteristics, CPU, process count, payload sizes, and full hardware/software versions. Do not omit failed or timed-out requests from the latency distribution.

An initial actual packaged calibration used runtime `8958e68cae8f5195fdf1a8e004a13569dedf7f97`, two projects with colliding worker display names, four separately authorized principals and 48 SSE connections. It committed 120 messages at 20 per second over six seconds while making concurrent job/inbox reads, then observed 16.54 quiet seconds. All 1,440 expected deliveries, recipient attribution and reconnect replay matched; 492 JSON API attempts had zero errors or timeouts, with no implicit seen/acknowledgment, provider requests or unrelated retained-table changes. The single workload passed in 23.025 seconds; input binaries and source stayed unchanged and all owned processes/listeners/state were cleaned. One private-helper compile error was corrected before that execution; no workload was silently retried.

| Client-observed sample | Count | p95 ms | p99 ms | Maximum ms |
| --- | --- | --- | --- | --- |
| Message send HTTP round trip | 120 | 6.855 | 12.156 | 23.235 |
| Concurrent job reads | 180 | 1.180 | 1.447 | 20.379 |
| Concurrent inbox reads | 180 | 1.103 | 2.947 | 3.383 |
| Response completion to complete SSE frame, live deliveries | 1,420 | 3.107 | 3.867 | 4.972 |
| Same frame measure including deliberately disconnected backlog | 1,440 | 3.364 | 1,197.932 | 3,998.104 |

Frame timing is a signed monotonic client observation relative to completion of the send response, not an internal commit timestamp: eleven negative samples were retained, with a minimum of -2.084 ms. The twenty intentional offline-backlog samples remain in the combined distribution. The quiet interval produced zero data frames and 48 transport keepalives totaling 624 decoded SSE bytes; no model-facing host was exercised. The WSL2 Linux 6.18.40.1 sample used an i7-12700KF, ten visible logical CPUs, Go 1.27.2, daemon `GOMAXPROCS=2`, WAL and 4 KiB SQLite pages. Sampled daemon peak RSS was 28,164,096 bytes and CPU time was 1.31 seconds. Database execution time, fsync/device behavior and saturation were not measured. This small local API/SSE calibration does not establish the candidate fleet budgets, slow-subscriber/provider isolation, installed-host delivery, current-head binary behavior or supported capacity; CLI/MCP executable proof remains separately recorded.

Run cold start, steady state, burst, reconnect storm, slow subscriber, provider outage, and retained-history growth cases. Provider calls must remain zero for job/messaging workloads and bounded/coalesced for explicit refresh workloads. An idle listener must produce no periodic model-facing output; document host timeout behavior and any actual rearm cost.

Once a release budget is selected, failure to meet it requires an explained limitation or an approved change, not hidden retries or a bigger undeclared machine. Test multi-project fairness and verify that a review running in one project does not stall another project's communication. No multi-server throughput claim is accepted before the distributed contracts are implemented and measured.

## Review gates

Docs need Markdown hygiene, repository-link checks, and verified source-test references. Code needs focused behavior and failure tests, race detection where applicable, and packaged boot proof for changed startup/runtime behavior. CI must run the same checked commands as local validation.

CodeRabbit reviews the initial design and meaningful implementation checkpoints. Human review decides consequential choices; automated review cannot approve the architecture on the human's behalf. Report current and outdated unresolved threads separately, inspect summary-only findings, and leave merges to the human.
