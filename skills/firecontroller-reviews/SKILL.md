---
name: firecontroller-reviews
description: Use an existing FireController connection to enroll or inspect PRs, run authorized reviews, adjudicate findings and retain fix evidence. Use for FireController review work; ordinary jobs/messages use firecontroller-use and installation uses firecontroller-onboard.
---

# FireController review tools

Use the supplied worker-bound connection and the project's existing authorization, review settings and workflow. Read [native review operations](references/operations.md) when entering review work or recovering missing operational context. It covers queue add/move, compact inspection, durable operation waits, finding batches, finalization, allowance holds and independent Subagent results.

For project-wide hourly Hosted/CLI activity, use `reviews activity` / `review_activity_read`; no per-PR aggregation is needed. See [activity coverage](../../docs/review-activity.md) when interpreting totals or undated history.

For newly created automatic requests or project/PR/channel activity without a known operation ID, use `reviews watch` / `wait_for_reviews`; load [quiet review activity watch](../../docs/review-operations.md#quiet-review-activity-watch) for initial snapshot, separate event cursor and host-delivery limits.

For optional automatic Hosted scheduling or pausing new reviews on a PR, load [scheduling controls](../../docs/review-scheduling.md). These are explicit project/PR controls, separate from ordinary requests and worker pauses.

This skill describes existing tool mechanics. It does not select project priorities, pipeline depth, successor-publication cadence, merge authority or new review-train strategy. Those remain in the human's instructions and project workflow. Tool availability never authorizes a review request, allowance query, new grant or pause change. A project instruction to defer reviews remains in force.

Keep the original request/key and comparison identity after an unknown outcome. A posted trigger is not proof of execution, and a pending receipt is not completion. Use retained evidence and supported waits instead of duplicating provider actions or narrating repeated status reads. Finding decisions and fix references have distinct meanings; finalized counts remain attached to their original review.

For exact argument schemas, prefer discovered MCP tools or local operation help. Matching-release [execution](../../docs/review-execution.md), [evidence controls](../../docs/review-controls.md), [round controls](../../docs/review-round-controls.md) and [Subagent reviews](../../docs/subagent-reviews.md) own the contracts. Do not load all of them for an ordinary operation. Reuse relevant guidance already in context; reread only when missing or when the selected operation needs another reference.

Ordinary explicit PR work automatically follows review-ready and PR merge/close inbox notices. Use a review-ready notice’s original evidence handle to inspect the result; it does not mean adjudication or fixes are complete. For deliberate follow/unfollow preferences, read [PR inbox notifications](../../docs/review-notifications.md); preferences belong only to the bound worker and an explicit mute survives later work. Notifications grant no ownership or permission to merge.
