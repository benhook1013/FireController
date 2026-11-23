# Review scheduling

## Pause a PR

A project writer can pause new reviews for a PR before its first review intent or while it has queued work. The pause covers undispatched Hosted and CLI work. It preserves accepted operation identities and their cooldown deadlines, and collection of an already issued review can continue. An optional reason explains the pause. Resuming clears that reason.

Draft eligibility, [round settings](review-round-controls.md), earned taper, channel closure and operation controls remain separate. To resume an individually paused operation, use its [operation control](retained-review-quota-controls.md).

Use `GET` and `POST /api/v1/projects/{project_id}/reviews/pulls/{number}/scheduling` to read or update the pause. A write supplies `expected_revision`, `paused` and an optional `reason`, with the normal explicit idempotency key. An unconfigured PR reads as revision zero and unpaused. Keep the original request and key when a mutation result is uncertain.

Project scheduling is read-only. `GET /api/v1/projects/{project_id}/reviews/scheduling` shows the retained revision, settings, authorizing worker and pauses for currently queued PRs. It reports unavailable authorization and provider holds without exposing credentials. The per-PR read can find controls outside the queue.

## Accepted work

Explicit selection uses the existing durable queue. Automatic origin in retained state requires matching attribution, an authorized original worker and complete relevant scheduling history. Queued or selecting work retains its original authorizer. An already selected operation can retain that original authorizer through a manager change when both authorizers remain valid and the intervening history proves continuous enablement of its channel. Missing or conflicting attribution and ambiguous legacy selections hold for operator investigation. Legacy explicit origin can be proven by an exact retained mutation receipt, a caller-supplied PR pin or complete history from before automatic scheduling for that channel.

Current queued diagnostics identify an earlier accepted Hosted request before the later request's independent cooldown. The bounded local read uses compatible active queue and quota bindings. A paused, cancelled or invalid earlier request releases that turn. CLI shared-account quota diagnostics retain the original deadline while the accepted account and current configuration still agree.

Version 5 of `X-FC-Operation-Wait` extends version 3 with optional `queue_ahead_pull_number` in the round-progress read. Older response versions omit it. It reports the accepted predecessor without changing allowance or deadlines.

## Backup and restore

Backups retain scheduling settings, PR pauses, acceptance events and automatic attribution. Inspection checks their consistency. Older bundles may omit the optional domain. Version-one Hosted-only settings migrate with automatic CLI off. Restore lands in an inactive destination with provider execution held. Historical authorizers remain evidence after revocation or retirement.

A legacy bundle can omit a PR control when its unique latest canonical event proves an unpaused resume. Restore retains that event's actor, revision and time in the destination control and preserves the journal. A missing paused control or ambiguous history blocks restore.

A linked selection with proven custody retains its original child identifier and observed revision in a diagnostic record. Quarantine preserves the child, authorizer, retained evidence and any existing provider action. A held child cannot obtain new admission or reserve a queued turn. Ambiguous provenance holds for investigation.

The active selection custody migration is forward-only. Retain a verified version-one backup before upgrading. For binary rollback, restore the verified bundle to a separate inactive destination and use the [backup recovery procedure](backups.md). Version-two bundles require compatible readers and retain custody records pending an explicit retention policy.
