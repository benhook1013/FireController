# Hosted Capacity Settlement

## Implementation status

This contract adds an explicit administrator action for a hosted attempt whose bounded observation journal is full. The administrator HTTP action is implemented. Focused public-route normal/race tests, vet and formatting pass; full combined validation and actual packaged proof are pending. Automatic recovery keeps uncertain work reserved; reaching a limit is not evidence that the external action stopped.

## Operator responsibility

Before settlement, an administrator independently verifies that the original external action has ended or has been stopped and that no predecessor or other actor will continue it. Identify the original repository, PR, candidate and issued command, and retain an evidence reference. Timeout, an empty observation, a moved head, exhausted capacity, a quota reset or a stopped local daemon does not establish external termination.

The service records this trusted operator assertion; it does not independently verify the referenced external evidence, cancel the provider action or declare a review complete. Ordinary project write permission is insufficient. This recovery control is deliberately absent from routine worker CLI/MCP tools and the read-only browser. An administrator can use an HTTP client with protected credential/header and request files against the operator API. Do not put credentials in tracked files or shell argument values.

## Guarded API

`POST /api/v1/projects/{project_id}/reviews/attempts/{attempt_id}/settlement` requires current administrator authorization and an explicit idempotency key. The body requires every field below; null, duplicate and unknown fields are refused.

| Field | Requirement |
| --- | --- |
| `expected_revision` | The current attempt revision, one through `MaxInt64-1`. |
| `expected_repository_id` | The positive numeric repository identity in the admitted anchor. |
| `expected_head_sha` | The exact admitted lowercase candidate SHA. |
| `expected_external_id` | The exact retained external ID, including an explicit empty string when unknown. |
| `external_action_stopped` | Explicit true, asserting the operator completed the external verification above. |
| `reason` | Nonblank UTF-8 operator explanation, at most 1024 bytes; newline/tab allowed, other controls refused. |
| `evidence_reference` | Nonblank UTF-8 reference to the operator's verification, at most 1024 bytes, without control characters. It is retained data, never fetched or executed. |

The complete canonical JSON audit payload, including the original target and operator explanation/reference, must fit the existing 4096-byte event budget. Individual text limits do not override this aggregate encoded limit: escaping can make otherwise bounded strings too large. Oversize audit content returns HTTP 400 `settlement_audit_too_large` before any write and asks the operator to shorten the explanation or reference; valid text is never truncated.

The selected operation and attempt must be consistently linked, hosted and uncertain, with a valid admitted anchor/intent, a full 200-reference journal, no retained final evidence and the exact original account/target reservation. The operation reason need not already say `hosted_observation_capacity`: an attempt that was uncertain before its journal filled retains its earlier reason. Live/ordinary pending work, another project, an incomplete journal, a retained final capture, a changed target, a mismatched reservation or malformed state cannot be settled through this action. Existing final evidence belongs to ordinary observation-based recovery.

A valid action atomically advances the local attempt and operation to `cancelled`, records `operator_verified_external_stop`, appends the normal transition, releases only this attempt's exact reservation and writes one administrator-attributed audit event plus a compact replay receipt. The event retains the operator reason/reference and the original target, external ID and guarded revision. Original observations, capture references, evidence and historical intent remain untouched. This local cancellation abandons recovery after the operator's verified stop; it does not send cancellation or another review command.

The HTTP 200 receipt contains `attempt_id`, `operation_id`, `revision`, `state:"cancelled"` and `outcome:"settled"`. It grants no completed review, findings, zero count, coverage or merge authority. It does not enqueue replacement work, alter configuration or clear a restored instance's provider hold. A later explicit review request still passes normal current admission and provider-activity checks.

A stale guard returns HTTP 409 `review_attempt_revision_conflict` with `details.current_revision`; target/state/journal/final-evidence/reservation mismatches return `review_settlement_unavailable`. Missing resources and authorization use existing scoped codes. Current administrator authority is rechecked inside the mutation transaction and on replay. Exact-key retries return the original receipt without a second audit, transition or reservation change; changed arguments conflict. A failure in any write rolls back every effect. Revision overflow is refused. Wall-clock corrections use existing attempt-recording chronology without altering provider times.

## Proof boundary

Required proof uses public HTTP controls and actual retained capacity-bound attempts. Verify administrator-only access, revocation, instance/project isolation, explicit field/target/guard validation, exact replay after later work and restart, concurrent operator/completion guards, event-failure rollback, unchanged original observations/intent and no completed credit or provider work. Prove a different account/target reservation remains occupied and normal subsequent admission still checks provider activity. Packaged service proof must exercise the real route before provider review. The operator's external verification is a trust prerequisite; tests must distinguish a controlled fixture's known stopped action from the service's recording of that assertion. This does not implement general restore reconciliation, automatic expiry or provider cancellation.
