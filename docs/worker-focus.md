# Worker Primary Job

## Implementation status

The primary-job API, typed client, CLI and MCP adapters are implemented. Focused public API, adapter and backup/restore race tests pass. Full combined validation and actual packaged proof pass at the source checkpoint recorded below. Existing job statuses, [updates and checklists](job-progress.md), and [checkpoints](job-checkpoints.md) remain independent.

## Focus and assignment

A worker has one optional primary job within its project. This is an explicit resume focus, not a restriction on concurrent active jobs. New workers have no primary. Selecting, switching or clearing focus never edits a job's brief, assignment, status, revision, checklist or checkpoint. A parked, blocked or completed job may remain primary; status changes do not silently select or clear focus.

A selected job must exist in the same project and currently be assigned to the target worker. Reassigning or unassigning that job clears its previous worker's primary in the same job-edit transaction. That clear advances the independent focus revision and appends its history/event; it does not select a primary for the receiving worker. A failed or stale job edit cannot clear focus. Current authorization governs exact-key replay, which returns the original receipt without repeating effects.

Project read/write permissions govern these operations. The worker selector identifies the target; the existing authenticated worker header supplies attribution. It does not create worker-exclusive authorization or permit cross-project access. Ordinary bound CLI/MCP calls inherit the target worker from their private profile.

## API and guarded history

Routes are relative to `/api/v1/projects/{project_id}/workers/{worker_id}`.

| Endpoint | Request or result |
| --- | --- |
| `GET /primary-job` | `{worker_id, revision, job_id}`; absent focus is revision zero and `job_id:""`. A later clear retains its incremented revision. |
| `PUT /primary-job` | Required `expected_revision` and `job_id`; empty string explicitly clears. Returns `{id, revision, outcome}` with the target worker ID. |
| `GET /primary-job/history` | Explicit newest-first `{items, cursor?, has_more}`, default 20 and maximum 100 records. |

The request requires both fields; omitted/null values, unsupported fields, invalid IDs and revision guards outside zero through `MaxInt64-1` fail before mutation. A missing target worker returns `worker_not_found`; a missing selected job returns `job_not_found`. A selected job assigned elsewhere or unassigned returns HTTP 409 `primary_job_assignment_conflict`. A stale focus guard returns HTTP 409 `primary_job_revision_conflict` with `details.current_revision`. An assignment change that would overflow the old worker's focus revision returns HTTP 409 `primary_job_revision_exhausted` without changing either resource.

Focus revisions are independent of job revisions. A changed replacement returns `outcome:"updated"`, advances the focus revision, and atomically records current state, immutable focus history, one compact `worker.primary_job_changed` event and the replay receipt. An identical replacement returns `outcome:"unchanged"` with the current revision and only its receipt. Clearing an already absent initial focus is unchanged at revision zero. Revision order defines history; timestamps do not establish elapsed time or activity presence.

History entries contain `worker_id`, `revision`, `job_id`, `actor_id`, `actor_worker_id`, `reason` and `created_at`. Reasons are `selected`, `cleared` or `job_reassigned`. The compact event contains only the target worker, selected job ID and reason, never job text or evidence. A job reassignment's ordinary job update and focus-clear history/event share one transaction. Lists use the existing instance/principal/project/route/worker-bound cursors. Reads make no provider calls or durable writes and never retrieve job briefs, archives or inboxes automatically.

## Agent surfaces and proof

CLI reads are `jobs primary` and `jobs primary-history`, bound to the profile's worker. Mutations are `jobs primary-set JOB_ID --expect-revision N --key KEY` and `jobs primary-clear --expect-revision N --key KEY`. The typed client exposes equivalent read/replace/history operations. MCP tools are `job_primary_read`, `job_primary_replace` and `job_primary_history`; replacement requires a string `job_id`, with empty string meaning clear. Mutations use explicit keys, local validation before credentials/network, the existing five-second client budget and original-key recovery after an uncertain response. Current reads fit the normal compact response budget; history is bounded and explicitly paged.

Required proof covers several simultaneously active jobs with one optional focus, same-job no-op, switch/clear, independent job statuses and histories, reassignment/unassignment atomic clears, stale/racing writes, changed-key conflict, original-key replay after later changes and restart, denied/revoked/cross-project access, attribution, bound pagination and overflow rollback. Backup inspection and inactive restore retain focus and history without reviving credentials or provider work. Packaged CLI/MCP proof must exercise the actual API; implementation tests alone do not establish model-host resume behavior. Worker pause, deferred notes and a combined resume packet are separate slices.

## Executed proof and limits

At clean source `93ad696dbd09d46506e4f6a4ebe3e891e170e957`, full `make check build` passed: browser type/format/lint checks and 182 tests, all Go race tests, vet, documentation checks and 19 link-checker tests, generated browser assets and all three binaries. The built binaries matched those used in the separate actual packaged proof.

Six packaged API/CLI/official-SDK MCP groups passed in 2.815 seconds using isolated local state. They exercised selection, switching, clearing, no-op and original-key replay, a lost committed response recovered across adapters, zero-revision conflicts, bounded history, status independence, atomic reassignment clearing, authorization and instance isolation, restart, and verified backup/inspection/inactive restore preserving two focus rows and nine history rows. The proof used 60 subprocesses and 103 HTTP requests; it made no external provider calls. These small local timings do not establish fleet performance.

Independent functional review also reproduced a rehashed backup with a malformed focus-table key that passed inspection and failed on a later mutation. The corrected registry rejects missing or misplaced primary keys, incorrect types, nullable required columns and unexpected defaults in both focus tables before restore writes. Ten malformed variants and valid current/older-schema controls passed focused normal/race tests.

Separate [installed QA checks](hosting.md#job-progress-qa-upgrade) verify default reads and the deployed job-progress browser. The local checkpoint does not establish physical-phone behavior, accessibility, model-host wakeup/resume or fleet guarantees. The browser displays job progress; primary selection is currently an API and agent-tool surface. The retained development controller remains primary pending implementation completion and human browser review.
