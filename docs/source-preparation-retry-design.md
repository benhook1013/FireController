# Source preparation rate-limit recovery

## Provider timing evidence

The provider error carries additive `RetryTiming` evidence independently of the legacy rounded and capped `RetryAfterSeconds` hint. Evidence records the response classification timestamp in `ObservedAt`, an absolute `NotBefore` only for `proven` timing, and an explicit `unknown` or `unsupported` state otherwise. Its zero value is not proof. Consumers use the absolute floor, never restart its duration from a later observation or restore time. Valid expired floors do not prove current availability.

The parser preserves fractional-second relative deadlines and exact HTTP-date deadlines. When both valid Retry-After and exhausted-quota reset headers supply floors, the later floor wins. Ambiguous or malformed headers remain unknown. Delays beyond the supported 24-hour window are unsupported, not shortened into proven timing. Evidence contains no raw header, response body, credential, or account identity. Timing never establishes authorization, a read-only request, or a known mutation outcome. For a GitHub rate-limited read with a proven exhausted primary bucket, use a five-second margin after the whole-second reset. Keep any later provider Retry-After and do not renew an already expired margin.

## Retained source waits

A new CLI `source_resolution` or Hosted `source_resolution`/`source_refresh` preparation failure with a typed rate-limit HTTP 429 or 403 and no possible side effect is eligible. This includes the first Hosted preparation, without a fabricated CodeRabbit cooldown baseline. A proven future floor within 24 hours is preserved exactly. A qualified refusal explicitly classified with unknown timing and an observation timestamp from this preparation instead retains a conservative 15-minute backoff; it is not a provider reset or availability promise. The observed timestamp must belong to this preparation and precede its settlement. Missing observation timestamps, expired or unsupported deadlines, malformed timing, authentication failures and other preparation stages retain ordinary terminal behavior. Historical terminal operations are never reopened.

The service atomically ends the unused preparation, retains its original failure observation, releases its reservation and keeps the same operation queued. It permits at most three such retries per operation. The fourth eligible refusal becomes an actionable terminal hold. Waiting emits no false stopped notice. An exact settlement replay cannot add another retry or reset the clock.

A due retry starts fresh preparation under the original operation, configuration, actor, worker and source selection. All normal authority, current-head, configuration, findings, round-control and final-admission checks remain mandatory. No source wait supplies a comparison anchor, allowance, review credit or permission to repeat a provider action. Pause, resume and cancellation use existing undispatched-operation controls; resume never shortens a retained floor.

Source throttling is distinct from CodeRabbit allowance. Production composition groups only identical GitHub credentials using private hashes that never enter client responses. Every new REST or GraphQL read through those adapters consults the retained shared retry deadline, including observation lanes and sibling capabilities. Already in-flight reads are not cancelled. Different credentials for one principal are not assumed independent quota, but this slice does not discover principal equivalence and never rotates credentials. An inherited local gate refusal predating another preparation does not masquerade as that preparation's new provider observation.

Candidate discovery filters future shared floors before its bounded candidate window. It explicitly uses the deadline index, because SQLite may otherwise prefer a whole-history group scan for distinct credential groups. A missing index fails discovery closed instead of silently removing the bound; exact per-operation admission still checks nanosecond deadlines.

## Compatibility and backup gates

Version 2 of the separate `review_source_retries` domain records whether each deadline is proven or conservative and retains immutable per-attempt observations and a contiguous sequence bounded at three. Existing operation wait rows continue to mean CLI zero allowance or Hosted cooldown; source waits never become quota baselines. Historical attempt projection and validation recognize the new unused preparations without fabricating preflight evidence.

Wait protocol version 4 extends version 2 with `source_retry`, including the original attempt, observation time, absolute retry floor and sequence. It does not expose the private credential group or quota estimates. Earlier clients receive their existing wait shape and truthful queued state. Source timing is not placed in the existing Hosted-only compact cooldown field.

Wait protocol version 6 combines version 3 retained `cli_allowance` estimates with version 4 `source_retry` evidence. Version-6 clients validate each optional field against its original channel, state and baseline. Neither field grants permission or turns an estimate into a retry clock. Version 6 omits version 5 queue-priority hints; the browser continues to request version 5. Versions 1–5 keep their original projections, and omitted optional facts remain absent when reading an older service. Strict unknown-field and malformed-value rejection remains unchanged.

The bound client now requests wait protocol version 7, which extends version 6 with Hosted source-retry evidence, including initial preparations without a baseline. Older Hosted clients receive only a truthful queued/pause shape for an initial source wait; existing baseline-backed waits retain their prior effective-deadline projection. A retry deadline is an earliest recheck, not evidence of a provider reset.

Matching backup helpers validate the new domain's inventory, ownership, unused-attempt history, clock ordering, retry bounds and absence of execution evidence. Export and inactive restore preserve original rows and timestamps. Older backup helpers must reject the unknown domain. Roll out the matching service and backup helpers together; do not bypass an older helper's compatibility refusal. New helpers continue to inspect prior snapshots without migrating them. Restore reconciliation validates source waits before abandoning queued work, and never authorizes provider release by itself.

## Proof boundary

Focused tests cover exact timing, bounded settlement and replay, restart clocks, shared credentials, pause/cancel, authority and source guards, no early provider calls, typed client compatibility and backup corruption/restore. Test execution and independent review are required before rollout. This slice does not claim production automatic-resumption proof, and the existing [native review execution](review-execution.md) dispatch and authorization invariants remain in force.
