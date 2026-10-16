# Review Orchestration

## Implementation status

The packaged daemon supports optional native CLI and hosted review execution, with journaled admission, retained captures and restart recovery described in the [execution boundary](review-execution.md). An explicit [captured-comparison assessment](review-controls.md#captured-comparison-assessment) applies current local policy to retained native evidence. Fresh-source aggregate assessment, required-CI assessment and merge readiness remain unimplemented. The [native review workspace](review-workspace.md) implements provider-free stored observations and browser inspection; those historical records do not establish native admission or readiness. Existing source code and tests establish the inherited behaviors catalogued in [source evidence](inherited-evidence.md). This document defines the intended portable boundaries; provider-specific limits and project policy are configuration rather than universal constants.

## Review identity

A review target binds project, provider repository, PR, exact candidate head, selected base or effective parent, relevant parent head, merge base, and canonical patch identity. Evidence may count only when its capture and publication can be attributed to that target and the applicable reviewed scope. Unknown ancestry or identity is a hold, not permission to fall back to a different PR.

The project may choose independent PRs or an ordered stack. In stacked mode, the nearest preceding unmerged PR is the effective parent; the first unmerged PR uses the configured repository default base. Parent movement requires re-evaluation of affected descendants. An explicit expected PR asserts selection; it never forces a different target past admission policy.

Use deterministic Git diff options to calculate patch identity. History-only change, identical patch, candidate movement, parent movement, and actual scope change remain distinct classifications. Reuse of evidence across any boundary requires an explicit proven applicability rule rather than comparing counts or choosing the latest-looking result.

## Observations versus authorization

Read-only review pages and status commands show stored evidence, operation state, and freshness without provider calls. Explicit refresh retrieves only the selected target and the dependency facts required by the requested scope. The [architecture](architecture.md) owns observation storage, completeness, coalescing, and quota scheduling.

Starting a review requires sufficiently current PR identity and lifecycle, exact selected anchor, relevant active-provider activity, admission policy, and applicable account quota facts. The action records which observations and revisions supported that decision. Freshness is a predicate over required facts, not one whole-repository cache TTL.

Recheck exact identity at the last provider-action boundary. GitHub offers no transaction with the local service: the head can still move after that check. Capture the admitted identity and classify later results against it; a moved head cannot turn into evidence for the new head silently.

Review completion establishes attributable coverage and finding disposition; it does not itself establish mergeability. A separate merge-readiness assessment combines applicable review evidence, required CI, unresolved current and outdated threads, accepted finding obligations, applicable stops, and current mergeability. Unknown or conflicting mergeability cannot produce a ready verdict. A cached dashboard can show its last result but cannot present it as a fresh verified merge gate. Humans perform merges.

Required-check authority comes from the provider's applicable branch/repository rules, supplemented by stricter project expectations. A project cannot silently weaken provider-required checks. Match results to the exact candidate, required context, expected provider-app identity, and applicable latest run/attempt; a same-named foreign check or obsolete successful retry cannot satisfy the requirement. Missing authority, incomplete inventory, and ambiguous check identity remain holds rather than an empty passing gate.

## Admission and execution

The initial [project contract](architecture.md#projects-and-actors) rejects duplicate repository bindings within an instance. External-action admission additionally creates a durable project-owned attempt and reserves the target by provider, canonical repository identity, PR, and action class. The admitted head and operation identity are reservation data, not a way to bypass an active reservation by changing heads or configuration. A conflict does not expose another principal's private attempt details. Different action classes may overlap only when their adapter's admission contract explicitly permits it.

Provider-account scheduling coordinates shared quota; it does not replace duplicate-action exclusion. Reservations survive uncertain outcomes and are released only by a proven safe terminal/recovery transition. The explicit [capacity settlement](hosted-settlement.md) records a trusted administrator’s verification that the original external action stopped; a limit, timeout or local cancellation alone cannot establish that fact. Updating reservations and state uses database transactions; waiting for a provider happens outside them. This guards clients of one supported service instance, not arbitrary independent installations or manual provider actors; current provider activity must also be checked where the action requires it.

Model attempts as prepared, posting/running, active, completed, failed, cancelled, or uncertain with retained transition history. The provider adapter records exact request and response identifiers. A disconnect or crash after a possible post leaves an uncertain attempt for observation-based recovery, not an automatic retry that might post twice.

Hosted review and local CLI execution are separate adapters with independent review cycles and quota observations. A cooldown, exhausted allowance, or active attempt in one channel does not by itself prevent admission in the other. They may review the same immutable target concurrently when each adapter satisfies its own admission checks and neither requires an exclusive shared resource. Candidate or effective-parent movement invalidates applicability in either channel; separate cycles do not relax target identity or publication safety.

For an ordered stack, select the earliest unfinished eligible target independently within each channel. A channel may advance after its configured sufficiency or stopping policy is met without waiting for the other channel to catch up; an upstream movement then requires reassessing affected evidence. A genuine shared provider-account quota or exclusive executor resource constrains only the actions it covers. Record provider responses and next availability rather than infer one channel's limits from the other.

A local executor receives an explicit project, allowed workspace, immutable target, operation ID, and policy. The server never adopts a client's current working directory as a global setting. The initial executor runs on the server machine; remote executors are deferred until authentication, leasing, and artifact-transfer contracts are proven.

A project administrator configures the server's repository source and local executor capability. The service obtains and verifies the candidate/base objects from that source, then creates a service-owned operation workspace containing the admitted target tree and comparison objects. Its lifetime is independent of the developer's mutable checkout; neither another client nor a later branch switch can change what the executor reads. A remote client's filesystem path cannot substitute for server source provisioning. Missing source or executor capability is reported explicitly without blocking coordination or hosted reviews.

Local execution is an operator-trusted capability, not a hostile multi-customer sandbox. The executor receives an explicit argument/environment allowlist and only its required credentials, rather than the service's full environment or other projects' credentials. Source preparation does not run repository build/install hooks implicitly. Retained evidence identifies the verified workspace tree and admitted comparison even if the provider head changes during execution.

Cancellation requests distinguish queued work, safe-to-cancel execution, and already-posted provider work. Terminating a process does not erase a reservation, recover quota, or establish that a provider action never happened. Timeouts preserve diagnostic evidence and return an operation state that explains whether recovery is needed.

## Evidence and findings

Persist original captures privately with checksums and durable provider/run identities. Completed, attributable, scope-applicable reviews are eligible for policy credit. Failed, incomplete, provisional, ambiguous, and rate-limited attempts remain inspectable but cannot masquerade as completed review coverage.

Finding decisions distinguish accepted, routed, and rejected, with reasons and source identities. Finalized source counts are immutable. A routed finding has independent receiving-project/PR disposition only when an explicit authorized route exists; the first release restricts routes to the same project. Receiving decisions cannot rewrite source review counts.

Accepted findings require recorded fix evidence tied to the exact run and finding. Corrections append an audited change guarded by the expected current value. Correcting a transcription or adjudication preserves prior records and does not manufacture review credit. Reusing a finding count with replaced finding text must be rejected as conflicting evidence.

Private source finalization and any public checkpoint publication are separate operations. Record a publication intent with exact semantic identity before posting. Recover uncertain publication through provider history and the intent; repeated requests do not create duplicate checkpoints. A checkpoint parser may accept neutral formatting edits only when semantic identity, attribution, counts, and linkage remain provable.

## Project policy

The first native review delivery supports explicit provider/channel selection, provider ceilings, operator stops, queue mode, required CI, and accepted-finding obligations. Store policy changes with revisions and actor attribution. A policy change does not rewrite historical admitted attempts or prior decisions.

Initial aggregate review sufficiency requires an explicit nonempty set of required review channels. Each selected channel needs at least one completed, attributable, complete capture applicable to the current target and comparison, with every finding adjudicated and its accepted/routed obligations satisfied. Enabling an executor does not automatically make its channel required. Missing requirements produce a policy hold, not vacuous success. A project using coordination without automated reviews remains usable, with review sufficiency shown as unconfigured. Per-attempt completion, aggregate review sufficiency, and merge readiness are distinct states; this initial rule does not implement legacy taper or round budgets.

Legacy minimum/maximum rounds, useful-finding taper, scoped allocations, review-credit periods, and cross-channel policy interactions are retained in the source inventory as an optional later policy layer. They are not prerequisites for delivering reliable native review execution. If enabled later, an accepted useful correction resets its applicable dry streak, while optional style, invalid findings, and already-fixed duplication do not. Exhausted budget and human stop remain distinct from taper completion, and neither clears accepted findings or required CI.

Provider limits are observed/configured constraints, not hardcoded product truths. Refuse an oversized review before consuming provider work where the limit is known. Unknown CLI ceilings remain unknown.

## Inspection and diagnostics

Every provider-bearing operation reports its selected PR and anchor, refresh scope, provider-call count, time spent selecting versus waiting, and retained operation ID. Expose cache hit/coalescing and completeness diagnostics without credentials or private capture contents in ordinary logs. This evidence makes unexpected whole-queue work detectable.

The original proof inventory is a regression source, not an instruction to reproduce historical JSON/SQLite bridges, path names, public marker format, or historical repair commands. Migration from those records is a separately reviewed adapter after the new engine's native behavior is proven.
