# Required CI Observations

## Implementation status

The provider collector, pure assessment, durable service storage and bound API/CLI/MCP controls are implemented. Focused provider and local integration tests, full repository checks and controlled packaged-service proof pass. Operational cutover remains separate.

## Explicit collection

Source refresh accepts optional `include_ci: true`; the bound CLI exposes `--ci` and MCP exposes `include_ci`. Omitting it keeps source-only collection. CI collection uses the same durable operation, two-second default result wait and optional longer wait described in [source refresh](review-source.md). Adding CI changes the action identity: source-only work cannot silently fulfill a CI request. Ordinary operation and assessment reads never contact GitHub.

Source-only collection retains its fifteen-second provider budget. A refresh including CI has one thirty-second aggregate budget, with at most fifteen seconds for its initial source phase. Provider work runs outside database transactions. Cancellation of the caller's result wait does not cancel the accepted operation. Restart may repeat interrupted provider reads, using the same operation identity; collection never posts a review or reserves provider review allowance.

Successful source collection remains useful when CI cannot be established. The operation retains its source comparison and an explicit unproven CI outcome. Provider failure, incomplete pagination, unsupported identity or exceeded capacity never becomes an empty passing result. Raw provider errors and full inventories stay out of ordinary agent responses.

## Authority and candidate

Required checks come from the selected base branch's complete applicable branch protection and active repository rules. Exact context and expected GitHub App identity matter. An unavailable or ambiguous authority remains unproven. An explicitly complete empty requirement set means `not_required`, without fetching check runs or workflow jobs.

The collector binds the repository's stable identity, PR, head, base SHA and base branch. It prefers CI on a verified test merge when that commit has check runs or commit statuses. A completely observed empty merge result may fall back to the head. Failed or incomplete merge observation cannot justify that fallback. A conflicting merge can have head CI facts, but their outcome does not imply that the PR is mergeable.

Before crediting a passing result, collection rechecks the source candidate, requirement authority and selected CI identities. Movement leaves the observation unproven. These checks produce attributable facts as of their observation interval, not an atomic lock on GitHub or a guarantee that later provider state is unchanged.

## Current workflow identity

For GitHub Actions, workflow identity and provider run number select the newest run for the selected commit. A newer pending or failed run prevents an older successful run from supplying credit. Event type, timestamps and numerical check-run IDs do not establish currentness by themselves.

Required check runs must bind positively to the selected workflow run, its current numbered attempt and the actual job's check-run identity. Partial reruns do not inherit earlier successful jobs without that proof. Unsupported provider currentness, missing job identity and ambiguous duplicate contexts remain unproven. Commit statuses retain the provider's newest-first ordering; matching status and check contexts must both satisfy their applicable requirements.

Collection is finite: at most 128 required contexts and sixteen relevant workflows, in addition to each provider reader's page, item and response-size bounds. Optional workflows do not cause job-fetch fanout. Exceeding a supported boundary is explicit rather than truncating evidence.

## Stored assessment

`GET /api/v1/projects/{project_id}/reviews/source/{pull_number}/ci`, `fc reviews ci-assess --pr N` and `review_source_ci_assess` require current project read permission and perform no provider work. The local CI assessment reports the refresh identity, selected source, CI commit, observation time, state and compact counts. It checks applicability against current local repository, source configuration and capability binding and the latest stored source comparison. A changed head or base cannot inherit earlier CI success. Full typed inventories are private retained evidence with a bounded storage budget, separate from the ordinary response. Source-policy assessment and latest-comparison checks read source metadata without selecting or decoding that CI inventory. Invalid unrelated CI evidence cannot block source-policy assessment; explicit CI assessment and complete operation reads still validate their selected retained CI strictly and fail closed on corruption.

States distinguish satisfied, failed, pending, unproven and not required. Review-channel coverage remains a separate [policy assessment](review-source.md#local-assessment). Neither assessment authorizes a merge or constitutes a combined merge-readiness verdict.

## Executed proof

The service preserves an explicit `ci_required_context_capacity` outcome when the collector exceeds 128 required contexts. It retains a compact unproven result and discards the oversized inventory; no passing CI credit is created. Real-engine and public HTTP regressions cover the 129-context refusal, a satisfied 128-context control, retained local reads and malformed or foreign collection guards. Focused normal/race tests and vet pass.

At clean runtime checkpoint `e70be2c901b6c60e4d4be00861c752b362af2fb3`, `make check build` passed: 121 browser tests, all Go race tests and vet, documentation checks, embedded assets and three binaries. An independent integration pass found and corrected a false pass where a successful commit status could hide a newer workflow missing its current check. Public-handler and official-SDK tests also caught the HTTP boolean-decoding gap before publication.

Six actual packaged CLI/daemon/MCP groups passed in 1.833 seconds with a controlled HTTPS GitHub fixture. The default CI refresh returned completed facts in 70.365 ms and 976 bytes; immediate return used an 87-byte receipt in 7.538 ms, and a same-key longer wait returned the original operation. Jobs and inbox reads stayed near five milliseconds while the provider was blocked. These are small local fixture measurements, not network or fleet latency guarantees.

The real GitHub adapter verified required context/App identity, the test-merge candidate, current workflow and numbered attempt/job identity. A newer pending run prevented older success credit. A partial rerun lacking the current job stayed unproven. A CI-provider refusal retained source facts with an unproven CI outcome. Actual official-SDK calls, local assessment, exact replay, completed-state restart and source-only defaults passed. Local reads and terminal replay left logical SQLite contents and provider counts unchanged; original native evidence and imported merged history were preserved.

All three binaries identified the exact clean checkpoint. Source/assets manifest SHA-256 `7ce02770ecea867051a44e71190b84b2e972af269d946247ffb510654537abf7` and binary hashes stayed unchanged; the tested `fcd` hash was `3d3df1bb6ccc697eac1730ae680a26bce8f29e01bc64c0d64ca7285bfefc1156`. The final run made 145 controlled provider GETs, zero provider POSTs, zero Git or review-process invocations and zero real external calls. Owned processes, listeners and runtime state were removed.

This proves the local executable path with controlled provider responses, not live account behavior, provider availability, browser/phone rendering, remote deployment, fleet throughput or cutover. CI-specific restart during an interrupted fetch was not repeated in this packaged run; focused real-SQLite recovery tests and the earlier source-refresh packaged proof cover that mechanism. No source finalization, merge readiness or provider review credit was inferred from CI success.

## Retained timestamps and clock correction

Fresh CI ingestion checks the bounded collection interval and current-time upper bound before adopting a provider result. Reading retained CI validates each nonzero timestamp and immutable source/target/inventory bindings independently; it does not compare historical observations with the host's current clock or a later evaluation timestamp. A backward clock correction cannot turn otherwise valid stored evidence into unavailable storage. Observation age remains informational, and timestamps do not replace current workflow or source identity checks.

Regressions reproduced retained future-dated source/CI reads and typed assessment rejection, then passed without changing stored timestamps. Fresh future observations and malformed or mismatched retained facts remain rejected. Focused CI/routes normal and race tests plus vet passed; no host-wide clock change or live provider behavior is claimed.
