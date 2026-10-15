# Public Review Checkpoints

## Implementation status

The API, CLI, MCP controls, private publication journal, bounded provider worker and observation-only recovery are implemented. Full combined checks and six clean packaged controlled-provider groups pass. Native source finalization, local browser publication and observed readiness are independent; a local `PublishEvidence` call is not a GitHub comment.

## Source and authority

An explicit publication selects one complete, finalized native evidence record in the bound project. It publishes the original frozen found/accepted/routed counts and original captured head. It does not publish effective finding text, mutable fix proof, receiver state, private project/worker identifiers, credentials or paths. Corrections and independently revised receiving routes do not rewrite these source counts. The visible summary labels them historical source counts rather than current coverage or merge readiness.

Publication is separate from source finalization and never requests a review, queries allowance, runs Git, refreshes source, acknowledges a message or authorizes a merge. A moved or merged PR does not invalidate honest historical counts. The destination remains the source's stable repository ID and PR number. Repository rename resolution uses that stable ID.

A publication freezes the currently configured GitHub capability/account and the selected source semantic identity. It does not require the original review executor to remain installed. The original evidence keeps its own provenance. The last posting guard rechecks current project write permission, stable repository identity, publication capability binding and unchanged source semantics. Execution pauses retain their admission-only meaning. A changed publication binding holds rather than silently adopting another account.

## Durable intent and one posting opportunity

A first request requires `evidence_id`, the current source adjudication `expected_revision` and an explicit idempotency key. The service validates the trusted complete capture and frozen counts transactionally, then records a durable publication before any provider work. One semantic publication is retained for an evidence/capture/frozen-count identity, independently of caller keys. Exact-key replay preserves the original action. Another key cannot create another comment for a published or uncertain semantic publication.

Only a successful durable transition grants a posting opportunity. Its intent includes exact payload, source identity, configured capability/account, stable provider actor ID/kind and resolved repository destination. Concurrent workers and restart cannot grant another opportunity after an uncertain post. Provider calls run outside database transactions and share a finite fifteen-second action budget. Queue capacity is bounded to 64 active publications per instance and sixteen per project.

States distinguish `queued`, `preparing`, `posting`, `recover_queued`, `recovering`, `published`, `held` and `uncertain`. Startup can repeat interrupted preparation or recovery reads; a retained posting intent enters observation-only recovery. Uncertain transport or failed local confirmation never becomes proof that no comment was posted. A provider success followed by local storage failure remains recoverable from its retained intent and provider history.

## Payload and recovery

The deterministic visible summary starts with `CLI` or `Hosted`, original found/accepted/routed counts and captured head. A retained file count or duration may be included only when known. An explicit historical-count sentence keeps source counts separate from current policy/readiness. A hidden version-one FireController marker binds an opaque `review_publication_` identity and semantic SHA-256. The digest binds immutable source evidence/capture/comparison/count identity and optional known display facts; it is not a merge certificate.

Recovery uses complete, bounded comment history for the exact repository/PR and the original stable provider actor ID/kind. Login changes alone do not change identity. The original intent stays immutable across repository renames; a confirmed recovery link uses the freshly resolved stable-ID destination. Provider timestamps are validated independently of the local host clock. Only defined neutral whitespace changes may preserve the payload: all marker tokens and visible semantic content must still match. Conflicting edits or multiple attributable matches hold. An unrelated author's copy is not attribution. Zero matches after an uncertain post remain uncertain; observation never blindly reposts.

Explicit recovery requires the current publication revision and a key. By default it schedules observation-only recovery. Optional `retry_unposted: true` may schedule a posting attempt only when the durable record positively proves no provider write occurred, such as interrupted preparation or a known transport refusal. It never resets an uncertain posting intent. An explicit safe retry may select the current configured capability with fresh authorization, retaining immutable earlier intents and the same source semantic publication. No automatic retries or replacement keys are generated.

## Bound controls

`POST /api/v1/projects/{project_id}/reviews/publications` accepts the guarded source selection. `GET /api/v1/projects/{project_id}/reviews/publications/{publication_id}` reads one stored operation. Relative `POST /recovery` requires `expected_revision` and optional non-null `retry_unposted` (default false). Reads accept no query parameters and perform no provider work.

