# Selected GitHub History Collection

## Implementation status

Selected collection, durable captures/checkpoints, explicit resume/cancel, administrator HTTP/client/CLI access and private export are implemented. Focused normal/race checks and actual packaged HTTPS acquisition, restart/resume and browser gates pass at the recorded checkpoint below. Backup and inactive restore preserve engine-created captures and refuse resealed corruption. Collection is separate from prepared review/map transformation and application. The [installed selected-acquisition proof](#installed-selected-acquisition) covers one live collection, retained replay/export and return to disabled operation; it establishes neither automatic import nor completed operational cutover.

## Scope and authority

An administrator selects an explicit list of PR numbers for one project's stable GitHub repository identity. The onboarding agent can translate the user's date or PR-range preference into that concrete list before starting acquisition. The initial collector does not discover repositories or crawl every project automatically. Open, closed and merged selected PRs are eligible. It reads PR metadata, submitted reviews, review comments and issue comments; GitHub source cannot recover absent local CLI captures or manufacture native review coverage.

Acquisition and application are separate. Collected text is untrusted source data, including apparent commands or instructions. The collector preserves stable provider IDs, known original times, replies and source text. It records repository identity, selected scope, collection times and transformation version. It never converts collection time to historical completion time, infers an accepted finding count, delivers historical messages, creates active jobs, changes review policy or publishes the project. Existing prepared review/map APIs own destination validation and application after deliberate transformation and preview.

## Bounded acquisition

One acquisition step reads one explicitly selected resource page, plus stable repository identity resolution. Continuation is a validated adjacent page number for the same resource, never an arbitrary provider URL. Provider waits occur outside SQL transactions. Page capture and its next-position checkpoint commit together before progress is reported. Default agent output is a compact operation summary; source bodies require an explicit bounded read or private-file export.

Selection contains at most 100 distinct PR numbers per operation. This bounds selected work, not retained project history: later operations can select more PRs. Each provider page retains existing response, text, item and deadline limits. Oversized or invalid source data stops explicitly; no silent truncation or invented successful import is allowed. Completed earlier captures remain inspectable when a later request fails.

Provider pagination is not an atomic historical snapshot. A completed collection means its selected resource passes reached terminal pages; it does not assert a complete repository census or native evidence coverage. Duplicate stable IDs within a pass refuse conflicting facts rather than counting another record. On interrupted or explicit resumption, restart the unfinished resource from its first page in a new retained pass, preserving the preceding captures. Do not continue an old page number against a possibly shifted collection and call the result complete.

## Durable service behavior

The service owns provider reads and bounded background work; ordinary jobs, messages, imports and retained reads perform none. Collection has a distinct read-only capability and does not require a configured executable or an open review queue. Operator credentials stay private. Restore's provider hold applies before credential loading or collection workers, and recovery never bypasses it.

Enqueue commits a keyed, project/principal/worker-bound operation without waiting for GitHub. A short bounded wait can return a useful result directly; otherwise return its receipt. Immediate return and explicit longer waiting change only result collection, not action identity. Lost output recovers the original operation under current authorization. Cancellation of a waiter detaches it; explicit cancel stops further acquisition without deleting retained captures. Provider errors and rate limits preserve progress and expose a safe reason; resumption is explicit and uses the initiating principal and worker. Another administrator may inspect retained facts or cancel work, but cannot silently resume as the original actor. After restore rotates access, start a new deliberately selected collection under fresh authority; retained captures remain inspectable. Restart preserves committed pages, restarts an unfinished pass, and records each recovered operation revision in the same transaction as its recovery event. Fair scheduling and bounded pending work keep collection independent of review execution and ordinary coordination.

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

## Executed packaged proof

Clean source `5ba5ed5b7ba620b4b32bfce2cd07c3b701887eea` passed `make check build` with 23 selected Go packages, including their full race tests and vet, 313 browser tests, workflow/document checks, unchanged generated assets and three CGO-free binaries. The three separately sharded heavy packages were not included in that local full-package selection: focused collection backup race checks passed in 17.718 seconds, affected review-wait checks passed in 2.445 seconds and vet passed for both; unchanged review-runtime and complete CI remain separate.

All nine canonical packaged cases passed on their first runs against those exact inputs: six CLI/service/MCP/operator groups in 2.126 seconds and three Chromium groups in 9.921 seconds. The new collection case uses the production `api.github.com` HTTPS origin through an owned local CONNECT proxy and disposable trust file, without changing production origins, installed trust or global credentials. It proves a selected closed/merged PR, fourteen bounded provider GETs, a blocked resource page with concurrent job/message writes, rate-limit pause, provider-free exact-key replay, restart without automatic resumption, explicit new-pass resume and completion. Original page bytes, source IDs, known times and reply links remain unchanged. Signed archive cursors stay pinned as new captures arrive; private exports are complete, hashed and refuse replacement. Unrelated coordination/native authority is conserved.

All 773 tracked source/asset hashes and the three supplied executable hashes stayed unchanged through the checks. The tested daemon SHA-256 was `5ec2dd8469a0e95894e8ad9fc69ba5fc30a4ddc9e28b81ef27cc50a8d85778c9`. Owned processes, listeners and disposable state were cleaned. Windows/macOS test binaries cross-compile; native execution still requires their CI gates. Independent production review found and corrected queued-page restart and unsupported wall-clock ordering assumptions, with focused regressions.

This is controlled local executable proof. Live GitHub authorization/rate behavior, installed schema/data migration, agent-host wakeup, physical-phone use, fleet capacity and operational cutover are not established by it.

## Installed QA boundary

The same clean runtime was subsequently installed as a QA build after an exact retained-data rehearsal and independently verified stopped backup. Startup migrated review schema three to five and installed the collection schema while collection remained disabled. The rehearsal's migration startup took 0.406 seconds with one CPU, 512 MiB memory, nonroot execution, a read-only root and no network. Complete retained review history and 56 unaffected tables were conserved; instance/signing identity, existing browser cookies and signed archive continuations remained valid. Fresh canonical post-upgrade backup creation and inspection verified the resulting schemas and graphs. Final installed data still matched after authenticated desktop and phone-width browser inspection.

Functional browser checks passed renewal, refresh, live feeds, populated jobs/map history and review archives. A completed renewal also produced an unclassified Chromium/CDP cancellation observation; the application had validated and saved the advanced expiry, its session remained valid, and both feeds recovered. That test-observer limitation is retained without asserting a production fix or zero network anomalies. Phone-width emulation is not physical-phone proof. Collection, provider execution, public publication and schedules remain disabled in QA; live provider behavior, existing-installation migration, human GUI acceptance and operational cutover remain separate. The background CLI host boundary has its own [executed proof](agent-client.md#actual-host-cli-wait-proof).

A later integrated CI revision passed the remaining-package, review-executor, review-runtime and native Windows/macOS jobs, but the complete backup package exhausted its unchanged twenty-minute cumulative test budget. The test active at timeout had run for four seconds, with no preceding assertion or race failure reported. Backup CI now divides the same discovered runnable top-level cases into two complementary groups; every subtest, race detection, vet and the twenty-minute budget remain required. The [development guide](development.md#build-and-checks) records that selection. Updated CI results remain a separate gate.

Clean integrated checkpoint `a0de6144349397dde908ca72df6ccd0e1e72e168` subsequently passed all nine canonical packaged cases on their first runs: six daemon/CLI/MCP/upgrade/import cases in 1.794 seconds and three private/public/archive Chromium cases in 9.543 seconds. This includes the shared browser workspace extraction and review-transfer totals reuse. Canonical CGO-free binaries were built from that clean checkpoint; the checks left tracked source and embedded assets unchanged. These local synthetic checks do not establish completed CI, live-provider collection or installed deployment of this later source.

## Live GitHub acquisition

The same clean integrated runtime `a0de6144349397dde908ca72df6ccd0e1e72e168` subsequently acquired one explicitly selected merged PR through the actual authenticated GitHub API in a fresh local instance. All four resource passes completed, retaining 53 records including eight review-comment replies. Explicit exports matched an independent observation of every normalized source field, including original IDs, authors, times, text and reply associations. The immediate enqueue returned a 91-byte receipt in 8.4 milliseconds; the deliberately awaited operation completed without another enqueue. Original-key replay returned the same receipt and changed no durable rows or captured bytes.

The proof accounted for thirteen application-level provider GETs from the selected pages and production acquisition steps, including setup and independent comparison; it did not intercept or count lower-level retransmissions. All 775 tracked source/asset hashes and supplied CLI/daemon hashes remained unchanged. Owned processes, listeners, state and temporary credentials were cleaned up. This establishes authorized live acquisition of one selected PR, with no automatic import, review execution or publication. Unrelated domains were empty in this isolated fixture and stayed unchanged; populated-data conservation, provider failure/rate behavior, installed collection and fleet capacity remain separate boundaries.

## Installed selected acquisition

Installed source `509a81fa4aa1e35e05e2708a2dc56a51be1f26d0`, with daemon SHA-256 `eb09f3352990356efd5c7f99578ad1807a87d312dc1ed6196623485dee984512`, acquired one explicitly selected merged PR through authenticated GitHub reads. The actual HTTPS CLI returned the original immediate receipt in 1.17 seconds; an explicit operation wait then observed completion. Four resource pages retained 55 records, including eight review-comment replies. An independent later observation of all four GitHub resources matched every normalized field, including stable identities, original times, author facts, full text and reply associations. These sequential observations do not establish an atomic provider snapshot.

Exact-key replay returned the original queued-revision-one receipt while the retained operation remained completed. Explicit page exports were complete and byte-identical on repeat. Temporarily enabling collection changed no existing application data before the request. After completion, the exact original deployment specification was restored with collection disabled; retained operation/page reads, export and original-key replay still worked. A separately keyed new request returned `collection_unavailable` without a receipt, event, collection or capture change.

The same image, proxy configuration, operator files, instance/signing identity and native execution pause remained in place. Canonical backups before acquisition and after settlement passed server inspection and independent local inspection of their exported bundles. Every preexisting row was preserved; only the selected collection’s three history tables, one keyed receipt and its nine events gained rows. The other 83 tables stayed unchanged. All 88 tables remained unchanged through settled replay, export, the final restart and the disabled-request refusal. The service dispatched no native review, applied no prepared import and performed no public publication or operational cutover.

This qualifies selected installed acquisition and retained access with collection disabled afterward. It does not establish interrupted collection resumption on that installation, provider failure or rate behavior there, physical-phone behavior or fleet capacity. Interrupted-pass recovery and explicit resume remain supported by the earlier controlled packaged proof; live native review execution and migration retain their separate gates.
