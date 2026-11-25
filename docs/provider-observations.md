# Provider observation boundary

This boundary covers bounded GitHub reads, normalized history and parent facts, required-check rule decoding and pure Hosted review decoders in `internal/reviewprovider`. Callers supply repository, PR, commit, actor and command identities. Observation and parsing do not establish execution authority, native coverage, adjudication or merge readiness. The [CI contract](review-ci.md) and [execution contract](review-execution.md) own their consuming workflows.

## GitHub source reads

Observation requests retain explicit selectors, bounded response bodies, pagination and collection deadlines. Incomplete inventories, changing selectors, malformed identities and exceeded limits remain errors or unproven facts. Transport interruption differs from invalid completed JSON; safe error classes preserve provider status and retry guidance without exposing raw response bodies or credentials.

Each GitHub adapter owns a bounded idle connection pool (at most eight idle connections, expiring after thirty seconds). Successive metadata windows can reuse connections without skipping fresh requests, sharing credentials across adapters, increasing selection budgets or replaying an uncertain mutation. Existing request deadlines and redirect refusal remain unchanged.

Conditional GET reuse is scoped to one authenticated adapter and exact request URL. Only a matching server `304` permits reuse of retained response bytes and pagination links. The cache is bounded; typed decoding and pagination checks still run after revalidation. A cached response never establishes collection completeness by itself.

Parent lookup returns a unique open same-repository PR whose head branch is the selected base only after a fresh matching read of that PR. A complete unpaginated empty inventory returns a zero `Pull` without an error. Valid multiple matching candidates or a valid paginated candidate page returns `ErrParentAmbiguous` without another fetch. Malformed candidate inventories, unsafe/malformed pagination and ordinary provider failures retain their provider error classes. Moving or mismatched fresh parent identity remains an error. Queue position and historical captures cannot supply that identity. Explicit history-page readers preserve original IDs, actors, bodies, reply relationships and locations. Normalized history validation checks one selected page; cross-page completeness, durable acquisition and import authority remain caller responsibilities.

Required-check rule decoding preserves the distinction between documented rules and unavailable or unsupported authority. Decoding a requirement set does not establish current workflow success; CI selection and assessment remain with their existing owner.

## Nonterminal Actions diagnostics

`ObserveActionsLiveness` is an explicitly invoked provider-adapter diagnostic, not a required-CI assessment or a watchdog. It scans `queued`, `in_progress`, `waiting`, `pending` and `requested` runs without a head filter, so an older-head observation is not hidden by current-head success. Every returned row retains the exact repository, run, workflow, suite, run number, attempt, head, event, status and timestamp facts. Duplicate run IDs, contradictory workflow/run-number identities, malformed responses, moving totals and incomplete pagination refuse the entire result rather than returning partial facts.

One scan has a fifteen-second aggregate deadline, at most two hundred rows per status and at most ten GET requests. Existing request, response-size, aggregate-body, redirect and provider-error bounds remain in force. The caller selects a diagnostic age between one hour and thirty days. `Aged` compares the returned update timestamp to the scan's start time; it does not establish absence of job progress, execution death or permission to recover the run. The returned observation timestamp marks completion, not an atomic repository snapshot. Runs may move between independently read status buckets without being observed, so even a successful empty scan cannot prove that no nonterminal run exists.

Capacity is always `unknown`: a queued workflow does not establish runner occupancy, concurrency-lock ownership or exhausted capacity. The explicit service, CLI and MCP read described below consumes this adapter. It has no recurring scheduler, durable observation history, job-progress diagnosis or cancellation/retry implementation. The explicit exact-attempt job read below adds bounded observed job facts; repeated source-bound progress evidence remains separate from any future recovery decision. Current-head green CI never resolves an older queued run, and this diagnostic never grants a passing required-check verdict or authorizes a cancellation, restart, duplicate attempt or change to shared limits.

## Explicit Actions diagnostic read