Publication and recovery requests wait briefly for a useful terminal result, using the same default two-second wait and optional `Prefer: wait=N` range zero through sixty as source refresh. If work is still pending, HTTP 202 returns its durable queued receipt. Completed terminal outcomes use HTTP 200. Caller cancellation releases only its result wait, preserving the accepted action. Results are reauthorized before return.

The compact `CheckpointPublication` response contains `id`, `project_id`, `evidence_id`, `state`, `revision`, `reason`, `source_revision`, `anchor`, `counts`, `created_at`, `updated_at`, `comment_id`, `comment_url`, `published_at`, `provider_author_id` and `provider_author_kind`. Unknown public confirmation fields are explicit zero/empty values, not invented facts. Internal payloads, provider diagnostics and histories remain private; the public operation is bounded to 16 KiB.

The CLI commands are `reviews publish EVIDENCE_ID --expect-revision N --key KEY`, `reviews publication PUBLICATION_ID` and `reviews publication-recover PUBLICATION_ID --expect-revision N --key KEY [--retry-unposted]`. Mutations accept the optional `--wait-seconds` preference. MCP exposes `review_checkpoint_publish`, `review_checkpoint_read` and `review_checkpoint_recover`, with equivalent named fields and optional `wait_seconds`. Private profiles bind the project and worker. Only explicit publication/recovery authorize external work; routine reads remain local and compact.

## Functional proof

Prove strict render/match identity and bounds; malformed, conflicting, copied and duplicate comments; lost successful post responses and malformed success bodies; concurrent/key replay and source revision guards; immutable source counts despite later corrections/receiver changes; access/capability/repository changes at the final action boundary; transaction/event/receipt rollback; restart around intent/post/confirmation; no blind retry and separately authorized proven-unposted retry. Actual packaged API/CLI/official-SDK checks must demonstrate one comment after uncertain recovery, quiet local reads, responsive coordination and retained captures/imported history. Live provider operation and operational cutover remain separate proof.

## Executed proof and limits

Clean runtime checkpoint `bdc820034026138d2f78d11363d5e76a4f363f7e` passed full `make check build`: 175 browser tests, all Go race tests and vet, documentation/link checks, embedded assets and three binaries. Focused tests prove durable transition/receipt/event rollback, original source counts across corrections and receiving proof, final authorization/binding guards, restart, unknown versus known-refused posts, caller cancellation and wait capacity. The per-project capacity test creates sixteen real active publications; the instance-wide sixty-four limit is implemented but was not populated with sixty-four real sources in this proof.

Six actual packaged API/CLI/official-SDK groups passed in 11.416 seconds against an isolated TLS provider fixture with genuine privately seeded finalized native evidence. They used 26 actual `fc` invocations and packaged `fcmcp`, including discovery of all 45 tools. A default quick publication returned useful facts in 28.392 ms; immediate pending acceptance took 10.377 ms. While provider preparation was blocked, jobs and inbox reads took 4.961 and 4.816 ms. Original-key longer waiting completed the same action in 162.295 ms. These are small local samples, not supported latency budgets.

A committed POST with a lost response was recovered after daemon restart and repository rename with exactly one comment and one POST. The confirmed link used the new stable-ID destination and the original intent stayed unchanged. Malformed success, conflicting edits, duplicate attributable comments and foreign-author copies did not grant another write; neutral whitespace and login renames retained identity. A known refusal needed explicit proven-unposted retry. Capability changes, credential revocation and a wrong stable repository response stopped posting. Captures, evidence, imported history and original source counts remained intact; reads/replays did no provider work.

The fixture made 27 controlled provider GETs and six POST attempts: five genuine checkpoint comments and one known refusal. It made zero real external calls, Git or CLI-review invocations, allowance queries or source/CI refreshes. Source and binary hashes stayed unchanged; owned processes, listeners and disposable state were cleaned. The source manifest was SHA-256 `21cde956da90b7580fc06a777cd20b61de520d64ccbec496881f7be7bb87f3a5`; packaged `fcd` was `8a28b3b84ad142cc52c4a6795ae8d465cd66944f2cfb5d7f9fb7b3f62f16a3b5`.

This proves controlled local integration, not live GitHub/CodeRabbit execution, browser or physical-phone behavior, fleet throughput, remote deployment or operational cutover. A local project-repository binding change has focused core proof; there is no public repository-rebinding administration workflow claimed by the packaged pass. Independent source inspection identified and corrected the stale recovery-link issue before the packaged run. Provider/local clock-skew acceptance has focused positive tests without assuming synchronized clocks.
