# Retained review quota waits

This slice follows native CI observation and keeps an authorized review request
alive when its provider has no available allowance. It does not discover new
reviews, increase grants, adjudicate findings, or merge pull requests.

## One request, multiple unused preparations

An observed Hosted cooldown or proven zero CLI allowance releases the unused
preparation reservation and retains the original operation with a retry floor.
The supervisor rechecks authority, configuration, grants and source before
preparing again. The first comparison remains immutable. A changed head, parent,
base, merge base or patch cannot silently become the retry target.

CLI backoff belongs to the shared provider account, including after cancellation
of one request. Hosted reservations are scoped to a repository on the account.
Within a project, an eligible earlier queued intent retains priority while its
timer waits. Neither a timer nor a fresh allowance query counts as a completed
review round.

Pause, resume and cancel apply only to an undispatched queued operation and use
an expected revision plus an idempotency key. They cannot cancel an issued
provider review or reopen terminal history. Explicit CLI requests made while a
PR is draft remain queued until a bounded fresh source check observes readiness.

## Reading state

Operation and round-progress reads expose waiting state only when requested with
`X-FC-Operation-Wait`. Version 1 preserves the original Hosted shape; version 2
also understands CLI baselines. Version 3 adds retained provider allowance
estimates. A full-capacity estimate is not the next retry time or a guarantee
that a review can start. Older clients continue receiving their existing shapes.

The public control endpoint is
`POST /api/v1/projects/{project_id}/reviews/operations/{operation_id}/control`.
The body contains `expected_revision` and `action` (`pause`, `resume`, `cancel`).
The usual project write permission and idempotency header are required.

## Recovery and scope

Restart recovery only requeues preparations with retained proof that no review
was issued. Ambiguous dispatch remains uncertain. Historical allowance captures,
attempts and evidence stay readable after a preparation detaches. Backup
validation checks the schema, timer ownership and retained allowance facts.

Schema versions 7 and 8 reserve the integrated request-flag columns so later
slices use the same migration history; this slice does not expose new override
options. Automatic review selection, wider notification changes and automatic
workspace pruning are separate work. The workspace eligibility helper alone
does not delete checkouts.
