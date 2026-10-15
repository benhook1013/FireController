# Native Review Evidence Controls

## Boundary

The service exposes retained native CLI evidence and its decision/fix ledger through project-scoped API, CLI and MCP operations. These controls use the same authorization and private client binding as jobs and messages. They do not start a review, upload a capture, refresh GitHub or publish a public checkpoint. The [execution contract](review-execution.md) owns trusted capture ingestion and provider admission; the [review workspace](review-workspace.md) owns stored browser projections.

The daemon installs the execution/evidence/adjudication and project-policy schemas and registers the local routes. Its capability set remains empty: runnable provider configuration and execution are unavailable until the supervised driver and admission integration are delivered. Adding these controls does not complete native review cutover or change the separately running development executor.

## Explicit reads and stable finding identity

`GET /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}` returns one retained parsed capture with its project, operation/attempt identity, capability fingerprint, exact source anchor, capture checksum/size/timing, outcome, completeness, reviewed paths and findings. All fields use snake_case. It excludes raw stdout/stderr, credentials, operator paths and the duplicate anchor inside the stored ingestion input. This full-detail read is deliberate; ordinary review queue reads do not acquire its finding text.

Each finding has an immutable, zero-based `finding` index within that evidence record. Select the index returned by the API; do not derive it from a browser row number or an earlier executor's numbering. Evidence IDs use `review_evidence_` followed by 32 lowercase hexadecimal characters. Browser projection run IDs are separate identities and cannot select this ledger.

`GET` on the same path plus `/adjudication` returns the current revision, finalized flag, original frozen counts when present and current per-finding decisions/fix references. It does not repeat the source finding text. Both reads require current project read permission, enforce instance binding and perform no domain mutation or provider work. Full responses are bounded to 4 MiB by the typed clients; service-error responses retain their separate 64 KiB limit.

## Guarded writes

Each write requires project write permission, an explicit `Idempotency-Key`, and the current adjudication `expected_revision`. Initial state has revision zero. Successful writes return only `{id,state,revision}` with HTTP 200. Preserve the exact key, request and binding if a response is lost; never generate another key or automatically retry an uncertain mutation.

| Relative POST path | Required JSON fields | Receipt state |
| --- | --- | --- |
| `/decisions` | `expected_revision`, `finding`, `disposition`, `reason` | `adjudicated` |
| `/fixes` | `expected_revision`, `finding`, `expected_sha`, `fix_sha`, `proof_note` | `fix_recorded` |
| `/finalization` | `expected_revision` | `finalized` |

Fields are explicit and non-null, including finding zero and an empty `expected_sha` for the first fix reference. Unknown fields and query parameters are rejected. Disposition is `accepted` or `rejected`; routing remains a later integration. Reasons and proof notes are bounded to 2048 UTF-8 bytes and follow the ledger's safe-text rules. Commit references are lowercase 40-character SHAs. `expected_sha` guards a correction against the current effective fix reference; it does not cause a provider lookup or independently verify that a commit proves the finding fixed.

Finalization requires a completed attempt, complete retained capture and a decision for every finding. It freezes the original source counts, including a valid zero-finding run. Later decision/fix corrections append audit records and retain the original capture and frozen counts. An accepted finding with a fix reference cannot be rejected silently. Finalization establishes source counts; policy sufficiency, fixed findings and merge readiness remain separate facts. Humans perform merges.

## Browser consistency

When a native run has already been published to the stored review workspace, a decision, fix reference or finalization updates its browser projection in the same transaction as the authoritative ledger, audit, event and receipt. A failed projection leaves all of them unchanged. Native publication and corrections must fit the browser's encoded 4 MiB run-detail response budget, including JSON escaping and its final newline. An oversized result returns `workspace_capacity` before commit; source text and history are never truncated. Shorter decision or proof text can fit without replacing captured evidence. The browser shows the new observation on refresh; these controls do not make provider requests.

The native run's original source identity and zero-based finding indexes bind that update. It preserves captured finding text, historical snapshots, imported runs, merged queue history and current PR facts. Finalized source counts remain the original counts even when a later audited correction changes an effective decision or fix. Updating a decision does not claim a fresh provider observation for other PRs.

Evidence without an initial native workspace publication remains usable through the ledger controls. A decision does not invent a browser run or convert imported historical evidence into native execution. Initial publication from real provider execution remains part of the driver integration.

## Explicit project policy

`GET /api/v1/projects/{project_id}/reviews/policy` requires project read permission and returns `{project_id,revision,actor_id,recorded_at,policy}`. An unconfigured policy returns `reviews_policy_unconfigured` with HTTP 409; absence never means that review requirements passed.

`POST` to that path requires administrator permission, an explicit idempotency key and four non-null JSON fields: `expected_revision`, `required_channels`, `paused_channels` and `max_review_files`. Initial revision is zero. Required channels are an explicit nonempty unique subset of `cli` and `hosted`; paused channels are an explicit unique subset and may be empty. `max_review_files` is zero for no additional project ceiling, or 1–100000 for a project ceiling. It neither asserts a provider's actual limit nor bypasses one. Unknown fields and query parameters are rejected.

Successful configuration returns `{id,state:"policy_configured",revision}` with HTTP 200. Changes retain immutable administrator-attributed revisions and a compact event; same-key recovery and stale-revision rejection follow the other guarded writes. Policy currently has storage and API controls only. Admission, aggregate sufficiency and merge-readiness assessment do not yet consume it; no policy CLI/MCP tools are exposed in this slice.

## Bound CLI and MCP

The CLI uses its selected private profile without project or worker flags on each operation:

```sh
fc reviews evidence EVIDENCE_ID
fc reviews adjudication EVIDENCE_ID
fc reviews decide EVIDENCE_ID --key DECISION_KEY --expect-revision 0 --finding 0 --disposition accepted --reason 'Verified against the current source.'
fc reviews fix EVIDENCE_ID --key FIX_KEY --expect-revision 1 --finding 0 --expect-sha= --fix-sha COMMIT_SHA --proof 'Focused runtime proof and source reference.'
fc reviews finalize EVIDENCE_ID --key FINALIZATION_KEY --expect-revision 2
```

Replace the uppercase placeholders with returned identities and the verified commit. Explicit source decisions can be finalized before fixes; the commands above are one possible ordering rather than a required project workflow. Read the current revision instead of inferring it from this example.

MCP exposes `review_evidence_read`, `review_adjudication_read`, `review_finding_decide`, `review_finding_fix` and `review_source_finalize`. Mutations require the corresponding fields above plus `evidence_id` and `idempotency_key`. Tool schemas describe the zero-based finding index and field bounds. The adapter validates arguments before opening credentials or making a request and retains one five-second command budget. Exact repeats have the same keyed effect; neither adapter retries automatically or selects another project.

These tools record local review evidence. Project instructions and the optional [agent workflow](agent-workflow.md) decide when workers should review, update jobs or hand off.

## Validation boundary

Focused public-handler, typed-client, CLI and actual stdio MCP checks must cover project/read-write isolation, instance mismatch, strict fields, exact keyed replay, stale revisions, guarded fix corrections, retained source counts and bounded failures. Policy handlers also cover administrator authority, explicit empty arrays/zero, immutable revisions and the unconfigured state. Integration checks must verify atomic ledger/projection rollback and retained history. Packaged-service checks must verify daemon registration and actual browser refresh before claiming executable projection consistency. Passing these controls does not establish initial provider publication, admission, hosted recovery or native cutover.
