# Same-Project Finding Routes

## Implementation status

Native routing implements the same-project ledger, source/receiving guards, transactional browser projection, incoming-policy holds and bound CLI/MCP controls described below. Focused SQLite, HTTP, client and SDK tests pass. Clean packaged CLI/MCP/browser proof is still pending; this slice does not establish native operational cutover.

## Source and receiving authority

A route assigns one retained native finding to another configured PR in the same project and repository. It preserves the original evidence, finding index, source PR, source head and actor-attributed routing reason. Creating a route requires current project write permission, the source adjudication revision and an explicit idempotency key. The receiving PR must differ from the source PR and belong to the configured queue. Routing performs no provider work, starts no review and sends no implicit worker message.

The source decision becomes `routed` and references a durable route ID. Finalization freezes original found, accepted and routed counts. Receiving decisions never rewrite those source counts or the capture. Receiving acceptance and rejection have their own route revision, actor, reason and timestamp. An accepted receiving finding needs a guarded fix SHA and proof note; rejection records its reason without inventing a fix. An undecided route or acceptance without fix proof remains an outstanding obligation.

Source coverage requires source finalization and satisfied receiving obligations. Local policy assessment also holds a receiving PR while active incoming routes remain undecided or accepted without fix proof. CI success cannot clear either obligation. Routed findings are not fresh review captures for the receiving PR. Both local policy responses expose optional `incoming_routes_pending` (an exact count through 100) and `incoming_routes_capacity` (an explicit hold beyond that inspection budget). A held response uses `routed_findings_pending` or `routed_findings_capacity` as its reason while retaining each channel’s independent coverage result. Corrupt inspected route records fail closed. An omitted count/capacity field means zero/false; neither is a readiness claim by itself.

## Revisions and correction

The route ledger owns receiving state. The source adjudication stores only the current route identity; browser route summaries are derived projections. Each route revision retains a complete immutable state record. All changes use the normal project authorization, idempotency and transactional event boundary.

An explicit reroute creates a new route identity and supersedes the previous route, retaining its complete history. It requires both the current source revision and the expected receiving-route revision. A source decision can similarly replace a routed disposition only with that receiving revision guard. A route carrying fix proof cannot be silently superseded or rejected. Fix corrections append an audited change guarded by the expected current SHA. Superseded routes remain readable and cannot receive new decisions or fixes.

Receiving changes refresh an already published source browser run in the same transaction. Projection failure rolls back the route revision, audit, event and receipt. Ledger-only source evidence stays ledger-only; routing never invents a browser run. Original captures, source decision attribution, frozen counts, current PR metadata, merged history and previous snapshots remain intact.

## Bound controls

Creation uses `POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/routes` with `expected_revision`, `finding`, `target_pull_number`, `reason` and optional `expected_route_revision`. Omission of the route guard means zero and is valid only for an initial route. A completed creation returns `{id,state:"routed",revision,source_revision}`: the new route ID and revision, plus the resulting source adjudication revision.

`GET /api/v1/projects/{project_id}/reviews/routes/{route_id}` deliberately returns the route, its original source anchor and the one original finding. Ordinary incoming-route lists exclude finding text and full history. `GET /api/v1/projects/{project_id}/reviews/routes?pull_number=N` selects one receiving PR, with default limit five, a maximum of 100 and a project/filter-bound cursor. It uses stored indexed facts and performs no provider work. Lists show active routes; explicit ID reads preserve superseded history.

Relative POST paths `/decisions` and `/fixes` on a route use `expected_revision` and an explicit key. Decisions require `disposition` (`accepted` or `rejected`) and `reason`. Fixes require `expected_sha`, `fix_sha` and `proof_note`. Both return compact completed `{id,state,revision}` results. Existing source decision controls accept an optional `expected_route_revision` only when explicitly replacing a routed decision.

The bound CLI and MCP use the same private profile and typed client as other review controls. Incoming lists are compact and paged; route detail and source finding text require explicit selection. The five CLI commands are `reviews route-create EVIDENCE_ID`, `reviews route ROUTE_ID`, `reviews routes --pr N`, `reviews route-decide ROUTE_ID` and `reviews route-fix ROUTE_ID`. Creation accepts `--finding`, `--target-pr`, `--expect-revision`, `--reason` and `--key`, with `--expect-route-revision` for a correction. Receiving decisions use `--expect-revision`, `--disposition`, `--reason` and `--key`; fix references use `--expect-revision`, `--expect-sha`, `--fix-sha`, `--proof` and `--key`. MCP exposes `review_finding_route`, `review_route_read`, `review_routes_list`, `review_route_decide` and `review_route_fix` with equivalent named fields. No per-call project or worker flags, provider refresh, automatic polling, generated retry keys or implicit acknowledgment are introduced.

## Functional proof

Required proof covers same-project identity and authorization, source/receiving revision races, exact replay, wrong-key conflicts, receiving acceptance/rejection and guarded fixes, reroute and proof protection, source finalization before and after receiving action, pending obligations on both PRs, historical counts and captures, projection rollback and ledger-only operation. Stored corruption must fail closed. Index-backed pagination must not scan unrelated projects or disclose their routes. Restart and packaged CLI/MCP/browser checks must demonstrate the claimed executable boundary before native cutover.

Focused proof covers real source/receiver revision and replay races, all five projection rollback boundaries, ledger-only operation, source freeze and capture preservation, schema reopening, scoped pagination, guarded proof conservation and corrupted stored records. Policy tests retain a fully covered receiving channel while incoming obligations hold the PR, then clear that hold on receiver rejection or guarded fix. Client and HTTP tests cover strict fields, nulls, source/target binding and compact mutation results. These tests do not establish fleet performance or live-provider behavior.