`POST /api/v1/projects/{project_id}/reviews/actions-liveness/observe` is a one-shot read requiring current project read permission. Its strict JSON object requires `age_hours` (1–720) and `limit` (1–20); query selectors, repository/capability overrides and idempotency keys have no diagnostic authority. `fc reviews actions-liveness [--age-hours N] [--limit N]`, typed `Client.ActionsLiveness` and MCP `actions_liveness_read` use the existing bound project, defaulting to 24 hours and 10 rows. MCP correctly declares an open-world read because the call contacts GitHub; ordinary stored views still do not recollect providers.

Only the existing four source-refresh workers perform the scan. One temporary diagnostic can be queued or running across the instance. It shares project exclusion with ordinary source refreshes and the existing provider request gates; no additional worker pool is created. Registration, worker admission and final return reauthorize the read and verify the same project repository, configuration revision and operator capability fingerprint. An inactive restored domain or held providers refuses the call. Missing/stopped workers, busy admission, invalid facts and provider interruption return errors without partial rows. Cancellation keeps the permit until the worker actually finishes; shutdown drops queued/interrupted diagnostics rather than replaying them. No diagnostic is automatically retried or scheduled.

The service bounds the entire read to twenty seconds, with a twenty-five-second HTTP/client response allowance. The provider scan retains its fifteen-second/ten-GET limit. Response JSON is at most 8 KiB and contains the oldest aged rows, sorted by update timestamp then run ID, plus scanned, aged and omitted counts. Each retained row preserves run, suite, workflow, run number, attempt, head, event and timestamps. Omission is explicit, including any extra rows dropped to meet the byte bound. Observation completion time, `non_atomic_status_buckets` and `capacity: unknown` preserve the limits above. Nothing in this output is a required-check verdict or authorization to cancel/restart a run, infer dead jobs, alter concurrency or declare recovery.

This is ephemeral diagnostic availability, not a durable operation or recovery workflow. A caller may explicitly make a later new observation, but a lost response grants no automatic retry. The separate exact-attempt read below does not add progress comparison, capacity evidence or guarded recovery.

## Exact selected-run job diagnostic

`POST /api/v1/projects/{project_id}/reviews/actions-run/observe` is an explicit project-read drill-down. Its strict object requires positive `run_id`, expected current `run_attempt` and `limit` (1–20). `fc reviews actions-run --run N --attempt N [--limit N]`, typed `Client.ActionsRun` and MCP `actions_run_read` default the display limit to ten. Clients cannot select a repository, credential or provider capability. A stale expected attempt refuses rather than silently switching to a rerun.

This read shares the liveness diagnostic's single global permit, four existing source workers, same-project source exclusion, twenty-second service budget and current authorization/configuration/repository/restore checks. There is no additional pool, durable operation, recurring request or automatic retry. The provider makes two direct run witnesses around at most eight exact-attempt job pages, bounded to eight hundred jobs and ten GETs in fifteen seconds. It does not rediscover unrelated runs. Changed run/attempt/head/suite/workflow/event identity or status, incomplete pagination, duplicates and malformed facts refuse the entire read; no partial job inventory escapes.

The response is at most 8 KiB and retains full selected run identity, observation completion time, complete bounded pending/running/completed job counts and compact job/check-run identities, names, states and timestamps. Running jobs sort first, then pending jobs, then completed jobs, with increasing job ID in each group. Count/byte omissions are explicit. The `non_atomic_attempt_jobs` marker is deliberate: independent job pages and run witnesses are not an atomic GitHub snapshot. Provider skipped/cancelled synthetic start timestamps may follow completion; they are preserved, not interpreted as execution duration.

A known in-progress job is an observed state, not a process heartbeat or proof that all work is progressing. Equal later observations establish only no observed transition, not a dead process. Empty jobs do not prove runner exhaustion or concurrency-lock ownership. Capacity remains `unknown`; no logs, runner names, CI credit, cancellation, restart, new attempt or recovery authority is supplied. Actual progress comparison, capacity evidence and guarded recovery remain separate incomplete work.

