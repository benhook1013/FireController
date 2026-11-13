# Retained review quota waits

This slice follows native CI observation and keeps an authorized review request
alive when its provider has no available allowance. It does not discover new
reviews, increase grants, adjudicate findings, or merge pull requests. This stage delivers retained-state migration, validated historical records, admission guards and CLI quota/draft waiting. The Hosted adapter does not yet schedule or replay retained cooldowns; that runtime integration follows in a dependent stage. The Hosted behavior below defines the target contract and the historical records accepted by this foundation. Public progress/control endpoints and bound client, CLI and MCP controls follow above both channel behaviors.

## One request, multiple unused preparations

An observed Hosted cooldown with a future deadline that advances any previous retry floor, or proven zero CLI allowance, releases the unused preparation reservation and retains the original operation with a retry floor.
The supervisor rechecks authority, configuration, grants and source before
preparing again. The first comparison remains immutable. A changed head, parent,
base, merge base or patch cannot silently become the retry target.
Runtime reads verify that the stored retry deadline matches the latest retained, proven cooldown. The first comparison and allowance estimate remain bound to the original baseline. A syntactically valid but contradictory timer is damaged waiting history, not permission to prepare early. CLI account guards also derive live retry floors from retained exhaustion transitions, so shortening the cached timer cannot hide an active account backoff.

During normal preparation, an elapsed or non-advancing Hosted cooldown instead leaves the operation `held` with reason `hosted_allowance_cooldown`. A missing or malformed retained Hosted allowance record also holds only that operation, without inventing a retry timer or stopping unrelated work. Actual storage failures retain their normal fatal behavior. It does not retry automatically, and the queued-only pause, resume and cancel controls cannot resume it. Inspect the retained operation and allowance facts, resolve the hold's cause, then submit a fresh authorized [review request](review-execution.md#bound-execution-tools) with a new explicit key and the qualified current comparison. The held history remains unchanged; the new request still passes authority, configuration, grant, source, finding, provider and known-cooldown guards. Recover an unknown original request outcome with its original key before considering a new request.

Startup recovery of a still-preparing Hosted attempt revalidates the retained cooldown and restores a queued wait only when its deadline remains future and advances the prior retry floor. An elapsed or non-advancing deadline instead cancels that unused preparation with reason `supervisor_interrupted` and releases its reservation, preserving the original allowance facts and attempt history. A still-preparing CLI attempt with a valid retained zero-allowance record instead re-arms its account backoff from the recovery transition time; it does not apply the Hosted future-deadline or advancing-floor checks. An operation already held before restart remains held; startup recovery does not reopen it or issue another review.

CLI backoff belongs to the shared provider account, including after cancellation
of one request. Hosted reservations are scoped to a repository on the account.
Within a project, an eligible earlier queued intent retains priority while its
timer waits. Neither a timer nor a fresh allowance query counts as a completed
review round.

Damaged live CLI wait history suppresses candidates only on its provider account. Other accounts and Hosted work remain schedulable. Damaged retained Hosted waits suppress the affected candidate and later queued intents in its project/channel that depend on that unproven predecessor; other projects and channels remain schedulable. Typed retained wait failures in either channel emit a bounded candidate diagnostic using the same 30-second, 256-entry diagnostic budget; actual database and context errors still propagate. The retained request and evidence are not changed, and expiry removes only the account timer, not audit validation.

Malformed or missing intermediate configuration history keeps the affected request queued without acquiring capacity or preparing a provider action. It also prevents later requests from bypassing that unproven predecessor, but does not stop other projects. A bounded diagnostic identifies the affected candidate and project without exposing stored content, at most once per candidate per 30 seconds within a 256-entry diagnostic bound. General database and context failures retain their normal error behavior.

Pause, resume and cancel apply only to an undispatched queued operation and use
an expected revision plus an idempotency key. They cannot cancel an issued
provider review or reopen terminal history. Explicit CLI requests made while a
PR is draft remain queued until a bounded fresh source check observes readiness. A failed repository or pull probe leaves the request queued and emits a diagnostic with the operation ID, failed read and safe provider error code, at most once per operation per 30-second probe interval; provider bodies and credentials are excluded. An unknown Draft field uses the same bounded diagnostic with reason `draft_state_unknown`, without treating the PR as ready.

Only one of the two supervisor workers probes retained Draft waits. Its scheduling pass selects at most one eligible probe and advances its rotation cursor at selection, so a local validation failure before probe claim cannot starve later waits; the other worker excludes Draft waits before applying the candidate limit, so slow probes do not hide ready work in other projects. Both workers retain the same authority, queue-priority, account and reservation guards.

## Recovery and scope

Restart recovery only requeues preparations with retained proof that no review
was issued. Ambiguous dispatch remains uncertain. Historical allowance captures,
attempts and evidence stay readable after a preparation detaches. Backup
validation checks the schema, timer ownership and retained allowance facts.

Schema versions 7 and 8 reserve the integrated request-flag columns so later
slices use the same migration history; this slice does not expose new override
options. Its HTTP operation responses omit both reserved flags for every supported wait version while retaining their stored values. Automatic review selection, wider notification changes and automatic
workspace pruning are separate work. The workspace eligibility helper alone
does not delete checkouts.

### Retention failure recovery

The following Hosted recovery procedure is future behavior for this foundation slice and requires the dependent Hosted runtime stage; restarting this slice alone does not provide that replay.

A Hosted cooldown retention storage failure leaves the original attempt preparing after bounded local retries. Before interrupted-attempt recovery, the configured Hosted adapter reconciles the retained allowance-query capture against the same operation, query, attempt and pinned workspace, with no provider call. Successful replay writes the same idempotent cooldown facts and normal recovery restores a future queued wait. An ongoing storage failure blocks startup without changing the attempt. Missing, malformed or non-cooldown private source, or a changed workspace, grants no cooldown and follows normal interrupted preparation cancellation. This replay never obtains fresh allowance or dispatches a review.

CLI account retry queries use indexed live deadline and exhaustion-time ranges; their capped prior-transition count uses the owning attempt index. Expired retained history remains auditable without being scanned as current quota proof. Actual database and context failures still propagate and stop the supervisor rather than admitting an account whose live guard was not established.

When the original Hosted recovery driver is unavailable, startup preserves that evidence-backed preparation and its reservation, emits a diagnostic identifying its operation and attempt, and continues recovering unrelated work. Restore the original capability and restart to retry its retained source reconciliation. Actual replay, storage and context failures still block startup; they are not permission to cancel the original attempt.

Before SQL retention, CLI zero-allowance observations are saved as bounded, digest-checked, attempt-bound private artifacts under `cli-exhaustion-captures`. Startup can replay them without the original CLI executable, credentials or workspace. Replay validates the original operation, attempt, project, provider/account, comparison and configuration/policy bindings, preserves the original observation timestamp, and atomically retains the zero observation and account cooldown. It makes no provider call, grants no review credit or permit, and does not bypass current admission checks when the original request becomes retryable.

Missing, malformed or mismatched private CLI source follows ordinary unused-preparation cancellation and releases its reservation. Inaccessible private storage or a failed replay transaction preserves only the affected preparation and reservation with a diagnostic while unrelated attempts recover; retry recovery after resolving that failure. A missing current CLI driver alone does not invalidate an intact original binding. Managed backup includes these artifacts even before a SQL preflight row exists, and restore does not require the original worktree. Already-retained SQL observations recover through their existing path independently of private-source availability.
