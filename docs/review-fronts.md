# Stored Review Fronts

## Implementation status

The service stores bounded source frames from native queue selection and exposes their current local assessment to the browser. Focused core, HTTP, backup/restore and browser tests pass. Packaged front-read integration is recorded in [channel-front review selection](../tools/packagedsmoke/README.md#channel-front-review-selection); installed runtime qualification belongs to [own-project live proof](review-execution.md#own-project-live-qualification) and [deployment proof](hosting.md#executed-deployment-proof). Do not infer a front from the first open merge head or retained review counts.

## Purpose and authority

Expose a provider-free, as-of explanation of each configured channel's current position. Hosted and CLI can have different fronts. This display does not reserve capacity, authorize dispatch, establish CI readiness or permit a merge. Actual requests continue to acquire fresh source facts and use the canonical native admission path.

The existing native queue-selection evaluator owns ordering and holds: merged predecessors are skipped, unresolved routes or evidence hold the prefix, completed local requirements are skipped, an uncovered closed predecessor holds, and the first eligible open PR is the front. The display reuses that evaluator rather than implementing another interpretation of the rules.

## Bounded source cache

Retain at most one current source frame per project and channel. A frame records the configured repository ID and configuration revision, the local check time, and the verified source prefix acquired by that selection. Each prefix entry retains only the source fields required by the evaluator: PR identity, state/merged flag, head/base/ref, effective parent identity and source observation time. A separate boolean records whether that entry was visible in the stored workspace when the frame committed. Titles, diff statistics, captures, credentials, actor instructions and historical review bodies do not belong in this cache.

Prefixes contain at most the configuration's one hundred PRs in its exact leading order, with canonical JSON bounded to 128 KiB. They distinguish a complete queue from a prefix that stopped at a front or hold. A failed read without a verified prefix records unavailable source rather than leaving an apparently fresh successful front. The cache replaces its prior frame; it does not append a full source prefix to every selection's lifetime history. Existing operation, attempt, capture and selection histories keep their own retention semantics.

Persist the frame in the same final transaction that records selection after current repository, configuration and authorization checks. A changed binding cannot install an old frame. Recording an expected-PR mismatch may still retain its verified actual source prefix; that failed assertion does not redefine the channel's real front. Existing selection events notify clients, including when PR display metadata is unchanged. Explicit single-PR source refresh does not claim to have acquired a queue prefix. No new provider reads, timer crawl or review invocation are introduced.

## Read API

`GET /api/v1/projects/{project_id}/reviews/fronts` requires project read access and accepts no query fields. It returns the current repository, configuration and policy revisions, current workspace snapshot when available, configured queue order and separate channel results. An unconfigured response has `configured:false`, zero configuration/policy revisions and empty queue/channel arrays. Responses never include raw source prefixes, private captures or provider diagnostics. This is a browser read, not an additional agent tool or a request to acquire source.

## Local assessment

Assess the cached prefix against current local requirements, routes, evidence and closure controls in one bounded read transaction. A policy change can therefore change the display without calling GitHub. Reaching the end of an incomplete prefix after local requirements have advanced means unknown, not queue complete; an explicit fresh queue selection obtains the next source prefix through the ordinary workflow.

Refuse a ready/front claim when the current repository or configuration differs, the frame is malformed or absent, or a cached prefix's visible stored PR lifecycle/head/base no longer matches. An entry visible at acquisition cannot disappear or become retired without invalidating that explanation. A predecessor absent at acquisition may remain absent: observing a source prefix does not require importing unwanted historical PRs into the website. If that entry later appears, compare its current facts normally. A ready front itself must be visible and match; a held diagnostic may identify an unrepresented blocker without claiming readiness. A newer administrator edit or retirement must not be overridden by a cached frame. Include the current workspace snapshot identity in the assessment so a browser can reject a response for another loaded queue version. Unknown or absent source stays explicit; timestamps describe observations rather than a continuously checked state.

Return configured channels independently with `ready`, `held`, `complete` or `unknown` state, a bounded reason where needed, the implicated PR number/head/base where established, and source/local check timestamps. Held results identify the actual blocking PR when known. Preserve the existing admission evaluator's behavior for its callers while exposing this richer diagnostic result. Native pause or restore recovery cannot become permission to request a review; display any such hold explicitly.

## Storage and recovery

The optional `review_fronts` schema version one owns a project/channel current-frame table, foreign project identity and strict canonical row validation. Ordinary current lookups use its primary key. Older canonical backups without this optional domain remain supported; backups containing it validate its exact schema, bounded canonical data, source identities and prefix invariants. Inactive restore preserves its historical observation facts and the normal recovery fence. Cached source never bypasses the fresh-request or restore-resumption boundary.

## Browser use and proof

Delivery and Reviews share one bounded local front read. A response must match the displayed current workspace and relevant PR identities before a front badge is shown. Current round closure remains separate from source position. Queued means an established position behind a channel's known front, not a promise that all provider or admission checks would pass. Historical snapshot pages do not present current front state as historical permission.

Proof covers different channel fronts, blocking predecessors, incoming routes/evidence, closure/taper and policy changes, incomplete-prefix exhaustion, source failure, concurrent configuration/display changes, retirement, restart/backup/restore validation and scope isolation. Provider call counts stay unchanged. Browser proof covers mixed/unknown states, stale responses and narrow layouts; no live provider or operational cutover claim follows from synthetic fixtures.
