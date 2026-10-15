# Observed Review Readiness

## Implementation status

This slice is in progress. Source, CI, review coverage and finding routes already have separate implemented boundaries. A combined verdict must also retain complete thread-resolution and provider merge-state facts; it must never infer them from flat review comments or a cached browser label.

## Explicit work and compact reads

Readiness uses deliberately requested provider observations. Ordinary reads remain local and return one compact selected-PR result, with source, CI and thread observation times and the local policy evaluation time. A result is an as-of assessment, never a merge action or a promise that GitHub remains unchanged. Quick collection returns its completed result; work still pending after the caller’s bounded wait returns the durable operation identity. No polling or provider fanout is added to unrelated coordination commands.

## Provider review state

The provider collector selects an exact stable repository ID/name, PR number, head, base SHA and base branch. It reads GitHub’s paginated review-thread connection with only stable thread ID, resolved state and outdated state. All unresolved current and outdated threads are counted separately; an outdated thread is not silently resolved. Full comment bodies are unnecessary for this assessment.

The same observation retains draft/PR state, mergeability, detailed merge state and nullable review decision. These are provider facts, not a local readiness verdict. Missing or unknown mergeability cannot establish readiness. Required CI and exact test-merge identity retain their separate authority; a provider merge-state label cannot replace them.

Every page must bind the selected repository and comparison. Collection rejects GraphQL errors or partial/null data, duplicate thread IDs, invalid or repeated cursors, contradictory counts, changed comparison or incomplete pagination. Calls share finite time, page, item and byte budgets and never retry automatically. A second complete bounded pass must agree before a successful inventory is retained; movement remains an explicit unproven outcome rather than an empty passing set. The two observations do not create an atomic provider snapshot.

The collector performs only GraphQL queries, with no mutations, comments, allowance requests, CLI review processes or repository discovery. Raw provider diagnostics, credentials and complete inventories stay out of routine tool responses. [GitHub’s pull-request schema](https://docs.github.com/en/graphql/reference/pulls#pullrequestreviewthread) owns the thread fields; local code owns strict supported bounds and failure handling.

## Composition boundary

A combined assessment must evaluate current local policy, applicable finalized native review coverage, accepted and routed obligations, complete retained thread state, required CI, provider merge state and explicitly applicable holds in one authorized local snapshot. Imported browser history grants no native review credit. Pausing review execution does not erase previously valid credit. Missing or contradictory inputs remain holds; an unconfigured review policy cannot pass vacuously. Humans perform merges.

Exact request fields, public combined response and executable proof will be documented with their implementation. The provider collector alone does not implement this composition or establish operational cutover.

## Service and bound tools

Source refresh adds optional `include_readiness` (default false). It requests CI and provider review-state observation, independently of the existing `include_ci` option; either option requests CI, while only readiness requests threads and merge gates. Both booleans participate in action identity and in-flight coalescing. The operation retains its exact requested options; a completed readiness operation includes compact CI and `review_state` summaries, with full inventories retained privately. Source-only and CI-only defaults keep their existing cost. Readiness collection shares a forty-five-second aggregate provider budget, with the source phase bounded to fifteen seconds. A failed extra collection preserves useful source facts with an explicit unproven component. Existing wait/cancellation/restart rules apply.

`GET /api/v1/projects/{project_id}/reviews/source/{pull_number}/readiness` evaluates the latest completed readiness observation against the latest completed source comparison and current local configuration, repository, capability and policy, all in one authorized database snapshot. Missing readiness observation returns `reviews_readiness_unavailable`, with an instruction to explicitly collect it. The read performs no provider requests or writes.

Its compact `SourceReadinessAssessment` identifies `project_id`, `refresh_id`, `source:"observed_comparison"`, `configuration_revision`, `policy_revision`, `source_observed_at`, `evaluated_at`, `target`, `state`, `reasons`, `channels`, `ci` and `review_state`, plus the existing optional incoming-route count/capacity fields. `state` is `ready_as_observed`, `held` or `unconfigured`; the reason list contains stable codes rather than provider diagnostics. The CI object uses the existing compact summary. The review-state summary contains `state`, `reason`, `observed_at`, `pull_state`, `draft`, `mergeable`, `merge_state_status`, nullable `review_decision`, `total_threads`, `unresolved_current` and `unresolved_outdated`. Inventory IDs and comment bodies are excluded.

`ready_as_observed` requires current applicable source/configuration binding; an explicit configured review policy; every required channel covered without pending native obligations; no pending/capacity-held incoming findings; CI satisfied or explicitly not required with a verified candidate merge identity; and a complete provider review-state observation for an open, non-draft PR with `MERGEABLE`, `CLEAN`, no `CHANGES_REQUESTED` or `REVIEW_REQUIRED` decision, and zero unresolved current or outdated threads. Other merge states, including external hooks, cannot establish this initial ready state. Unknown facts hold. A conflicting, pending or unproven CI candidate cannot be silently replaced by a clean merge-state observation; it remains `ci_merge_state_unproven` until a coherent refresh. Null review decision is a provider fact and does not bypass the separately required clean merge state and explicit local review policy. Execution pauses retain their existing admission-only meaning.

The CLI adds `--readiness` to explicit `reviews source-refresh` and a provider-free `reviews readiness --pr N`. MCP adds optional `include_readiness` to `review_source_refresh` and the read tool `review_source_readiness`. Bound profiles, explicit keys and optional short/immediate/longer waits retain their existing mechanics. No new background polling, generated retry keys, automatic merge or per-command project/worker flags are introduced.
