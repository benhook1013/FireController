# Native Review Evidence Controls

## Boundary

The service exposes retained native CLI and hosted evidence and its decision/fix ledger through project-scoped API, CLI and MCP operations. These controls use the same authorization and private client binding as jobs and messages. They do not start a review, upload a capture or refresh GitHub. Finalizing complete source adjudication automatically queues its compact historical GitHub summary; delivery runs in the background and does not require fixes. See [review summary delivery](review-publication.md). The [execution contract](review-execution.md) owns trusted capture ingestion and provider admission; the [review workspace](review-workspace.md) owns stored browser projections.

The daemon installs the execution/evidence/adjudication and project-policy schemas and registers the local routes. Its default capability set is empty; an explicitly selected private startup catalog enables CLI, hosted or both pipelines described in the execution contract. Adding these controls does not complete native review cutover or change the separately running development executor.

## Explicit reads and stable finding identity

`GET /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}` returns one retained parsed capture with its project, operation/attempt identity, capability fingerprint, channel, exact source anchor, capture checksum/size/timing, outcome, completeness, reviewed paths and findings. All fields use snake_case. It excludes raw stdout/stderr, credentials, operator paths and the duplicate anchor inside the stored ingestion input. This full-detail read is deliberate; ordinary review queue reads do not acquire its finding text.

The channel is explicitly `cli` or `hosted`. Hosted captures additionally expose `hosted` publication metadata: `publication_id`, `provider_attempt_id`, `trigger_comment_id`, `review_ids` and `observed_base_sha`. Each hosted finding has a `source` object containing `source_key`, `fingerprint`, `kind`, `side`, `review_id` and `comment_id`; kinds distinguish `inline`, `outside_diff`, `duplicate` and `additional` source observations. Additional-comment identities are derived from the real attributed review, location and original detail bytes because that provider format has no fingerprint marker; they do not claim a provider inline-comment identity or a verified finding. Standing-summary zero-result evidence instead adds `standing_summary` with `comment_id`, `provider_run_id` and `updated_at`; `publication_id` and `provider_attempt_id` are empty, and `review_ids` and findings are explicit empty arrays. Its real comment identity is retained without inventing a review object. A supported single positive submitted review instead adds `submitted_review` with `provider_run_id` and `acknowledgement_comment_id`, retains its one real GitHub review ID in `review_ids`, and leaves `publication_id` and `provider_attempt_id` empty. It cannot stand in for zero-result or batched evidence. Clients may read a new bounded non-inline provenance label when it retains the same review-bound source key and location contract; service ingestion support remains explicit. These identities describe retained provider evidence, not current review sufficiency. CLI responses omit the hosted objects. Hosted captures do not fabricate a reviewed-file list. Both channels use the same decision, finalization and fix controls below.

Each finding has an immutable, zero-based `finding` index within that evidence record. Select the index returned by the API; do not derive it from a browser row number or an earlier executor's numbering. Evidence IDs use `review_evidence_` followed by 32 lowercase hexadecimal characters. Browser projection run IDs are separate identities and cannot select this ledger.

Retained validation checks immutable structure and binding, rather than comparing historical timestamps with the current wall clock. Clock rollback cannot invalidate evidence or hosted-observation reads, exact capture/observation replay, adjudication, publication, coverage or the retained hosted recovery boundary. A genuinely new capture still rejects a finish time more than five seconds in the future at ingestion; changed replay remains a conflict.

`GET` on the same path plus `/adjudication` returns the current revision, finalized flag, original frozen counts when present and current per-finding decisions/fix references. It does not repeat the source finding text. Both reads require current project read permission, enforce instance binding and perform no domain mutation or provider work. Full responses are bounded to 4 MiB by the typed clients; service-error responses retain their separate 64 KiB limit.

## Guarded writes

Each write requires project write permission, an explicit `Idempotency-Key`, and the current adjudication `expected_revision`. Initial state has revision zero. Successful writes return only `{id,state,revision}` with HTTP 200. Preserve the exact key, request and binding if a response is lost; never generate another key or automatically retry an uncertain mutation.

