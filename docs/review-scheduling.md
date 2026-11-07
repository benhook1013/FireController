# Review scheduling

Projects can optionally enable automatic new Hosted requests. The default is off. CLI reviews remain explicitly requested; a proven quota refusal retains that accepted request through [durable CLI waiting](review-execution.md#durable-cli-quota-waiting), without automatically choosing another PR or round. Automatic GitHub observation and automatic retry of an already authorized timer-backed request are independent of this setting; see [observation](github-observation.md) and [timer-backed waiting](review-execution.md#timer-backed-hosted-waiting).

## Automatic Hosted requests

A project manager enables scheduling through the queue's **Automatic Hosted** control, CLI or MCP. Enabling binds authorization to that manager's exact credential and an active project worker. A browser manager explicitly selects the authorizing worker while retaining the human session identity. The controller rechecks that authorization before selection and dispatch. Revoked credentials, lost management access, retired workers and restored-provider holds cannot authorize new work.

The scheduler uses the existing configured queue and ordinary durable selection/operation path. It checks eligible work at most once per five-second coordinator interval; this is not a promised start time or another GitHub polling loop. Source, dependency, publication, findings, routes, imported obligations, round limits, taper and provider guards still apply. Independent PRs can proceed while another awaits fixes; real dependencies retain predecessor guards. Automatic requests never inherit out-of-order or file-limit overrides.

Meaningful source, review or control changes can create another automatic intent. A waiting later PR does not prevent reconsideration of an earlier PR that becomes eligible; the earlier request can take priority while the later request retains its identity and cooldown. Existing queued work on a PR prevents a duplicate intent for that PR, and active provider work is not interrupted. Timer-backed requests retain their original operation and retry deadline. Ticks, restarts, observation timestamps and failed/cancelled operation transitions cannot recreate the same request. A clean CLI completion alone cannot restart a cancelled Hosted request. A dependent PR can become eligible when its actual predecessor completes at the same head. Cancellation stops that intent; use a PR pause to hold new reviews persistently.

Results still require normal adjudication, finalization and fix evidence. Automation does not infer fixes or grant more reviews. Enabling it can spend existing authorized Hosted allowance; it is a deliberate project control, not a prerequisite for routine review use.

## Pause a PR

An ordinary project writer can pause new reviews for a PR, even before its first review intent. Queue entries expose **Pause reviews** and **Resume reviews**. The pause covers undispatched work in both channels, preserves queued identities and cooldown deadlines, and permits collection of an already issued review. An optional short reason explains the pause; resume clears it.

Draft PRs are separately ineligible for dispatch. [Explicit CLI requests can wait for readiness](review-execution.md#cli-requests-while-draft). A PR pause does not change GitHub Draft state, round settings, earned taper, channel closure, worker coordination pause, project execution pause or an individual operation's pause. Resuming a PR does not resume an explicitly paused operation or reopen a closed channel. See [operation controls](review-execution.md#timer-backed-hosted-waiting) and [round controls](review-round-controls.md).

## CLI and MCP

Read the current scheduling revision before changing settings. Keep the exact request and key after an uncertain mutation outcome; replaying the same key cannot enable or pause twice.

```sh
fc reviews scheduling
fc reviews scheduling configure --automatic-hosted true --expect-revision 0 --key retained-enable-key
fc reviews scheduling read --pr 123
fc reviews scheduling pause --pr 123 --reason 'Preparing fixes' --expect-revision 0 --key retained-pause-key
fc reviews scheduling resume --pr 123 --expect-revision 1 --key retained-resume-key
```

Normal `reviews execution` / `review_execution_read` output composes execution with this scheduling read, including its independent revision, automatic setting and authorizing worker. The authorizer is permission attribution, not lane ownership. Use [quiet review activity watch](review-operations.md#quiet-review-activity-watch) to discover automatic requests and subsequent native operation changes without manufacturing inbox messages.

MCP equivalents are `review_scheduling_read`, `review_scheduling_configure`, `review_pull_scheduling_read` and `review_pull_scheduling_update`. The update takes explicit PR number, `paused`, expected revision and idempotency key; project configuration takes explicit `automatic_hosted`, expected revision and key. Use tools advertised by the running client. Neither read starts work. Project reads include only currently queued paused PRs; explicit PR reads can inspect pauses outside the queue.

## HTTP and retention

`GET`/`POST /api/v1/projects/{project_id}/reviews/scheduling` read/configure project scheduling. The write body is `{expected_revision, automatic_hosted}` and requires project management. `GET`/`POST /api/v1/projects/{project_id}/reviews/pulls/{number}/scheduling` read/update a PR pause. The write body is `{expected_revision, paused, reason?}` and requires project write access. Writes require the normal explicit idempotency key. Unconfigured records read as revision zero and off/unpaused; writes return a new revision. Project reads report unavailable authorization or provider holds without exposing credential identifiers or tokens.

Backups retain scheduling, PR pauses and automatic-intent attribution. Older bundles without this optional domain remain off. Restore remains inactive with provider execution held. Historical authorizers remain valid historical evidence after revocation or retirement.

## Validation

Focused tests cover default-off behavior, authorization withdrawal, browser/writer/manager boundaries, pause before intent and dispatch, timer identity, cancellation suppression, independent progression and same-head dependency release, ordinary supervisor dispatch, typed CLI/MCP/browser contracts, exact-key recovery and backup/inactive restore. Controlled providers spend no live review allowance. Actual Chromium interaction proof covers desktop and mobile width. Installed release verification is recorded with deployment evidence rather than inferred from component tests.
