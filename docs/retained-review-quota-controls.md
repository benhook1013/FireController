
### Retention failure recovery

A Hosted cooldown retention storage failure leaves the original attempt preparing after bounded local retries. Before interrupted-attempt recovery, the configured Hosted adapter reconciles the retained allowance-query capture against the same operation, query, attempt and pinned workspace, with no provider call. Successful replay writes the same idempotent cooldown facts and normal recovery restores a future queued wait. An ongoing storage failure blocks startup without changing the attempt. Missing, malformed or non-cooldown private source, or a changed workspace, grants no cooldown and follows normal interrupted preparation cancellation. This replay never obtains fresh allowance or dispatches a review.

CLI account retry queries use indexed live deadline and exhaustion-time ranges; their capped prior-transition count uses the owning attempt index. Expired retained history remains auditable without being scanned as current quota proof. Actual database and context failures still propagate and stop the supervisor rather than admitting an account whose live guard was not established.
