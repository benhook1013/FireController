# Per-PR review rounds

## Implementation status

The native service, bound CLI and MCP implement explicit per-PR/channel additional-round grants, minimum/exact requirements, taper, closure/reopening and persistent native completion accounting. Source preparation and final admission recheck the grant independently of captured coverage; policy/readiness and browser assessment reads show outstanding round requirements. Backup validation preserves the control history and native completion links. Focused checks are recorded below. Automatic queue continuation and conversion of legacy operational controls remain implementation work. A grant alone performs no provider action and does not weaken the separate [review policy](reviews.md#project-policy).

## Operational controls

Hosted and CLI have independent per-PR scheduling state and independently advance from the front of the review queue. A cooldown or active review in one channel does not hold the other channel. Maximum additional rounds are counted from the request that grants them; minimum and exact-round requests remain available. A maximum permits early taper rather than requiring every allowed round. An exact-round request is explicit about the required number.

Default early taper is one eligible hosted round with zero accepted findings, or three consecutive eligible CLI rounds with zero accepted findings. A selected minimum still applies. A useful accepted correction resets the applicable dry streak; failed, incomplete, ambiguous and rate-limited attempts cannot supply successful dry rounds. Preserve the distinction between exhausted allowance, taper completion and explicit closure.

Closing a channel affects only that PR/channel. Reopening explicitly grants a new allowance. Record actor attribution and policy revision; retain consumed history when settings change. No implicit project-wide closure or reopening follows from a local change.

Head or parent movement preserves allowance history but cannot grant old capture coverage for changed code without proven applicability. Closing, tapering or exhausting a channel does not erase findings, satisfy accepted/routed obligations, establish current CI or authorize a merge. Current comparison coverage and observed merge readiness remain separate assessments.

## Native control semantics

A per-PR/channel grant is explicit administrator configuration, separate from project-wide required channels. Its minimum and maximum count additional eligible native completions from operations accepted under that grant. The maximum is between 1 and 1,000, the minimum is between zero and the maximum, and an exact request sets both to the same positive number. Defaults for a new grant are minimum zero and a taper of one hosted or three CLI dry rounds. A new grant requires an explicit maximum. Projects without a grant retain the existing single-comparison admission behavior; importing or configuring the selected controls is a deliberate adoption step.

One eligible completion has a completed native operation/attempt, validated attributable complete capture and finalized source counts. An operation counts once, independently of whether its accepted findings have been fixed. Original finalized accepted counts determine the historical dry streak; later decision/fix corrections remain separate obligations and do not rewrite consumed rounds. Failed, rate-limited and incomplete attempts consume no completed round. Queued, preparing, posting, active and uncertain requests occupy grant capacity until proven terminal; uncertain work never becomes a free retry.

Discovery completes when the maximum is consumed, or the minimum is reached and the applicable dry streak reaches its taper threshold. Exact requests therefore require the selected additional number even if taper would otherwise finish early. Before completion, an eligible round with accepted findings resets the dry streak. Reached completion remains latched until an explicit new grant; head movement and later corrections cannot silently reopen it.

Close is a revision-guarded change to that PR/channel alone, preserving its grant and consumed history. It stops new dispatch, including queued/prepared actions at their final guard, and does not cancel a possibly posted action. A posted result can still be captured, adjudicated and counted against its original grant. Reopening or changing a grant creates a fresh additional allowance and dry streak, while retaining prior grant/completion records. A new grant refuses while any operation in that PR/channel is queued, preparing, posting, active or uncertain; finish or safely settle it first. Operations accepted under an earlier grant cannot consume a later grant even if their source counts are finalized later.

Per-PR changes use their own revision and actor attribution rather than changing the project policy revision or holding unrelated PRs. Requests bind the applicable grant at acceptance and recheck it before preparation and provider dispatch. Both source preparation and final transactional admission allow another round on a covered comparison only while the bound grant still requires discovery. They preserve pending-capture/finding obligations, source identity, current authority, provider limits and reservations. For stacked progression, an explicitly finished grant replaces the predecessor single-capture requirement; outstanding evidence/fix obligations still hold dependent work. Closure or exhausted allowance without current applicable coverage remains a separate readiness limitation and never manufactures captured coverage.

Control reads are local, compact and provider-free. Administrator mutations use the ordinary authorization, expected revision, idempotency and atomic event/receipt rules. Automatic continuation must retain explicit actor/worker authority, recheck revocation and provider hold on every action, and select each channel independently; merely storing a grant does not authorize provider calls. The initial control storage/admission work does not claim that automatic continuation or legacy control conversion is already implemented.

## Bound commands and API

Commands use the selected private administrator profile; ordinary worker credentials cannot configure grants. Reads retain the ordinary five-second budget and perform no provider work. `--min` defaults to zero; `--exact` is exclusive with `--min` and `--max`. A close requires the current revision and omits allocation fields.

```sh
fc reviews rounds read --pr 42 --channel cli
fc reviews rounds grant --pr 42 --channel cli --expected-revision 0 --max 8 --key cli-42-grant
fc reviews rounds grant --pr 42 --channel hosted --expected-revision 0 --exact 2 --key hosted-42-grant
fc reviews rounds close --pr 42 --channel cli --expected-revision 1 --key cli-42-close
```

`GET /api/v1/projects/{project_id}/reviews/rounds/{pull_number}/{channel}` returns the compact configured state, revision/epoch, bounds, consumed/dry counts, taper and closure. An unconfigured read explicitly returns `configured:false` with revision zero. Administrator `POST` at the same route accepts `expected_revision`, `action` (`grant` or `close`), and grant-only `minimum`/`maximum`, with the explicit idempotency header. The receipt contains only pull number, channel, revision, epoch and outcome. Exact-key replay retains that result after later changes or restart, with current administrator authorization still required.

MCP `review_rounds_read` and administrator `review_rounds_configure` use the shared typed client, bound project/worker and strict declared arguments. MCP grant arguments use explicit minimum and maximum; exact means equal values. Neither tool refreshes source, starts a review or silently generates a key. A dispatched write with missing or invalid output is uncertain: retain its original binding, request and key for exact recovery.

## Focused proof

Real SQLite/domain normal and race tests cover grant baselines, close/reopen isolation, current authority and demotion, CAS/replay, all live/uncertain states, capacity, minimum/taper, original finalized counts, late old-epoch completion and atomic failure. An actual ledger-write failure rolls back native finalization and receipt; exact-key retry counts once. Shared client/CLI checks use actual service handlers for strict scope, lost-response/stdout recovery and no-provider effects. Official SDK stdio checks cover the two tools, administrator enforcement, replay and complete documented discovery.

The real local runner with a synthetic executable completed three additional CLI rounds on an already covered head and stopped at the default dry taper. Closing after preparation prevented another review process from starting. Focused race proof passed in 6.945 seconds; this is controlled execution, not live provider proof. Independent source review found a missing grant check inside transactional attempt admission; that check is now present, with a distinct close-between-preparation-and-admission regression. Current captured coverage, finished discovery and observed readiness remain separately tested.

Real backup creation/inspection/inactive restore covers a closed consumed grant, a new grant unaffected by late old-epoch completion, finalized accepted findings whose fix obligations remain pending, and genuine pre-grant captures that gain no additional credit. Resealed logical, key/type, audit, progress and schema/index corruption is refused before restore writes; older bundles without the optional domain remain supported. This validates the retained authority graph and corruption boundaries, not coherent rewriting of every authoritative native record. Operational migration, automatic continuation and installed cutover are not implied.

Actual packaged `fc`, `fcd` and official-SDK stdio `fcmcp` checks completed 23 CLI invocations and seven MCP calls in a 341 ms local workload. They exercised default/exact grants, cross-client original-key replay, PR/channel/project isolation, same-session credential demotion, uncertain stdout recovery and actual daemon restart. The workload made zero external/provider calls, created no review operations or completion credit, preserved unrelated table contents and left no owned process running. This is local functional proof, not sustained throughput, live provider, native-platform or installed-service proof. The canonical `make smoke` target includes this check.

## Migration boundary and proof

Carry effective current round limits, minimum/exact requirements, taper state and channel closure into the new operational policy with explicit provenance. Retain original records privately; rebuilding commands for retrospective allocation/credit-period manipulation is outside the adoption requirement. A converter must report unsupported live behavior rather than silently substitute a different rule.

Map source behavior tests to functional outcomes before implementation: additional allowance baselines, minimum/exact requests, eligible dry streaks and reset, closure/reopen isolation, independent queue advancement, head/parent movement, exhausted-versus-taper status and outstanding findings. Test observable scheduling and evidence results rather than copying legacy storage or command internals. Imported historical settings must not manufacture native captured review credit.
