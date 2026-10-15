# Source Refresh and Observed Policy

## Implemented boundary

An explicit source refresh reads the selected GitHub PR and only the repository and preceding PR facts needed to identify its comparison. It durably queues the operation before provider work starts. A short default wait returns the completed result in the original call when possible; only work that remains pending returns an operation receipt. It does not request a review, invoke the CLI, inspect allowance or download completed review captures. Ordinary operation and assessment reads use stored records without contacting GitHub.

The observed-policy assessment applies current local channel requirements and finding obligations to that stored source comparison. Its source observation time and policy evaluation time are separate. It is an as-of assessment, not a guarantee that GitHub has remained unchanged, a required-CI result or a merge-readiness verdict. The [captured-comparison assessment](review-controls.md#captured-comparison-assessment) remains available for inspecting a particular historical native capture.

## Request and recovery

`POST /api/v1/projects/{project_id}/reviews/source-refreshes` requires project write access, an idempotency key and exactly `expected_configuration_revision`, `pull_number` and `head_sha`. The PR must be in the configured queue, and the expected revision and head are assertions. The service never substitutes a different target. The HTTP handler waits up to two seconds by default. A completed or held operation returns its compact outcome with HTTP `200`. If work remains pending, HTTP `202` returns its original queued receipt containing ID and revision. The optional `Prefer: wait=N` header selects zero through sixty seconds: zero returns immediately, while a longer explicit wait can return the result without another model-facing call. The supported wait preference follows the purpose of the [HTTP wait preference](https://www.rfc-editor.org/rfc/rfc7240.html#section-4.3); it is result-collection metadata, not part of the idempotent action request.

Retain the original request, key and profile binding until its outcome is known. Exact-key replay preserves the original action and internal receipt; its wait can return the current completed outcome without another provider read. Compatible queued or running requests from the same project, actor and worker can share one operation; changing repository, PR, asserted head, source configuration or capability prevents that coalescing. The queue admits at most 64 active refreshes instance-wide and 16 per project. A capacity refusal starts no provider work.

`GET /api/v1/projects/{project_id}/reviews/source-refreshes/{refresh_id}` returns stored state and, for a completed refresh, the exact observed repository, PR, candidate, base and effective parent. States are queued, running, completed and held. A source mismatch, unavailable provider or changed authorization holds the operation with a safe reason. A failed refresh preserves earlier completed observations and their age. Reads do not restart failed work.

One bounded source worker runs independently of review dispatch. Repository and PR reads share a fifteen-second provider budget and run outside database transactions. Interrupted running reads can be queued again at restart because they perform no external mutation. Startup and shutdown belong to the daemon's shared worker owner; the database remains open until every owned worker has finished. An installation without configured provider capabilities starts no source polling loop.

Waits subscribe to bounded per-operation completion notifications outside database transactions. Expiry or cancellation releases the subscription without cancelling the queued action or acknowledging anything. The result is reauthorized before it is returned.

The service checks the original actor's access, stable repository binding and operator capability before fetching and before recording its result. Source configuration changes hold in-flight work. Changing only the execution pause leaves the source comparison valid; pausing a review channel does not prevent an explicit source refresh or erase existing review credit.

## Local assessment

`GET /api/v1/projects/{project_id}/reviews/source/{pull_number}/policy` requires project read access and accepts no query parameters. It evaluates the latest completed source observation and current policy in one authorized database snapshot. Missing source is explicit rather than an empty passing assessment. A changed repository, capability or source configuration holds the old observation; changing only the execution pause preserves it.

The response identifies the refresh, exact target, current configuration and policy revisions, source observation time and evaluation time. Each required channel reports coverage and pending evidence. All required channels need applicable finalized native captures and satisfied finding obligations; pending evidence dominates coverage. Imported history supplies no native credit. Missing channel policy is unconfigured. None of these states authorizes a merge.

## Bound agent tools

The CLI uses its selected private profile:

```sh
fc reviews source-refresh --pr 8 --head SHA --expect-config 2 --key retained-action-key
fc reviews source-operation REFRESH_ID
fc reviews source-assess --pr 8
```

Source refresh additionally accepts `--wait-seconds N` from zero through sixty; omitted means two seconds. A longer explicit wait extends only that command's client budget to the chosen wait plus five seconds. Other commands retain their normal deadline.

The equivalent MCP tools are `review_source_refresh`, `review_source_refresh_read` and `review_source_policy_assess`. The refresh tool accepts optional `wait_seconds` with the same default and range. Refresh is an explicit mutation that schedules provider reads; the other tools are provider-free reads. There is no automatic polling, generated retry key, implicit project selection or per-command queue refresh. Bounded results use the normal shared client deadline. A receipt means accepted for background work, not completed.

## Proof and limits

Implementation validation and packaged runtime proof must establish queue replay, authorization, scope, cancellation, restart recovery and provider isolation before this boundary is called complete. Controlled provider facts cannot establish real account behavior, fleet throughput, CI readiness or operational cutover.
