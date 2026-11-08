# Source preparation rate-limit recovery

## Implementation boundary

The provider error now carries additive `RetryTiming` evidence independently of the legacy `RetryAfterSeconds` hint. This does not change retry behavior. The legacy hint still rounds and caps values for existing callers; it must not be interpreted as an exact provider floor. New evidence records the response classification timestamp in `ObservedAt`, an absolute `NotBefore` only for `proven` timing, and an explicit `unknown` or `unsupported` state otherwise. Its zero value is not proof. Consumers use the absolute floor, never restart its duration from a later observation or restore time. Valid expired floors do not prove current availability.

The parser preserves fractional-second relative deadlines and exact HTTP-date deadlines. When both valid Retry-After and exhausted-quota reset headers supply floors, the later floor wins. Ambiguous or malformed headers remain unknown. Delays beyond the supported 24-hour window are unsupported, not shortened into proven timing. Evidence contains no raw header, response body, credential, or account identity. Timing never establishes authorization, a read-only request, or a known mutation outcome.

Durable automatic source-preparation retry remains unimplemented. The existing execution contract in [Native review execution](review-execution.md) continues to apply. Historical terminal holds remain terminal.

## Dedicated implementation slice

A future source wait must retain new eligible read-only preparation failures before any provider action. Eligibility requires the maintained source-resolution stage, typed rate-limit classification, a known rejected read, no intent or external identity, and unchanged operation ownership. Authentication failures and unknown mutation outcomes are ineligible. A source-refresh error after other preparation work needs separate proof and is not implicitly eligible.

Retain an immutable failure observation and a bounded retry count with the original operation, configuration, actor, worker, capability and source selection. Proven timing is a minimum, not an availability estimate. Local backoff may delay further but never shorten it. Missing or unsupported timing must remain an explicit conservative hold unless a separately specified finite local backoff policy is implemented. A due retry performs complete fresh source resolution and normal authorization, source comparison and final admission. It grants no new review round and never reposts an uncertain action.

Source throttling is distinct from CodeRabbit allowance. Capabilities' configured review account names do not prove GitHub principal identity. A shared internal limiter must group identical GitHub credentials or independently verified GitHub principals across applicable capabilities. Credential-derived grouping must not become a public identifier. Different credentials for one principal cannot be assumed independent capacity, and no path may rotate credentials to evade a rate limit. Discovery and observation consumers need coordinated throttling so another lane does not defeat the preparation wait.

## Compatibility and backup gates

Existing operation wait rows bind CLI zero-allowance or Hosted cooldown evidence. Their readers, retry-baseline reconstruction, historical attempt validation and backup restore reject detached source-only attempts. Do not reuse that representation without explicit versioned semantics.

Prefer a distinct source-wait evidence domain, with atomic operation/attempt settlement and reservation release. Include domain initialization, exact backup inventory, foreign-key validation, corruption rejection and restore proof. If attempts detach, update all historical attempt readers and projections intentionally. Negotiate any changed client wait shape without mislabeling source throttling as allowance exhaustion. Updated service, backup tools and helper readers must be rolled out together; old-reader behavior must be tested before deployment. No migration may convert historical terminal requests into active work.

## Required proof

- Exact floor and observation timestamp survive restart and backup/restore without resetting a relative duration.
- No provider read before the retained floor, including sibling capabilities sharing credentials.
- Finite retry exhaustion retains an actionable terminal notice; ordinary scheduled waits do not emit false stopped notices.
- Missing, malformed, duplicated, too-long and expired timing have explicit outcomes.
- Revoked authority, cancellation, paused requests, changed configuration and moved source heads prevent dispatch.
- Concurrent schedulers and lost persistence responses cannot duplicate preparations or provider actions.
- Authentication failures, ambiguous mutations and historical terminal operations never acquire automatic retry.
- Independent quota waits, source baselines, round controls and idempotent request replay retain their existing meanings.
