# Recover stalled review work

Use the existing project-bound connection and the authority already granted for this review. A visible control is not permission to spend allowance, add rounds, change a pause or force a different target. Read matching-release help and preserve the exact operation, selection, head, configuration and idempotency key after an uncertain outcome.

## Establish which state needs recovery

Read `reviews operations --pr PR --channel CHANNEL --status pending` and current `reviews rounds read`, `reviews execution` and `reviews scheduling --pr PR`. Read the original operation and any selection handle: an empty operations list alone does not exclude a queued or selecting request. Verify the current open, non-draft GitHub head before authorizing a new exact-target request. Current source policy and readiness distinguish unfinished rounds from findings, routes, source, CI or publication obligations; they are not interchangeable gates.

- Queued or timer-backed work keeps its original identity. Wait on that operation. A future retry deadline is only the earliest eligibility check, not a promised start. Resume an explicitly paused queued operation only when authorized; do not replace a cooldown to avoid waiting.
- Posting, active or uncertain work may already have spent allowance. Use supported original-operation observation/recovery, not a new request or direct provider comment. After an uncertain tool response, recover the same key and arguments first.
- A terminal failed or held operation does not resume by replaying its ordinary request key. A decoder repair can help new or still-recoverable observations; an already released `hosted_finished_incomplete` attempt needs the separate supported recovery action below. Preserve the original record and captures rather than manufacture completion or taper credit.

## Recover the original result before spending another round

When asked to recover an original result, inspect that attempt's latest indexed immutable post-trigger capture, its qualifying pre-trigger baseline and original comparison before considering a fresh review. Mutable GitHub content or a newly completed replacement cannot stand in for the original evidence. A still-recoverable `hosted_finished_inventory_unproven` attempt follows ordinary original-operation recovery; a released terminal failure does not.

For a released `failed` / `hosted_finished_incomplete` attempt, confirm the installed daemon supports [explicit recovery of a released Hosted result](../../../docs/review-execution.md#explicit-recovery-of-a-released-hosted-result), including its original configured Hosted capability and trusted retained-source decoder. Source publication or a decoder fix alone does not prove that this route is installed. The matching-release contract owns the administrator-only `POST /api/v1/projects/{project_id}/reviews/attempts/{attempt_id}/retained-recovery` request and exact original identity, revision and capture assertions. Use that supported route with the required authority; do not invent a worker command or change terminal state directly.

This path requires the audited release and latest retained source to prove a complete original result. It refuses pending replacement work or reservations, incompatible original round bindings, exhausted capacity and duplicate credit, including completed replacements awaiting finalization. A projected installation also requires current stored PR metadata for a fresh repair. It performs no provider request, new observation, allowance query, grant change or reissue. Missing or unsupported evidence is a blocker, not permission to pick an older capture, synthesize findings or bypass the checks. If this route is unavailable or refuses the original evidence, report that boundary rather than silently substitute a new round.

Successful recovery atomically retains original evidence, native archive linkage, completion and the recovery audit. It does not finalize findings or immediately award round credit. Inspect and adjudicate the recovered evidence, then use ordinary finalization; a proven zero-finding result can be finalized directly once against its original round binding. Preserve the exact recovery key and assertions after an uncertain response and replay those, not a fresh key. General failed-state transitions remain unchanged.

## Make a fresh ordinary request when appropriate

A terminal attempt and a new authorized review are separate actions. A fresh request does not repair the original result. If a new review is authorized, the required fix is live, no original request or selection is pending, the current target and configuration are verified, and the existing grant has room, use one ordinary `reviews request PR --expect-config N --channel hosted --head SHA --key NEW_KEY` or `review_request`. For normal channel-front work, use `reviews start --channel hosted --expect-pr PR --key NEW_KEY` / `review_start`; the expected PR is an assertion, not a forced selection. Existing automatic scheduling does not guarantee that a failed semantic intent will be recreated, so check the supported explicit path rather than waiting indefinitely for an unchanged terminal record.

Keep the resulting operation or busy handle and follow that same work. The server still applies source, findings, capacity, round, provider and fresh allowance checks. A request may queue or retain a cooldown; neither means a provider review has started. Do not add a grant merely because one slot is reserved by existing work. Do not request another review when recovery can safely collect the original pending result.

## Diagnose missing allowance replies before retrying

A terminal `hosted_allowance_reply_pending` is a preparation hold, not an issued provider review. It proves that retained collection found no attributable reply during its bounded checks and no review trigger was posted; it does not prove that no reply existed on GitHub or that a reply was late.

If GitHub shows an attributable reply timestamped before capture but absent from that capture, check query-specific pagination and collection completeness. A source count ending exactly at a page boundary such as 100 is a useful diagnostic, especially when the next comment is the missing reply. Compare original query/reply identities, timestamps and retained collection counts rather than classifying the reply as late or repeating fresh requests. This discrepancy alone does not prove a cache cause or justify a cache workaround, parser change or provider reissue.

After accounting for the collection failure or actual pending reply, one later fresh-key request for the still-authorized planned review can obtain a fresh allowance decision with current configuration/head and normal admission checks. Respect known cooldowns and avoid rapid retry loops; a later reply does not resume the old hold or grant fresh permission. An old available reply cannot substitute for fresh preflight. No manual provider repair or extra round grant is required merely because that query held.

## Use only a matching, explicitly authorized override

`--out-of-order` / `allow_out_of_order` bypasses predecessor progression for the exact requested target. It does not bypass earlier queued priority, the target's own findings, source validation, rounds, provider restrictions or allowance. `--override-file-limit` / `override_file_limit` bypasses only the optional project file ceiling. Read the actual file count and ceiling first; do not use it for a target already under the limit. Normal channel-front starts and automatic selection do not inherit either choice.

These flags do not repair a parser, revive a terminal attempt or award missing review credit. Do not guess an override from the word “blocked.” Identify the exact refusal and use a flag only when it addresses that refusal and the user's scope permits it. Otherwise retain the blocker and seek the required decision instead of weakening unrelated checks.

Administrator attempt settlement is another distinct action: it records verified external termination or supported local abandonment and releases eligible capacity. It does not cancel the provider, retry the review or mark it complete. Publication recovery retries an original checkpoint publication, not the provider review. Follow their matching-release contracts before either action.
