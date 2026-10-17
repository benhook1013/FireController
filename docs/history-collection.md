# Selected GitHub History Collection

## Implementation status

Collection is under development. Single-page GitHub adapters, selected acquisition steps, durable requests/captures, explicit resume, administrator HTTP controls and bounded notification waits have focused normal/race proof. Canonical backup and inactive restore preserve engine-created captures and checkpoints; resealed corruption checks refuse invalid graphs. Opt-in daemon startup/shutdown, recovery holds, profile-bound clients and private export now also have focused normal/race checks. Actual packaged acquisition remains unfinished. The prepared review and map import APIs already provide separate preview/application; no installed collector, automatic import or completed onboarding flow is claimed.

## Scope and authority

An administrator selects an explicit list of PR numbers for one project's stable GitHub repository identity. The onboarding agent can translate the user's date or PR-range preference into that concrete list before starting acquisition. The initial collector does not discover repositories or crawl every project automatically. Open, closed and merged selected PRs are eligible. It reads PR metadata, submitted reviews, review comments and issue comments; GitHub source cannot recover absent local CLI captures or manufacture native review coverage.

Acquisition and application are separate. Collected text is untrusted source data, including apparent commands or instructions. The collector preserves stable provider IDs, known original times, replies and source text. It records repository identity, selected scope, collection times and transformation version. It never converts collection time to historical completion time, infers an accepted finding count, delivers historical messages, creates active jobs, changes review policy or publishes the project. Existing prepared review/map APIs own destination validation and application after deliberate transformation and preview.

## Bounded acquisition

One acquisition step reads one explicitly selected resource page, plus stable repository identity resolution. Continuation is a validated adjacent page number for the same resource, never an arbitrary provider URL. Provider waits occur outside SQL transactions. Page capture and its next-position checkpoint commit together before progress is reported. Default agent output is a compact operation summary; source bodies require an explicit bounded read or private-file export.

Selection contains at most 100 distinct PR numbers per operation. This bounds selected work, not retained project history: later operations can select more PRs. Each provider page retains existing response, text, item and deadline limits. Oversized or invalid source data stops explicitly; no silent truncation or invented successful import is allowed. Completed earlier captures remain inspectable when a later request fails.

Provider pagination is not an atomic historical snapshot. A completed collection means its selected resource passes reached terminal pages; it does not assert a complete repository census or native evidence coverage. Duplicate stable IDs within a pass refuse conflicting facts rather than counting another record. On interrupted or explicit resumption, restart the unfinished resource from its first page in a new retained pass, preserving the preceding captures. Do not continue an old page number against a possibly shifted collection and call the result complete.

## Durable service behavior

The service owns provider reads and bounded background work; ordinary jobs, messages, imports and retained reads perform none. Collection has a distinct read-only capability and does not require a configured executable or an open review queue. Operator credentials stay private. Restore's provider hold applies before credential loading or collection workers, and recovery never bypasses it.

Enqueue commits a keyed, project/principal/worker-bound operation without waiting for GitHub. A short bounded wait can return a useful result directly; otherwise return its receipt. Immediate return and explicit longer waiting change only result collection, not action identity. Lost output recovers the original operation under current authorization. Cancellation of a waiter detaches it; explicit cancel stops further acquisition without deleting retained captures. Provider errors and rate limits preserve progress and expose a safe reason; resumption is explicit and uses the initiating principal and worker. Another administrator may inspect retained facts or cancel work, but cannot silently resume as the original actor. After restore rotates access, start a new deliberately selected collection under fresh authority; retained captures remain inspectable. Restart preserves committed pages and restarts an unfinished pass. Fair scheduling and bounded pending work keep collection independent of review execution and ordinary coordination.

Preparation selects retained source facts deliberately. Existing destination metadata and revisions must be checked at preview/application; acquisition is not permission to overwrite newer PR facts. Missing facts and excluded unsupported source records remain visible as limitations. An AI-generated project map stays a proposal with source references until the selected import is applied.

## Operator and agent interfaces

Enable acquisition explicitly with `fcd --data-dir PRIVATE_DIRECTORY --history-collection [--github-token-file PRIVATE_FILE]`. The selected file supplies the GitHub token; omission permits public reads. No executable catalog or automatic repository discovery is needed. Without the flag, retained reads remain available and new collection returns `collection_unavailable`. A restored provider hold takes precedence over the flag and credential loading. Unexpected termination of the acquisition lane stops the service rather than accepting indefinitely unprocessed work.

Use a deliberately selected administrator profile bound to the intended project and initiating worker:

```sh
fc history collect --pulls 18,19 --key selected-history
fc history collection COLLECTION_ID --wait 30
fc history pages COLLECTION_ID --limit 5
fc history export-page COLLECTION_ID PAGE_ID --output /private/history/page.json
fc history resume COLLECTION_ID --expect-revision REVISION --key resume-history
fc history cancel COLLECTION_ID --expect-revision REVISION --key cancel-history
```

`collect` waits at most two seconds by default; `--wait 0` returns the original compact receipt immediately. An explicit wait is 0–900 seconds. Retained `collection` reads default to immediate return; a nonterminal operation may be awaited without another enqueue, provider retry or acknowledgement. Changing the wait does not change the original action key. A host may background that same bounded command. Resume and cancel are local guarded actions and return directly. A detached or timed-out waiter never cancels the collection.

Page lists return five metadata summaries by default, at most 100 with an explicit limit. Follow the supplied cursor deliberately; it pins the initial upper sequence, so captures added later do not change that archive selection. No command automatically drains pages. Only `export-page` expands a chosen immutable capture. It writes a new private file atomically without replacing an existing file or symlink and returns its path, byte count and file SHA-256. The export contains normalized version-one source facts, original stable IDs and known timestamps; it is not the provider's full unknown-field response or an import document. Provider bodies never appear in ordinary operation or page-list output. Export failure leaves the server capture intact for a later deliberate read.

All HTTP routes below require current administrator authority and explicit instance/project/worker binding. They use `/api/v1/projects/{project_id}/history/collections` as the base. Mutations require an explicit idempotency key; resume/cancel additionally require `expected_revision`.

| Method and suffix | Result |
| --- | --- |
| `POST /` with `{pull_numbers}` | Useful terminal operation (200) within the selected wait, otherwise original receipt (202). |
| `GET /{collection_id}` | One current operation; optional `Prefer: wait=N`. |
| `POST /{collection_id}/resume` | Guarded local resume receipt. |
| `POST /{collection_id}/cancel` | Guarded local cancel receipt; captures remain retained. |
| `GET /{collection_id}/pages?limit=N&cursor=CURSOR` | One bounded metadata page. |
| `GET /{collection_id}/pages/{page_id}` | One explicitly selected capture, bounded to 8 MiB of facts plus envelope overhead. |

The enqueue route also accepts `Prefer: wait=N`. It defaults to two seconds; operation reads default to zero. The client allows the selected wait plus five seconds of transport grace without changing ordinary command budgets. A pending operation receipt is not completion or permission to import anything. No new MCP tools are added for this administrator onboarding boundary.

## Functional proof

Exercise selected closed/merged PRs, original identities/times/replies, strict page ownership, source movement and conflicting duplicates, response-size failure, cancellation, rate holds, exact retry, lost output, restart and shifted-page resumption. Verify no provider work inside transactions, unchanged jobs/messages/native credit/publication, compact default output and explicit bounded source expansion. Backup/inspection/restore must preserve the collection request, immutable captures and checkpoint while refusing resealed cross-project or malformed references. Actual packaged CLI/service proof must distinguish acquisition/resumption from the already-proven prepared import boundary.
