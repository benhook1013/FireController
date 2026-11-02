# PR Inbox Notifications

## Following and delivery

Normal explicit PR enrollment, review requests, finding decisions, finalization and fix recording automatically follow that PR for the actual bound acting worker. Automatic Hosted scheduling does not follow every PR for its project authorizer. Imports, passive observation, reads and maintenance do not infer participants or create follows. Existing recorded manual-request workers seed initial preferences once; principal identities never substitute for missing worker identity.

Each worker has one project/PR preference: automatic following, explicit following or muted. Explicit unfollow remains muted after later work; deliberate follow enables notifications again. Following is notification preference, not PR ownership, work assignment, authorization or a review/CI/merge instruction. It does not enqueue a review or add an untracked PR to observation. Queue enrollment is sufficient to follow before asynchronous provider metadata arrives. Observation remains limited to the existing queue; enroll an intended missing PR through the ordinary queue operation separately. Existing preferences remain readable and can be muted after a PR leaves that scope.

A verified provider observation changing retained state from open to merged or closed creates one durable Controller inbox message for each following, non-retired worker. The plain message identifies the PR number, title, terminal state and GitHub URL. The new observation, message, recipient receipt and event commit atomically. Repeated observations and exact retries do not duplicate messages; a later observed reopening followed by closure is a new transition. Initial unknown/already-terminal observations and historical imports establish a baseline without replaying old closures. No future messages are delivered to retired workers.

Controller messages use the ordinary inbox pending/unread discovery, selected-message reads and explicit seen/acknowledgment. They are one-way notification roots and have no worker reply destination. Receipt changes do not change PR state, grant authority or mark review work complete. The [incoming feed](message-events.md) owns delivery and wait semantics; host wakeup remains a separate capability.

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
