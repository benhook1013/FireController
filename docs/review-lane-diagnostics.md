# Review lane diagnostics

The read-only lane diagnostic explains each configured PR's local evidence and dependency state using the latest retained source frame for the selected channel. It never starts or resumes selection, refreshes GitHub, contacts a review provider, changes allowance or adjudicates a result.

Use `fc reviews scheduling diagnostics --channel cli` or `hosted`, MCP `review_lane_diagnostic` with the explicit `channel`, or `GET /api/v1/projects/{project_id}/reviews/lanes/{channel}/diagnostics`. Project read access suffices. The response contains at most the configured 100 PRs in queue order. It returns no provider bodies or credentials.

## Reading the result

The scope is always `retained_source_local_policy`. `evaluated_at` is the time of this local read; `source_checked_at`, each candidate's `source_observed_at`, and `source_configuration_revision` describe retained acquisition, not a fresh provider check. `source_complete` means that acquisition covered the configured queue with no global source error. It does not establish current GitHub heads or dispatch eligibility. Unknown suffixes remain `unknown` with `source_not_observed`; a changed configuration or repository invalidates the frame for all current candidates. Missing or damaged evidence never becomes an empty healthy lane.

Each observed candidate has `local_state` and `local_reason`. `locally_ready` means only that the shared local evaluator found no evidence, source-obligation, route, PR-pause, round or actual-parent hold in that retained comparison. It is not a preview of the next automatic selection. Provider limits, active operations, automatic-intent suppression, authorization, current source and transactional admission still apply. `lane_reason` separately reports a configured project/channel pause, disabled channel or global storage/identity hold.

A `predecessor_unfinished` row identifies `blocking_pull_number` and the concrete source-identity `blocking_reason`, such as a missing, moved, cyclic or unavailable parent. Ancestor findings, review rounds and PR pauses do not hold a child's review. The diagnostic shares the ordinary selector's source-chain checks and target-local obligation checks; it does not grant dispatch or merge permission. A local hold on the candidate itself remains its primary reason.

Hosted diagnostics optionally include `pending_selection`: the current queued or selecting journal identity (`id`, `state`, `created_at`, `updated_at`), read in the same local transaction. These are retained selection timestamps, not provider-review start or completion times. The field is omitted when no pre-child selection exists, including after child creation; it never infers a PR, head or operation before selection succeeds. CLI diagnostics omit it. This projection performs no source refresh, polling, dispatch or journal mutation.

For a lane audit, combine this diagnostic with current operation/scheduling/round reads and current published source observations. Do not use `locally_ready` to force a review or declare a lane healthy. A stale or incomplete frame means eligibility remains unestablished and calls for an authorized ordinary source/selection workflow, not a new review merely to obtain diagnostics.

## Validation boundary

Focused synthetic tests cover retained pending evidence, independent and stacked review progression, actual-parent source holds with exact blocker identity, incomplete/missing/configuration-changed/repository-changed source, malformed retained frames, revoked read access, strict bound client responses, read-only MCP discovery, CLI parsing and unchanged durable selection state. A 100-row shared completed-ancestor case passes the bounded read deadline normally and under the race detector. Each candidate's local facts stay within the current read transaction; ancestor traversal reads source identity rather than ancestor review sufficiency. Local proof does not establish deployed service support, current live admission or provider throughput.