## Hosted review parsing

CodeRabbit decoding recognizes closed generated envelopes and presentation variants while retaining original bodies and identities. Publication attribution, exact comparison, provider run identity, finished acknowledgment and counted finding inventory remain separate checks. Inline, outside-diff, duplicate and additional findings retain their own source references. Additional-only summaries support at most one bare `<br>` as the first visible, nonempty, non-Markdown-quoted line after each `<dd>` inside balanced definition-list wrappers at the same details and HTML blockquote depths, and a standalone heading around exact Review info metadata. Repeated breaks, mid-body breaks and breaks outside the matching definition body remain unsupported; hidden or Markdown-quoted examples cannot supply wrapper boundaries or restart a body. Neither presentation form can hide unknown findings or supply comparison authority by itself. Supported count omission for positive and marked-empty publications follows the [native source contract](review-execution.md#hosted-adapter). Malformed or contradictory count metadata remains refused; omission by itself never establishes a clean review. When that positive marked batch omits comparison metadata, the original pre-POST journal baseline and authenticated standing metadata may prove its exact comparison, fresh run, selected paths and finished acknowledgment; conflicting batch scope cannot be replaced. Proven historical annotation exclusion preserves original comment/review identity, body and positions while allowing GitHub to remap the comment’s current commit and CodeRabbit to remove one exact canonical count heading from an otherwise unchanged historical review. A strictly complete positive Additional/outside-diff summary, including a mixture of those categories, remains eligible after proven old-root annotations are excluded; duplicate-only or empty summaries cannot supply that positive inventory. These projections never rewrite retained captures or grant new completion or fix credit. Quoted prompts and examples cannot supply authoritative metadata. An authenticated foreign Bot report whose entire current body contains no case-insensitive `coderabbit` mention is not an inferred request authority merely because it was edited. This current-source policy does not prove its original body or require a pre-trigger copy; bot-looking prose or login names do not replace the source-owned actor kind. Visible competing review commands still close attribution regardless of author. Edited posting-actor, human and other actor-kind comments, and Bot comments with provider mentions even in quoted or hidden text, retain the conservative edit barrier. Baseline-qualified projections and old finding annotation proof preserve their separate source-binding checks without rewriting either capture.

Generated autofix controls use their paired comment boundaries and a single UUID-bearing unchecked checkbox. Fix prompts use a closed details block with one opaque fence, optionally inside balanced definition-list wrappers, in the recognized location in a submitted review or minor finding. The checkbox caption and prompt heading are display text. These blocks supply no findings or comparison metadata. Attributed split submissions use publication and attempt IDs and batch ordinals. The original review and attached comment IDs bind findings to the batch that reported them. Missing or conflicting batches leave the import incomplete.

A markerless submitted review may instead contain exactly one positive, counted Additional comments category followed directly by the existing `h6`-wrapped Review info and generated status footer. The complete provider collections must prove there are no attached inline roots or in-window orphan roots. The original submitted review, unique fresh run, configured provider actor, exact head, explicit full comparison, counted selected-file inventory and unique later finished acknowledgment retain their ordinary checks. Every additional finding must decode completely and name a selected file; category/group counts, source text and its unverified qualification remain intact. Actionable-count declarations, mixed or unknown categories, empty inventories, competing submissions or commands and ambiguous acknowledgments cannot use this path. This parser support preserves positive findings and does not itself finalize adjudication, change round accounting, or grant clean/taper credit. The existing runtime proof pipeline remains responsible for completion.

A closed unquoted Follow-up Comments envelope may carry positive outside-diff findings without a count header. Its generated badge, explicit matching title, severity, range, reviewed-head source location and fingerprint must agree. The source location establishes finding coordinates only; it never supplies base or effective-parent proof. Empty, ambiguous or unsupported envelopes remain incomplete.

A generated Review info block can report files whose changes were unavailable. Retain that bounded limitation separately from finding-inventory completeness; a complete captured result does not imply every selected file was reviewed. Original findings and completion accounting remain unchanged.

A finished reply alone cannot establish an empty review. Standing zero results require explicit supported inventory and coverage facts; positive submitted reviews require their own attributable submission and complete finding inventory. Recent-review metadata may present its complete details block inside one exact matching `<h6>` wrapper. Its zero claim may instead occupy one closed, visible root recent-review status block containing only that supported claim; duplicate, malformed or contradictory claims remain refused. These presentations do not replace any run, comparison, selected-file, inventory or attribution proof. Allowance parsing attributes a supported reply to its original query and that query's exact operation, attempt and head, preserving rounding uncertainty in cooldown deadlines without granting execution permission.

`SubmittedReview` recognition feeds the implemented native Hosted runtime, engine evidence and backup-retention paths. The runtime can recover a supported submitted review against its original attempt while preserving the real review, provider-run and acknowledgement identities and original source bodies. Parser recognition alone does not grant native completion credit: comparison validation, complete finding extraction, settlement and finalization remain separate mandatory boundaries. The [execution contract](review-execution.md) owns the consuming behavior and qualification boundary.

## Validation boundary

Focused validation scenarios include bounded HTTP and conditional revalidation, unique parent lookup, explicit history pages, required-check rule decoding and malformed or conflicting Hosted inventories. Qualification results belong to the slice's validation record; this document makes no installed-provider, native-recovery or operational-cutover claim.

### Isolated metadata-pair experiment

The default-off `LifecycleSyncOptions.BatchMetadata` experiment pairs two already-due,
non-hinted metadata observations under the existing project/account reservation. It
adds no provider concurrency and leaves one-minute metadata scheduling, CI collection,
watch admission, and required-CI authority unchanged. A bounded GraphQL query returns
both complete PR metadata records through the existing normalization path. This is
not an atomic snapshot. The normal per-target configuration/comparison guards still
control publication; late hints remain outstanding.

The transport shares the credential's read gate, GraphQL quota floor, HTTP client,
and request budget. It performs one request with a 64-KiB response bound, fixed aliases,
strict integer identities, and no REST fallback. Shared or member errors discard the
unpublished pair conservatively. Invalid inventory records ordinary failed-observation
retry cadence without source success; account failures preserve per-target state and
use normal account backoff. Later hints remain outstanding. Cancellation releases
reservations. Malformed data cannot become a successful source observation. The explicit
`--lifecycle-metadata-batching` daemon flag defaults false. No production activation or
daemon-credential qualification is implied.

The controlled two-target path uses one physical request instead of two source reads.
Real PC-credential differential samples matched all 21 selected normalized fields,
including one stale cached PR base that differed from the live branch tip. Those samples
are not daemon-credential, moving-source, or universal permissions qualification.
GraphQL cost was one point for the sampled pair; conditional REST 304 reads can cost
zero core points, so reduced HTTP volume does not imply reduced charged quota.

### Natural metadata compatibility qualification

`--lifecycle-metadata-qualification` is a default-off diagnostic, mutually exclusive
with metadata batching. It samples at most one naturally due two-PR cohort per
account per process, retaining the existing project/account reservation. Event
hints and existing CI subscribers do not initiate the sample. The actual bound
provider credential, read admission and shared quota apply. The sample has a
15-second aggregate deadline and at most five transport requests: two individual
REST observations, one paired GraphQL query, then two individual REST observations.
All normalized pull and repository fields must agree; movement or disagreement is
inconclusive. The log contains only a process-local account bucket, state and
transport count, not repository identity or provider payloads.

Diagnostic observations never publish source facts or grant CI freshness. After a
successful or inconclusive comparison, both PRs still take ordinary fresh
individual observation paths, including their binding and hint checks. Provider
errors retain ordinary account cooldown and stop the cohort. A later cohort is
ordinary, not another diagnostic attempt. This mode temporarily adds requests; it
is compatibility evidence, not a quota-saving feature or permission to activate
batching. Live results and deployment qualification remain separate.
