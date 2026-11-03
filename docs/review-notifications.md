# PR Inbox Notifications

## Following and delivery

Normal explicit PR enrollment, review requests, finding decisions, finalization and fix recording automatically follow that PR for the actual bound acting worker. Automatic Hosted scheduling does not follow every PR for its project authorizer. Imports, passive observation, reads and maintenance do not infer participants or create follows. Existing recorded manual-request workers seed initial preferences once; principal identities never substitute for missing worker identity.

Each worker has one project/PR preference: automatic following, explicit following or muted. Explicit unfollow remains muted after later work; deliberate follow enables notifications again. Following is notification preference, not PR ownership, work assignment, authorization or a review/CI/merge instruction. It does not enqueue a review or add an untracked PR to observation. Queue enrollment is sufficient to follow before asynchronous provider metadata arrives. Observation remains limited to the existing queue; enroll an intended missing PR through the ordinary queue operation separately. Existing preferences remain readable and can be muted after a PR leaves that scope.

A verified provider observation changing retained state from open to merged or closed creates one durable Controller inbox message for each following, non-retired worker. The plain message identifies the PR number, title, terminal state and GitHub URL. The new observation, message, recipient receipt and event commit atomically. Repeated observations and exact retries do not duplicate messages; a later observed reopening followed by closure is a new transition. Initial unknown/already-terminal observations and historical imports establish a baseline without replaying old closures. No future messages are delivered to retired workers.

Controller messages use the ordinary inbox pending/unread discovery, selected-message reads and explicit seen/acknowledgment. They are one-way notification roots and have no worker reply destination. Receipt changes do not change PR state, grant authority or mark review work complete. The [incoming feed](message-events.md) owns delivery and wait semantics; host wakeup remains a separate capability.

## Review results ready

When a native CLI or Hosted attempt completes with validated complete evidence, the completion transaction creates one durable Controller inbox notice for each following, non-retired worker. An explicit request already follows its actual requesting worker unless muted; automatic requests use existing PR participants, not their project scheduling authorizer as an inferred owner. The short notice identifies the original attempt’s provider and channel, PR, validated GitHub PR link, immutable evidence and original operation. Provider and repository identity come from the retained attempt and admitted comparison, so later project renames or capability changes do not redirect the notice. A ready result identifies that provider’s evidence; the separate [readiness assessment](review-readiness.md) owns complete observed GitHub thread state across providers. It does not copy mutable PR titles, findings or adjudicated counts, and does not grant permission to start further work.

The attempt transition, completion bookkeeping, notices and inbox events commit together. An immutable evidence-scoped key provides per-recipient deduplication; completion recovery after restart uses the original evidence and cannot duplicate a delivered notice. A message failure rolls back settlement so the same original completion can retry. Raw captures, provider trigger acknowledgments, queued requests, cooldowns, cancellations, incomplete results and other failure diagnostics do not produce a ready notice. Historical imports and enabling the native projection do not scan or notify old completions. Existing notice text remains immutable across upgrades, restart and exact replay; old notices are not rewritten or republished. Standalone execution journals without the native workspace remain independent of inbox delivery. No new polling, subscription or schema is required. Durable inbox availability does not guarantee that a host wakes an idle agent.

## Capture needs attention

A new native Hosted transition to `uncertain` with reason `hosted_finished_inventory_unproven` creates a durable inbox notice through the same participant, mute and retirement rules as ready results. The provider has finished, but the complete findings inventory cannot yet be verified. The notice identifies the original provider, admitted GitHub PR link and operation; it does not claim completion, supply findings/counts, free the reservation or grant another review.

The capture-block transition and notice commit atomically. Original operation and reason identify the notice, so repeated observer passes, recovery reentry and restart cannot repeat it. A message failure rolls back the transition for normal original-action recovery. Ordinary waiting, cooldown, transient observation errors, CLI uncertainty and other reasons remain quiet. Deployment does not scan or notify historical blocks. If later recovery proves a complete result, its normal evidence-scoped ready notice still follows; the earlier attention message remains an immutable record of that earlier event.

## Optional preference controls

No subscription step is needed for ordinary explicit work. For deliberate changes, read the preference revision first:

```sh
fc reviews following 42
fc reviews unfollow 42 --expect-revision 1 --key mute-pr-42
fc reviews follow 42 --expect-revision 2 --key follow-pr-42
```

`GET /api/v1/projects/{project_id}/reviews/pulls/{pull_number}/following` requires read authority and a bound `X-FC-Worker`. It returns `project_id`, `pull_number`, `worker_id`, `mode`, `revision` and `tracked`. Absent preferences use mode `none` and revision zero; stored modes are `auto`, `follow` and `muted`. `tracked` reports current enrolled scope, independently of the preference.

`PUT` on the same route requires write authority, an explicit idempotency key and `{expected_revision,following}`. It changes only the bound worker; `following:false` can mute future participation in an untracked PR, while `following:true` requires current observation enrollment. Revision conflicts do not overwrite another edit. Retain the exact key, arguments and binding after an unknown result. MCP exposes `review_following_read` and `review_following_update` with the same semantics and no per-call worker/project selector.

## Storage and proof

Current preferences use the separate `review_notifications` schema domain, without changing the immutable review-history version or rerunning its backfill. Backup inspection validates preference ownership, modes, revisions and timestamps; restore retains preferences. The source-status publication and inbox publisher own transition and per-recipient replay deduplication in the same transaction.

Focused tests cover verified transitions, unchanged/stale observations, initial terminal baselines, reopening, concurrent publication, muted/retired/uninvolved workers, atomic failure/retry, acting-worker following, automatic-authorizer exclusion, one-time manual seeding, revision/key/worker isolation and CLI/MCP controls. These local tests do not establish installed rollout, real GitHub delivery latency or idle-chat wakeup.

Completion-notice tests exercise CLI and Hosted transitions, original provider/repository attribution despite current metadata changes, safe GitHub link construction, old-format notice preservation, retained CLI recovery across restart, exact replay, message-failure rollback, original-requester following, automatic participant routing, explicit mute/retirement, paginated same-project delivery, historical-import silence and standalone journals. They use authenticated local request paths and synthetic captures, without provider calls or installed-host qualification.