| Relative POST path | Required JSON fields | Receipt state |
| --- | --- | --- |
| `/decisions` | `expected_revision`, `finding`, `disposition`, `reason` | `adjudicated` |
| `/fixes` | `expected_revision`, `finding`, `expected_sha`, `fix_sha`, `proof_note` | `fix_recorded` |
| `/finalization` | `expected_revision` | `finalized` |

Fields are explicit and non-null, including finding zero and an empty `expected_sha` for the first fix reference. Unknown fields and query parameters are rejected. Disposition is `accepted` or `rejected`; [finding routes](review-routes.md) separately create a routed disposition with receiving authority. Replacing an existing routed decision also requires its `expected_route_revision` guard. Reasons and proof notes are bounded to 2048 UTF-8 bytes and follow the ledger's safe-text rules. Commit references are lowercase 40-character SHAs. `expected_sha` guards a correction against the current effective fix reference; it does not cause a provider lookup or independently verify that a commit proves the finding fixed.

Finalization requires a completed attempt, complete retained capture and a decision for every finding. It freezes the original source counts, including a valid zero-finding run. Later decision/fix corrections append audit records and retain the original capture and frozen counts. An accepted finding with a fix reference cannot be rejected silently. Finalization establishes source counts; policy sufficiency, fixed findings and merge readiness remain separate facts. Humans perform merges.

## Browser consistency

When a native run has already been published to the stored review workspace, a decision, fix reference or finalization updates its browser projection in the same transaction as the authoritative ledger, audit, event and receipt. A failed projection leaves all of them unchanged. Native publication and corrections must fit the browser's encoded 4 MiB run-detail response budget, including JSON escaping and its final newline. An oversized result returns `workspace_capacity` before commit; source text and history are never truncated. Shorter decision or proof text can fit without replacing captured evidence. The browser shows the new observation on refresh; these controls do not make provider requests.

The native run's original source identity and zero-based finding indexes bind that update. It preserves captured finding text, historical snapshots, imported runs, merged queue history and current PR facts. Finalized source counts remain the original counts even when a later audited correction changes an effective decision or fix. Updating a decision does not claim a fresh provider observation for other PRs.

