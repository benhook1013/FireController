# Retained Operation Discovery

## Contract

`GET /api/v1/projects/{project_id}/reviews/operations` reads one compact page of retained native review operations under current authenticated project read permission. It performs no provider requests, review execution, retries or allowance changes. A list is discovery; select one operation for the compact status read below. The original [full operation read](review-execution.md#bound-execution-tools) remains available for explicit diagnostics.

Optional filters are `pull_number` (positive PR number), `channel` (`cli` or `hosted`) and `status` (`pending` or `all`, default `pending`). Pending includes queued, preparing, posting, active and uncertain operations, plus held operations only when their matching execution attempt still owns a reservation. `all` includes terminal and held history so a lost receipt can be recovered. Operations from other workers in the authorized project remain discoverable with attribution.

Pages default to five items, with an explicit `limit` from 1 to 100. Ordering is newest first by `(created_at, id)`. The signed `cursor` binds the service instance, principal, project and filters; preserve filters when continuing. Changing the page limit is permitted. Each page rechecks authorization and current operation states. Pagination is a keyset continuation over retained rows, not a historical snapshot across mutable states. No offset, automatic pagination or implicit continuation is provided.

The response contains `project_id`, `items`, `has_more` and a `cursor` only when more items exist. Each item contains only `id`, `pull_number`, `channel`, `head_sha`, `worker_id`, `state`, `reason`, `attempt_id`, `revision`, `created_at` and `updated_at`. Empty pages return an empty items array. Credentials, raw captures, allowance payloads, actor identities, configuration and policy are excluded. Invalid filters, malformed or mismatched cursors, and corrupt selected summaries fail explicitly without adopting partial results.

## Compact operation status

`GET /api/v1/projects/{project_id}/reviews/operations/{operation_id}/status` reads only `id`, `pull_number`, `channel`, stored `state`, `revision`, `stage`, `reason`, `attempt_id`, optional `evidence_id`, `updated_at` and `next_action`. It rechecks current project read permission and performs no provider requests, capture-file reads, source refresh, Git work, execution or accounting changes. The existing full operation GET is unchanged for clients that need diagnostics.

`Prefer: wait=N` retains the existing notification-based result collection, from zero through 900 seconds; zero returns immediately. Cancellation detaches collection without cancelling the operation or reposting a review. After the wait, a fresh authorized local read selects the compact status and its exact attempt binding. The response always preserves the stored operation state; a display stage cannot override execution rules.

Stages derive from durable facts. Queued and preparing operations report those stages. Posting or active operations without a complete bound capture report `provider_wait` and `next_action: wait`; an issued trigger alone does not prove that a provider is running. When their exact complete evidence is already retained, they report `result_captured` and `wait` while native publication/settlement finishes; this does not invent terminal completion or finalization. An uncertain Hosted operation with reason `hosted_finished_inventory_unproven` reports `capture_blocked` and `contact_controller_owner`. Other uncertain operations retain `uncertain`. Held or rate-limited work reports `held`, failed work reports `failed`, and their next action is `read_diagnostics`; cancelled work reports `cancelled` with no next action (`none`).

A completed operation reports `evidence_ready` and `read_evidence` only when its actual same-project, same-operation, same-attempt evidence ledger record has matching original admitted anchors and a complete successful capture. Its evidence ID is the native ledger identity, never a browser projection or an invented handle. Failed or incomplete evidence can retain its handle but cannot supply ready coverage; a completed operation with such a capture reports `capture_blocked`. Completed state without a matching retained evidence record reports `unknown` and `contact_controller_owner`. A bare finished acknowledgement cannot establish an empty result. Evidence readiness alone does not establish finalization, policy sufficiency or merge readiness.

The reader projects bounded identity and completion metadata from that one normalized evidence row; it does not load finding narratives or reparse provider bodies. It cannot invent finer preparation phases or provider progress that the durable journal does not record. Unknown facts remain unknown.

## Bound clients

`reviews operations [--pr NUMBER] [--channel cli|hosted] [--all] [--limit N] [--cursor CURSOR]` reads one page. `reviews operations --help` is local and requires no profile, credentials or service connection. Select `reviews operation OPERATION_ID [--wait-seconds N]` after discovery for compact status. Add `--diagnostics` for the original full retained operation. The wait range is 0–900 seconds; listing and status reads do not request replacement work. An `evidence_ready` result supplies the ID for the existing `reviews evidence EVIDENCE_ID` read.

MCP `review_operations_list` accepts optional `pull_number`, `channel`, `status`, `limit` and `cursor`. It uses the same bound typed client and authenticated HTTP API. Defaults and output are identical to the native list; the tool never polls or advances pages automatically. MCP `review_operation_read` returns compact status by default, accepts `wait_seconds` from 0 through 50 and optional `diagnostics: true` for the original full operation. Its wait is result collection, not permission for another provider action.

## Proof boundary

Focused offline checks exercise discovery and compact status through the native reader, public handler, bound client, CLI and official SDK tool discovery/calls using synthetic retained operations. They cover exact evidence binding, incomplete/missing captures, wait cancellation and fresh authorization, strict output validation, compact/default and explicit diagnostic views, and unchanged source/accounting records without provider work. Installed-host behavior, live provider state and packaged deployment are separate integration proof.
