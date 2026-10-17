# Per-PR review rounds

## Implementation status

These controls are selected behavior for legacy adoption. The native service currently implements required/paused project channels, file limits and captured-evidence obligations; per-PR round budgets, taper, channel closure and independent automatic queue progression are not yet implemented. This contract adds those controls without changing historical evidence or weakening the separate [review policy](reviews.md#project-policy).

## Operational controls

Hosted and CLI have independent per-PR scheduling state and independently advance from the front of the review queue. A cooldown or active review in one channel does not hold the other channel. Maximum additional rounds are counted from the request that grants them; minimum and exact-round requests remain available. A maximum permits early taper rather than requiring every allowed round. An exact-round request is explicit about the required number.

Default early taper is one eligible hosted round with zero accepted findings, or three consecutive eligible CLI rounds with zero accepted findings. A selected minimum still applies. A useful accepted correction resets the applicable dry streak; failed, incomplete, ambiguous and rate-limited attempts cannot supply successful dry rounds. Preserve the distinction between exhausted allowance, taper completion and explicit closure.

Closing a channel affects only that PR/channel. Reopening explicitly grants a new allowance. Record actor attribution and policy revision; retain consumed history when settings change. No implicit project-wide closure or reopening follows from a local change.

Head or parent movement preserves allowance history but cannot grant old capture coverage for changed code without proven applicability. Closing, tapering or exhausting a channel does not erase findings, satisfy accepted/routed obligations, establish current CI or authorize a merge. Current comparison coverage and observed merge readiness remain separate assessments.

## Migration boundary and proof

Carry effective current round limits, minimum/exact requirements, taper state and channel closure into the new operational policy with explicit provenance. Retain original records privately; rebuilding commands for retrospective allocation/credit-period manipulation is outside the adoption requirement. A converter must report unsupported live behavior rather than silently substitute a different rule.

Map source behavior tests to functional outcomes before implementation: additional allowance baselines, minimum/exact requests, eligible dry streaks and reset, closure/reopen isolation, independent queue advancement, head/parent movement, exhausted-versus-taper status and outstanding findings. Test observable scheduling and evidence results rather than copying legacy storage or command internals. Imported historical settings must not manufacture native captured review credit.
