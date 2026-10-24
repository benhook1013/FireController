# Retained Operation Discovery

## Contract

`GET /api/v1/projects/{project_id}/reviews/operations` reads one compact page of retained native review operations under current authenticated project read permission. It performs no provider requests, review execution, retries or allowance changes. A list is discovery; use the selected [operation read](review-execution.md#bound-execution-tools) for detail and recovery.

Optional filters are `pull_number` (positive PR number), `channel` (`cli` or `hosted`) and `status` (`pending` or `all`, default `pending`). Pending includes queued, preparing, posting, active and uncertain operations, plus held operations only when their matching execution attempt still owns a reservation. `all` includes terminal and held history so a lost receipt can be recovered. Operations from other workers in the authorized project remain discoverable with attribution.

Pages default to five items, with an explicit `limit` from 1 to 100. Ordering is newest first by `(created_at, id)`. The signed `cursor` binds the service instance, principal, project and filters; preserve filters when continuing. Changing the page limit is permitted. Each page rechecks authorization and current operation states. Pagination is a keyset continuation over retained rows, not a historical snapshot across mutable states. No offset, automatic pagination or implicit continuation is provided.

The response contains `project_id`, `items`, `has_more` and a `cursor` only when more items exist. Each item contains only `id`, `pull_number`, `channel`, `head_sha`, `worker_id`, `state`, `reason`, `attempt_id`, `revision`, `created_at` and `updated_at`. Empty pages return an empty items array. Credentials, raw captures, allowance payloads, actor identities, configuration and policy are excluded. Invalid filters, malformed or mismatched cursors, and corrupt selected summaries fail explicitly without adopting partial results.

## Bound clients

`reviews operations [--pr NUMBER] [--channel cli|hosted] [--all] [--limit N] [--cursor CURSOR]` reads one page. `reviews operations --help` is local and requires no profile, credentials or service connection. Select `reviews operation OPERATION_ID` after discovery; listing does not request replacement work.

MCP `review_operations_list` accepts optional `pull_number`, `channel`, `status`, `limit` and `cursor`. It uses the same bound typed client and authenticated HTTP API. Defaults and output are identical to the native list; the tool never polls or advances pages automatically.

## Proof boundary

Focused offline checks exercise the native reader, public handler, bound client, CLI and official SDK tool discovery/calls using synthetic retained operations. Installed-host behavior, live provider state and packaged deployment are separate integration proof.
