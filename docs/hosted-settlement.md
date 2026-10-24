# Ended Review Settlement

## Implementation status

This contract provides one explicit administrator action for three narrowly defined uncertain review cases: a full Hosted observation journal, a Hosted finished-command acknowledgment with unproven inventory, or a retained partial CLI result. The route settles only local state after independent operator verification of external termination. The original Hosted capacity path has the full combined and packaged proof recorded below; the two additional cases have focused public-route normal/race proof, while their actual packaged qualification remains separate. Automatic recovery keeps uncertain work reserved; a diagnostic or limit alone is not evidence that the external action stopped.

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

The selected operation and attempt must be consistently linked and uncertain, with their original valid admission, anchor, current transition, exact guarded target and original provider/account/channel/target reservation. The selected classes are:

| Class | Required retained authority | Local result |
| --- | --- | --- |
| Hosted observation capacity | Canonical fixed Hosted trigger intent, a full 200-reference bound observation journal, and no retained final evidence. The current reason may predate the capacity limit. | `cancelled`, reason `operator_verified_external_stop`. |
| Hosted finished inventory unproven | Exact current reason `hosted_finished_inventory_unproven`, the canonical fixed trigger intent, at least one bound ordinary review observation in the canonical journal, and no retained final evidence. The 200-reference minimum does not apply. | `cancelled`, reason `operator_verified_external_stop`. |
| Partial CLI result | Exact current reason `coverage_incomplete`, empty CLI intent, and one canonical non-complete CLI evidence record whose indexed identities, original capability fingerprint, original anchor, capture identity/digest and retained input fingerprint agree. | `failed`, reason `cli_partial_settled`. |

The Hosted acknowledgment does not establish zero findings or completed coverage. The operator still independently verifies the external action ended; the service does not fetch or reinterpret the referenced raw capture during settlement. A partial CLI capture may retain findings, decisions, routes and fix proof. Settlement preserves these records and does not replace or finalize the immutable source. Live/ordinary pending work, another project, arbitrary uncertainty, missing or inconsistent source records, a changed guarded target, a mismatched reservation or malformed state cannot use this action. Retained complete final evidence belongs to ordinary observation-based recovery.

A valid action atomically advances the local attempt and operation to the selected terminal state and reason, appends the normal transition and native attempt projection, releases only this attempt's exact reservation and writes one administrator-attributed audit event plus a compact replay receipt. Hosted actions retain the existing `review.hosted_settled` event; CLI partial settlement records `review.cli_settled`. The event retains the operator reason/reference and the original target, external ID and guarded revision. Original observations, capture references, evidence and historical intent remain untouched. This local settlement abandons recovery after the operator's verified stop; it does not send cancellation or another review command.

The HTTP 200 receipt contains `attempt_id`, `operation_id`, `revision`, the selected `state` (`cancelled` or `failed`) and `outcome:"settled"`. It grants no completed review, findings, zero count, coverage, round usage, taper progress or merge authority. It does not enqueue replacement work, alter configuration or clear a restored instance's provider hold. A later explicit review request still passes normal current admission and provider-activity checks.

A stale guard returns HTTP 409 `review_attempt_revision_conflict` with `details.current_revision`; target/state/journal/final-evidence/reservation mismatches return `review_settlement_unavailable`. Missing resources and authorization use existing scoped codes. Current administrator authority is rechecked inside the mutation transaction and on replay. Exact-key retries return the original receipt without a second audit, transition or reservation change; changed arguments conflict. The unchanged endpoint retains the historical opaque mutation identity `reviews.hosted.settle` for both channels, so existing receipts remain replayable without a separate pre-read or authority path. A failure in any write rolls back every effect. Revision overflow is refused. Wall-clock corrections use existing attempt-recording chronology without altering provider times.

## Proof boundary

Required proof uses public HTTP controls and actual retained capacity-bound attempts, bound Hosted finished-inventory observations and partial CLI captures with real retained findings and controls. Verify administrator-only access, revocation, instance/project isolation, explicit field/target/guard validation, exact replay after later work and restart, concurrent operator/completion guards, event-failure rollback, unchanged original observations/intent and no completed credit or provider work. Prove a different account/target reservation remains occupied and normal subsequent admission still checks provider activity. For partial CLI recovery, prove all original findings/decisions/routes/capture bytes survive, the private attempt projection becomes failed with `cli_partial_settled`, and a later explicit request may dispatch through its own ordinary admission without gaining any credit from settlement. Packaged service proof must exercise the real route before provider review. The operator's external verification is a trust prerequisite; tests must distinguish a controlled fixture's known stopped action from the service's recording of that assertion. This does not implement general restore reconciliation, automatic expiry or provider cancellation.

## Executed proof

Clean source `d5c624c9f50301901b40c2a1cc585c3a2c312fb5` passed full `make check build`: 128 browser tests, all Go race tests and vet, formatting, documentation and 19 link tests, embedded assets and all three release binaries. Focused route tests cover current administrator authority and revocation, strict fields and target guards, exact 4096-byte audit acceptance and 4097-byte refusal, a different reservation, concurrent completion/operator actions, event-failure rollback and replay after later work/restart. They establish no external stop verification by the service.

Two actual packaged groups passed on their first run in 13.862 seconds. A controlled provider accepted the original review POST and lost its response. With the daemon stopped, actual recovery calls through the real provider adapter and capture store filled all 200 journal slots (one allowance query and 199 review observations); the next capacity pass preserved uncertainty and its reservation without provider work. A separate operator observation then verified the original controlled action's terminal publication without retaining final evidence. The restarted daemon refused seven public-route controls, accepted the explicit administrator assertion, and preserved all original observations, intent and artifact bytes. A damaged response after commit recovered by the original key, including after another restart, with one audit and reservation release, no provider calls during settlement/replay and no completed review credit. The tested `fc` and `fcd` binaries match the full-build artifacts.

The same run proved direct and lost-response query confirmation at the exact five-second provider-clock lower bound, preserving original timestamps and avoiding extra query/review posts on recovery replay. Greater-than-bound, actor/body/edit and future-time refusals have focused normal/race proof. The expensive controlled journal fill is setup, not a production latency claim; the actual settlement request took 68.206 ms on this developer machine. All owned fixture processes and state were removed. These checks do not establish real-provider stop verification, restored-provider resumption, fleet throughput or operational cutover.