Evidence without an initial native workspace publication remains usable through the ledger controls. A decision does not invent a browser run or convert imported historical evidence into native execution. The optional configured CLI and hosted drivers perform trusted initial publication from retained evidence, as described in the [execution contract](review-execution.md#evidence-and-the-native-workspace); real CodeRabbit execution through those drivers and operational cutover remain unproven.

## Atomic selected-finding batches

`POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/decisions/batch` and `/fixes/batch` apply one explicitly selected batch under project write authority. Each request has one `expected_revision`, one `Idempotency-Key`, and an `items` array containing 1–100 unique zero-based finding indexes. Unselected findings are unchanged. The complete strict JSON request is bounded to 2 MiB, including escaping; unknown fields, duplicate keys, null required fields and duplicate selected indexes are refused.

A decision item contains explicit `finding`, `disposition` (`accepted` or `rejected`) and nonblank `reason` of at most 2048 UTF-8 bytes. Optional `expected_route_revision` has the same semantics as a single decision: correcting a routed source finding requires its current route revision and cannot supersede a route that already has receiving fix proof. This endpoint does not create routes or exercise receiving-PR decision authority. A fix item contains explicit `finding`, `expected_sha`, `fix_sha` and nonblank `proof_note` of at most 2048 UTF-8 bytes. `expected_sha` is required even when empty; it asserts the currently recorded source fix. The finding must already be accepted. Receiving fixes continue to use the receiving-route controls.

All items share one transaction, one adjudication revision, one audit history record and one compact receipt. Browser projection, route supersession, source policy facts, events and the receipt commit together. A bad later entry, stale adjudication/route/SHA guard, changed authorization or projection/event failure leaves no partial batch. Successful decision receipts are `{id,state:"adjudicated_batch",revision}`; fix receipts use `state:"fixes_recorded_batch"`. Audit history records the complete explicit selected input with finding index `-1`, rather than producing a separate adjudication revision for each item. Route supersession retains its own existing route history within the same transaction.

Batching never changes immutable captures, original finalized counts, earned rounds or taper accounting, and it never finalizes source implicitly. Later corrections preserve frozen source counts as single actions do. Use the existing explicit finalization endpoint when every source finding has a decision. Exact-key replay returns the original compact receipt under current authorization, including after later revisions; changed input under that key conflicts. A lost mutation response requires recovery with that same batch and key. Neither the service nor clients automatically split, retry, infer decisions, supply a shared default reason or accept all findings.

The bound CLI reads a private regular UTF-8 file and validates the complete request before loading its connection or contacting the service:

```sh
fc reviews decide-batch EVIDENCE_ID --file /private/decisions.json --key DECISION_KEY
fc reviews fix-batch EVIDENCE_ID --file /private/fixes.json --key FIX_KEY
```

The decision file is, for example, `{"expected_revision":0,"items":[{"finding":0,"disposition":"accepted","reason":"Verified against source and focused proof."},{"finding":2,"disposition":"rejected","reason":"The stated precondition cannot occur."}]}`. A fix file contains the same envelope with explicit fix items, such as `{"finding":0,"expected_sha":"","fix_sha":"0123456789abcdef0123456789abcdef01234567","proof_note":"Focused test and immutable source reference."}`. Use actual returned revision/index values and a verified fix commit. The adapters emit only the one receipt.

MCP exposes `review_findings_decide` and `review_findings_fix` with `evidence_id`, `idempotency_key`, `expected_revision` and the corresponding explicit bounded `items` array. Argument validation runs before credentials/network work. These tools retain the existing five-second command budget and unknown-outcome recovery rules. Single-finding controls remain available and use the same action validation and transaction-level guards.

## Explicit project policy

`GET /api/v1/projects/{project_id}/reviews/policy` requires project read permission and returns `{project_id,revision,actor_id,recorded_at,policy}`. An unconfigured policy returns `reviews_policy_unconfigured` with HTTP 409; absence never means that review requirements passed.

`POST` to that path requires administrator permission, an explicit idempotency key and three non-null JSON fields: `expected_revision`, `required_channels` and `max_review_files`. Initial revision is zero. Required channels are an explicit nonempty unique subset of `cli` and `hosted`. `max_review_files` is zero for no additional project ceiling, or 1–100000 for a project ceiling. It neither asserts a provider's actual limit nor bypasses one. Unknown fields and query parameters are rejected.

Successful configuration returns `{id,state:"policy_configured",revision}` with HTTP 200. Changes retain immutable administrator-attributed revisions and a compact event; same-key recovery and stale-revision rejection follow the other guarded writes. New review requests bind the current policy revision without adding a per-call policy flag. Paused channels refuse new requests. Before preparation and admission, a missing, changed, unbound or paused policy holds the original work rather than silently adopting another revision. The final local action guard repeats those checks with current actor, configuration, capability and reservation binding. Exact request-key replay retains its original receipt after policy changes. The concrete drivers check the project file ceiling against their fresh source/file facts. Policy configuration has no CLI/MCP tools; the captured-comparison assessment below is a separate read.

## Captured comparison assessment

`GET /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/policy` applies current project requirements to the immutable comparison captured by that native evidence record. It accepts no query parameters or caller-supplied source facts. Its compact response contains `project_id`, `evidence_id`, the retained `anchor`, `source:"captured_comparison"`, `policy_revision`, `evaluated_at`, `state` and per-required-channel `{channel,covered,pending}` results. It excludes source findings and raw captures and fits a 16 KiB client response budget.

The service authorizes the project, validates retained native evidence and its original admitted identity, and evaluates current policy plus both channels in one bounded database snapshot. `sufficient` requires every explicitly required PR/channel to have complete finalized native completion with no remaining native obligations or pending incoming routes across all heads. Later candidate, base, parent or history movement does not erase earned completion or require another round; the [review contract](reviews.md#pr-and-channel-completion) owns that boundary. Explicit round requirements and stops still apply. [Finding routes](review-routes.md) own the optional incoming count, capacity and reason fields. `held` reports unsatisfied requirements. Absent policy returns `unconfigured`, revision zero and an empty channel list; missing requirements cannot pass vacuously. Merely enabling another channel does not make it required, and pausing execution does not erase existing evidence. Effective decisions and fix references govern obligations while original finalized counts remain unchanged.

The returned anchor identifies the historical comparison actually captured; the result evaluates current PR/channel requirements using earned native completion and all live obligations. It does not relabel that capture as a review of later code, refresh source, establish that the captured comparison remains current on GitHub, assess required CI or unresolved provider threads, or declare merge readiness. Repeated reads can reflect later policy or finding corrections without rewriting the capture. Reads perform no mutation or provider work.

## Bound CLI and MCP

The CLI uses its selected private profile without project or worker flags on each operation:

```sh
fc reviews evidence EVIDENCE_ID
fc reviews adjudication EVIDENCE_ID
fc reviews assess EVIDENCE_ID
fc reviews decide EVIDENCE_ID --key DECISION_KEY --expect-revision 0 --finding 0 --disposition accepted --reason 'Verified against the current source.'
fc reviews finalize EVIDENCE_ID --key FINALIZATION_KEY --expect-revision 1
fc reviews fix EVIDENCE_ID --key FIX_KEY --expect-revision 2 --finding 0 --expect-sha= --fix-sha COMMIT_SHA --proof 'Focused runtime proof and source reference.'
```

Replace the uppercase placeholders with returned identities and the verified commit. Finalize source decisions when adjudication is complete, including accepted findings awaiting fixes: this freezes the original counts and automatically queues the review summary. Recording fixes later never changes that summary or creates another one. The example therefore finalizes before recording fix proof. Read the current revision instead of inferring it from this example.

`reviews evidence --help` and `reviews adjudication --help` explain these reads locally without loading profiles or contacting the service. Native evidence IDs are `review_evidence_` followed by exactly 32 lowercase hexadecimal characters; use the complete ID returned by the original operation. Invalid evidence IDs receive format-specific guidance, without echoing the submitted value.

MCP exposes the read-only `review_policy_assess` tool with `evidence_id`, plus `review_evidence_read`, `review_adjudication_read`, `review_finding_decide`, `review_finding_fix` and `review_source_finalize`. Mutations require the corresponding fields above plus `evidence_id` and `idempotency_key`. Tool schemas describe the zero-based finding index and field bounds. The adapter validates arguments before opening credentials or making a request and retains one five-second command budget. Exact repeats have the same keyed effect; neither adapter retries automatically or selects another project.

These tools record local review evidence. Project instructions and the optional [agent workflow](agent-workflow.md) decide when workers should review, update jobs or hand off.

## Validation boundary

Focused public-handler, typed-client, CLI and actual stdio MCP checks must cover project/read-write isolation, instance mismatch, strict fields, exact keyed replay, stale revisions, guarded fix corrections, retained source counts and bounded failures. Policy handlers also cover administrator authority, explicit empty arrays/zero, immutable revisions and the unconfigured state. Integration checks must verify atomic ledger/projection rollback and retained history. Packaged-service checks must verify daemon registration and actual browser refresh before claiming executable projection consistency. Passing the ledger controls alone does not establish provider dispatch, admission, hosted recovery or native cutover. The execution contract records the separate configured-driver publication proof and remaining live CodeRabbit limits.

## Executed proof

At runtime checkpoint `f8bb495`, full `make check build` passed: 75 browser tests, Go race tests and vet, documentation checks, embedded assets and all three binaries. An independent integration pass identified the escaped-output size gap; the correction and focused [response-budget tests](../internal/reviews/detail_test.go) prove oversized rollback and a full response within one percent of the limit. [Projection integration tests](../internal/reviewexec/projection_test.go) cover source identity, frozen counts, current facts and all-or-nothing ledger/projection changes.

Four actual packaged-service groups then passed with clean committed `fc`/`fcd` binaries, a private synthetic capture/publication fixture and Chromium at desktop and 390px widths. Six bound CLI mutations produced matching decisions, finalization and guarded fix corrections in the browser after refresh. Imported and merged history, old snapshots, source text and current PR facts were preserved. Controlled identity/capacity failures rolled back every durable effect; evidence without a projection stayed ledger-only. Policy checks covered explicit zero/empty arrays, administrator authority, viewer reads, read-only browser cookies, exact replay, stale revisions and missing/null fields. The fixture made 105 HTTP requests, including 48 browser requests; the browser made only reads and its session create/delete calls. There were no provider attempts, browser errors, credential persistence or retained fixture processes/state.

The earlier packaged evidence controls also passed actual CLI and official-SDK stdio MCP workflows, original-key recovery after a lost committed response, five-second command deadlines and restart retention. The latest browser proof does not exercise maximum-size captures; their budget boundary has focused Go proof. These checks establish local controls and projection consistency, not real provider execution, hosted recovery, remote deployment of this slice, other-platform execution or fleet throughput.

At runtime checkpoint `48fd6e8`, full `make check build` passed with 79 browser tests and all Go race/vet, documentation and asset checks. An independent integration pass caught and corrected an unbounded failed-history scan in the CLI coverage lookup; its completed-operation index and finite candidate selection have focused regression and query-plan proof. A separate lifecycle pass found no further issues in its scoped transition, replay, timing and rollback checks.

Three actual packaged-service groups then passed in 6.093 seconds using clean `fc`/`fcd` binaries, an isolated trusted admission/publication helper and Chromium at desktop/390px widths. Browser/API reads preserved preparing, held, uncertain and completed states; six real CLI mutations preserved original captures, imported merged history and frozen source counts. Policy-change holds created no attempt, paused requests were refused, exact replay had one effect, and coverage changed only with the required finalization and effective fix proof. A moved head never inherited coverage. The fixture made 165 loopback HTTP requests, including 127 browser requests, with zero external/provider calls, browser errors or retained credentials/processes/state. This proves the local integration with synthetic provider evidence; that checkpoint used an unconfigured daemon and did not exercise a runnable provider capability.

The channel-scoped completed-history comparison uses the partial `review_operations_completed_comparison` index in execution schema version five, replacing the earlier CLI admission index. Fresh databases omit the unused head-specific coverage index; a separately versioned index migration removes it from existing databases without renumbering execution-table migrations. Focused race tests upgrade an actual version-four SQLite fixture to version five, preserve the journal and exact request-key replay, and verify the completed-comparison index is present and obsolete indexes are absent. Version-one, version-two and version-three upgrade checks and the completed-scan query-plan regression also pass. This index correction does not claim new packaged-provider behavior.

### Captured-comparison assessment proof

Clean checkpoint `10143d0653c68664abfa512c0537663f148e91c1` passed five packaged policy/control/restart stages in 2.623 seconds through actual HTTP, `fc reviews assess` and the official SDK connected to packaged `fcmcp`. A completed but unfinalized two-finding hosted capture was held; explicit decisions, finalization and fix references made its required hosted coverage sufficient. Pausing hosted execution preserved that credit. Requiring CLI additionally held the assessment, and restart preserved the same captured target, current policy revision and held result.

All fifteen assessment reads left logical database contents and provider counts unchanged. Responses were 829–885 bytes; API reads took 1.24–1.48 ms, CLI reads 3.58–4.89 ms, and fresh SDK/MCP calls 9.30–17.03 ms including process initialization. Original captures, current PR metadata and imported merged history stayed intact. The setup execution made fourteen controlled provider GETs and one each of allowance-query and review posts; assessment added none. The 283-file source/assets/documentation manifest remained unchanged at SHA-256 `5c8597ca4aa0c0fc0f02f9b83110a23a1298dbdd1bf21503414d93de74af1a7f`, as did all three packaged executables. Disposable processes and state were cleaned.

These are local controlled-provider measurements. The pass did not exercise browser rendering, physical phones, real provider/account facts, fleet throughput, fresh-source or CI assessment, merge readiness or operational cutover. Focused tests separately cover required-channel combinations, exact comparisons, effective corrections, imported-history exclusion, authorization, corruption, cancellation and the completed-history bound.

## Retained empty collections

A retained zero-finding adjudication is returned as an empty array even if its stored JSON uses `null`. Normalization happens in the ledger read view, does not rewrite stored evidence or history, and applies equally to the API, typed client and local consumers. Focused handler and direct-read tests prove the accepted zero state remains readable without durable writes.

Project-wide channel pauses are removed. Use the single execution pause to stop all review requests, or per-PR channel closure/limits for deliberate local controls. Upgrade retains original policy/configuration history, appends a current policy without channel pauses, and conservatively converts any existing channel stop into the single project execution pause. Historical retained selections still validate their original policy; old records are not rewritten.
