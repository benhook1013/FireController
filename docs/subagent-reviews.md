# Subagent Reviews

## Implementation status

Direct worker API, CLI and MCP recording/read/update are implemented over the existing immutable history. Focused public-handler, actual SDK, replay, rollback and client checks pass; packaged website proof and installed deployment are still pending. Administrator history import is not the ordinary recording workflow.

## Worker workflow

A project writer records an actual completed independent review through `POST /api/v1/projects/{project_id}/reviews/subagent-reports`. Supply `pull_number`, the reviewed full lowercase `head_sha`, `reviewer`, `scope`, ordered RFC3339 `started_at`/`finished_at`, and `findings`. The scope describes the actual comparison and limits, including its base when relevant. Optional `coverage_limits` records explicit limits. Finding entries contain `title`, Markdown `detail`, `severity`, and optional `disposition` (`accepted`, `rejected` or omitted/unadjudicated) with `reason` for a decided finding. Paths and line references belong in the finding detail. The service generates identities and derives counts; callers do not choose a provider, producer or another worker identity.

Optional `finalized:true` records already adjudicated results in that same call, including a complete zero-finding review. Otherwise read `GET /reviews/subagent-reports/{run_id}` for the stable run revision, findings and zero-based finding indexes. Both creation and updates require an explicit idempotency key and ordinary project write access, without administrator access or another agent's profile.

`POST /reviews/subagent-reports/{run_id}/updates` accepts `expected_revision`, optional `decisions` and `fixes` arrays, and optional `finalize:true`. Each decision supplies `finding`, `disposition` and its own `reason`. Each fix supplies `finding`, required `expected_sha` (empty when none exists), full lowercase `fix_sha` and `proof_note`. At least one change is required, indexes are unique within each list, and the complete selected batch commits atomically. Omitted findings stay unchanged. A stable run revision guard does not become stale because unrelated queue observations advanced.

Finalization requires a decision for every finding and freezes original counts. Later corrections and fix references preserve captured text, reviewed head, reviewer, original times and frozen counts. Fixes never create another review. Exact request-key replay returns the original compact `{id,state,revision}` receipt under current authorization; changed input conflicts. Source findings, current decisions and immutable history reuse the existing review workspace and renderer rather than a parallel report store or website component.

## Commands and MCP

```sh
fc reviews subagent-record --file /private/review.json --key RECORD_KEY
fc reviews subagent-read RUN_ID
fc reviews subagent-update RUN_ID --file /private/changes.json --key UPDATE_KEY
```

The MCP equivalents are `review_subagent_record`, `review_subagent_read` and `review_subagent_update`. Record arguments match the record fields plus `idempotency_key`. Update arguments add `run_id` and `idempotency_key` to the update fields. Read needs only `run_id`. CLI files are private regular UTF-8 JSON, limited to 256 KiB. Reads use a 4 MiB response budget; ordinary writes retain their five-second client budget and return directly without provider work. Input validation precedes profile/credential access. Help is local.

## Review authority

A worker-attested Subagent review appears in the existing chronological history and Subagent pill. It does not impersonate CodeRabbit, request a review, spend an allowance, change grants or taper, or satisfy required CLI/Hosted completion. Original findings and their decisions/fix proof stay visible in that review; recording does not change provider admission rules, add provider credit, erase earned rounds or change round settings. Recording does not publish a GitHub checkpoint. Finding bodies retain the existing private boundary; deliberately published review counts use the existing allowlist. Original source and same-key provenance remain attributable to the authenticated recording worker.

The live `worker-subagent` producer is reserved for these controls. History import can retain an exact existing record but cannot forge a new live report or overwrite its capture, decisions or fix proof. Record insertion preserves the selected PR’s stored metadata/completeness and all unrelated history.
