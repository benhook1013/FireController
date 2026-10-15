# Required CI Observations

## Implementation status

The provider collector, pure assessment, durable service storage and bound API/CLI/MCP controls are implemented. Focused provider and local integration tests pass. Full-repository validation and packaged-service proof are in progress; operational cutover remains separate.

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

`GET /api/v1/projects/{project_id}/reviews/source/{pull_number}/ci`, `fc reviews ci-assess --pr N` and `review_source_ci_assess` require current project read permission and perform no provider work. The local CI assessment reports the refresh identity, selected source, CI commit, observation time, state and compact counts. It checks applicability against current local repository, source configuration and capability binding and the latest stored source comparison. A changed head or base cannot inherit earlier CI success. Full typed inventories are private retained evidence with a bounded storage budget, separate from the ordinary response.

States distinguish satisfied, failed, pending, unproven and not required. Review-channel coverage remains a separate [policy assessment](review-source.md#local-assessment). Neither assessment authorizes a merge or constitutes a combined merge-readiness verdict.
