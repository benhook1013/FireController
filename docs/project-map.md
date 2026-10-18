# Project Map

## Implementation status

The selected [editorial Project page](browser-experience.md#editorial-project-page) replaces the primary catalogue-style presentation. Flexible Markdown content and deliberate editorial order must support generic high-level project information; optional progress metadata must not force every section into a checklist. The redesign adds generic narrative sections, Markdown presentation and deliberate position ordering. Existing entries, identities, optimistic revisions and immutable history remain valid; executed proof for the new boundary is recorded in delivery.

The API, typed client, CLI/MCP, read-only browser and backup integration are implemented. Focused checks, full validation at the recorded checkpoint and actual packaged CLI/MCP/browser checks passed. Earlier combined validation runs encountered review-engine setup deadlines and CI exposed a client timeout-classification defect. The timeout defect is fixed with direct regression proof; fixture failures now report their setup phase, without changed deadlines or assertions. A subsequent full check/build passed at the checkpoint below; the earlier failures remain recorded and their setup-time cause was not established. The inherited source/test inventory informs observable behavior; it does not require the old file layout or controller internals. Prepared historical map import remains a separate explicit preview/apply boundary under [onboarding](onboarding.md).

## Editorial redesign proof

Focused race groups cover generic sections, position validation, exact replay and historical bytes, bounded indexed editorial reads, populated version-2 migration/restart and rollback, typed clients, CLI/MCP, and backup/inspection/inactive restore. Resealed version-3 schema damage is refused before creating a restore destination. The earlier map proofs below describe their recorded checkpoints rather than exact coverage of this redesign.

An actual packaged Chromium check passed the private and anonymous public presentation at 1440, 390 and 320 pixels. It exercised safe Markdown, deliberate order, a live added section, optional metadata, explicit catalogue expansion, publication allowlists and absence of private fields. The private view made two editorial reads, one deliberately opened catalogue read and no individual-entry reads. Browser domain writes and external HTTP requests were zero. The [packaged fixture](../tools/packagedsmoke/editorial_browser_test.go) keeps setup writes separate from browser activity.

The same fixture first reproduced headings hidden behind the sticky header and canvas prose using card text colours. Both fixes then passed with actual section-link clicks and deliberately distinct project canvas/card themes. This is synthetic local executable proof; installed migration, physical-phone behavior and human visual acceptance remain separate. The broader local race run reached its package time budget in backup and review execution; focused passing checks do not turn that run into a full-suite pass.

## Records and authority

Each project owns narrative sections, workstreams and return points. A section supplies a title and Markdown description, with optional state and links, without requiring initiative phases or a deferred-work trigger. A workstream describes a named area, its free-form state, where it stands, its next milestone and ordered named phases with their own free-form state. A return point describes deferred work and the trigger for reconsidering it. State text is editorial information, never a calculated completion or merge-readiness verdict.

Service records own both editorial structure and mutable progress. Runtime reads do not depend on local JSON files, tracker paths, GitHub or provider refresh. Each entry has its own revision so unrelated workstreams can be updated concurrently. A replacement checks the expected revision and atomically records the complete new snapshot, immutable history, a small event and an idempotency receipt. An unchanged replacement returns the current revision without adding history or an event. Exact request replay returns its original receipt before evaluating a newer expected revision, after current authorization is checked.

Entry IDs are service-generated and stable. Titles may change without changing identity. There is no deletion endpoint: archiving retains links and history. Phase IDs are caller-chosen stable keys, independent of phase title or position. Reordering and renaming preserve IDs. Once introduced, a phase remains in later replacements, optionally archived; omitting an existing phase is rejected rather than allowing its ID to be reused for a different phase. An archived entry can be read and explicitly restored. Archival does not imply a work outcome.

## API and bounded values

All paths below are under `/api/v1/projects/{project_id}` and require the existing scoped read/write authority. Browser sessions remain read-only. No map operation calls a provider.

| Method and path | Contract |
| --- | --- |
| `POST /map/entries` | Explicit idempotency key; create one section, workstream or return point at revision 1. |
| `GET /map/entries` | Compact keyset page, optional exact `kind` and `archived` filters, default active entries; limit 25, maximum 100. `archived` accepts exactly `true` or `false`; omission selects `false`. |
| `GET /map/editorial` | Full active content in editorial order; default limit 5, maximum 10, explicit continuation. |
| `GET /map/entries/{entry_id}` | Complete current entry. |
| `PUT /map/entries/{entry_id}` | Explicit key and positive `expected_revision`; replace editable content of the same kind. |
| `GET /map/entries/{entry_id}/history` | Bounded immutable revision summaries, newest first. |
| `GET /map/entries/{entry_id}/history/{revision}` | One complete captured snapshot with actor and time. |

The editable object contains `kind` (`section`, `workstream` or `return_point`), `title`, `description`, `state`, `where`, `next`, `trigger`, `phases`, `links`, `archived` and optional `position`. A replacement wraps it as `{"expected_revision":1,"entry":{...}}`; creation accepts the editable object directly. The kind cannot change. Position is an integer from 0 to 1,000,000, default 0; zero is omitted from canonical persisted snapshots and responses, preserving existing historical bytes. Editorial reads order by position then stable ID; changing position is an ordinary revisioned replacement, not an identity change. Compact catalogue reads retain ID ordering. A workstream uses `where`, `next` and `phases` and has an empty `trigger`. A return point uses `trigger` and has empty `where`, `next` and `phases`. A section uses `description`, optional `state` and `links`, with empty `where`, `next`, `trigger` and `phases`. The complete current entry adds `id`, `project_id`, `revision`, `created_at` and `updated_at`. Collections serialize as empty arrays rather than null. Only `kind` and `title` are required scalar input fields; omitted optional scalars become empty text or false, while explicit scalar null is invalid. Phase inputs require `id` and `name`; omitted state/archived become empty/false. Links require label/url. Omitted or null phases/links become empty arrays. PUT replaces the whole editable object: omitted optional content clears/defaults it. Responses always contain the complete fields and non-null arrays. Input is validated before mutation; unknown JSON fields, malformed Unicode and unsafe control characters fail through the existing strict API rules.

Titles, phase names and link labels are trimmed before validation and fingerprinting. Short fields (including state text) reject all Unicode control characters; prose fields allow LF and tab and reject other control characters. Titles are nonempty text, at most 240 bytes. State is at most 240 bytes. Description, where, next and trigger are at most 8 KiB each. A phase contains `id`, `name`, `state` and `archived`; IDs match `[a-z][a-z0-9_-]{0,63}`, names are trimmed nonempty text up to 160 bytes, and states are at most 240 bytes. At most 64 phases, including archived phases, are retained per entry. A link contains a trimmed nonempty `label` up to 120 bytes and `url` up to 2048 bytes; at most 12 links. URLs reject all whitespace and require an absolute HTTP or HTTPS URL with a host, no credentials or control characters. File, script, data and protocol-relative URLs are rejected. Editable content is at most 64 KiB encoded JSON, within the existing request ceiling. Prose is stored as bounded UTF-8 text and rendered as CommonMark Markdown by the browser. Raw HTML is never executed, arbitrary CSS is unavailable, and image syntax never initiates external requests. Links must pass the browser safe-link policy. Existing plain prose remains readable.

Compact entry rows contain only `id`, `kind`, `title`, `state`, `archived` and `revision`. They omit descriptions, phases, links and provenance. Every page uses `items`, optional `cursor` and `has_more`, with limit 25 and maximum 100. History is newest first. A history summary contains `revision`, `actor_id` and `created_at` without captured content; callers select one revision deliberately, returning those fields and `state` containing the complete entry snapshot. Entry mutation receipts contain `id`, `revision` and `outcome` (`created`, `updated` or `unchanged`). Cursors bind project, filters and ordering; using one for another project or filter fails. Malformed selectors, kind changes, omitted retained phase IDs and oversized content return validation errors; missing or foreign IDs return the same scoped not-found response; stale revisions return the existing conflict shape. Job association conflicts use `job_workstream_revision_conflict` with a nonnegative `current_revision`, including the initial revision 0. Lists and history pages report completeness and an explicit continuation cursor without walking further pages automatically.

## Job references

A job may have one optional workstream reference in its own revisioned association, separate from the job's brief/status revision. The initial empty association has revision 0. `GET` and keyed `PUT /jobs/{job_id}/workstream` read or replace it using `expected_revision` and `workstream_id` (empty clears); `GET /jobs/{job_id}/workstream/history` returns bounded captured association history. The current association contains `job_id`, `workstream_id`, captured-at-association `workstream_title` and `revision`; the title remains unchanged when the map is renamed, including a no-op selection of the same ID; revision 0 has empty reference/title. The PUT body contains `expected_revision` and `workstream_id`. A receipt contains `job_id`, association `revision` and `outcome` (`updated` or `unchanged`). Selecting a new reference requires an existing unarchived workstream in the same project, never a return point. Existing references survive workstream archival or title changes and may always be cleared; an unchanged reference is a no-op. Association history rows contain `revision`, `workstream_id`, captured `workstream_title`, `actor_id`, `job_revision` and `created_at`, so later editorial changes cannot rewrite what the association meant. Map history retains full editorial snapshots as well as states. Ordinary job reads and routine context reads do not automatically include whole maps or association histories.

Deferred notes remain independently scoped records. Their existing free-form phase text does not become a map registry or an implicit map foreign key.

## Privacy, events and retention

All map content and history use authenticated project reads. This slice adds no public projection, filesystem tracker integration, external embed or automatic historical collector. The browser renders titles as literal text and prose through the bounded safe Markdown presentation, with explicit safe link handling. Events contain entry/reference IDs and revisions rather than descriptions, triggers, links or historical bodies. Appearance remains owned by [browser presentation](browser.md#project-presentation), independent of map revisions.

The domain is installed during exclusive startup before handlers run. Schema version 3 widens entry kinds to include sections, preserving current rows, immutable history, job associations and import mappings atomically. Migration failure rolls back the schema and data together; startup is retried explicitly. Position remains in canonical entry JSON rather than a second independent source of ordering truth. Backups retain current entries, contiguous full history and job associations/history; inspection rejects orphaned, malformed or inconsistent retained map records. Restore preserves stable identities and revisions. No private deployment data or historical source file enters public Git.

## Focused proof

Prove scoped read/write authority and project isolation, strict bounded values, free-form states, stable phase rename/reorder/archive semantics, no-op conservation, CAS conflicts, exact replay after later edits and restart, transaction rollback and event privacy. Prove compact list/history pagination binds filters and does not return bodies. Prove job references reject foreign/return-point/archived targets, preserve captured meaning through rename/archive, support clearing and do not alter brief revisions. Prove backup/inspection/restore retains full history and rejects inconsistent records. Exercise the real CLI/MCP and read-only browser separately before claiming those interfaces complete; migration/import and live deployment require their own proof.

## Agent commands

A bound profile supplies project/worker identity as usual. `fc map list` returns compact entries; select `read`, `history` or `revision` deliberately. `--kind section|workstream|return_point` and `--archived true|false` filter the list; explicit limit/cursor reads one page.

```sh
fc map create --file /private/workstream.json --key request-key
fc map replace ENTRY_ID --file /private/workstream.json --expect-revision 1 --key replacement-key
fc map read ENTRY_ID
fc map history ENTRY_ID
fc map revision ENTRY_ID 1
fc jobs workstream JOB_ID
fc jobs workstream-set JOB_ID --workstream ENTRY_ID --expect-revision 0 --key association-key
fc jobs workstream-set JOB_ID --clear --expect-revision 1 --key clear-key
fc jobs workstream-history JOB_ID
```

Map files contain the editable object, require an absolute regular-file path and strict UTF-8 JSON, and are limited to 64 KiB before normalization. Simple creation can contain only `kind` and `title`; optional fields have the defaults above. Replacement is complete: retain fields/phases that should remain. A successful local mutation returns its compact receipt directly, without an operation-polling cycle. Lost response/output recovery uses the same key, content and binding. The [MCP inventory](mcp.md#tools-and-output) documents equivalent bound tools.

## Focused implementation proof

Real SQLite/authenticated-handler groups pass normal/race in 0.533/3.915 seconds, including scope and current authorization, free-form states, phase retention, full editorial snapshots, CAS/no-op/replay after reopen, compact pagination, transaction rollback and captured job references without changing job briefs/revisions. An actual query-plan regression reproduced history continuation scans limited only by project/record; production continuation queries now seek the revision range on existing indexes, without another index or schema migration.

Typed-client groups pass normal/race in 0.252/1.563 seconds. Actual-handler CLI groups pass in 0.184/2.932 seconds; official SDK and affected discovery race groups pass in 6.233 seconds, with final focused checks in 1.632 seconds. Tests exercise committed lost response, same-key recovery across clients, malformed/strict input, cancellation, failed/short receipt output, and zero additional provider work after setup. Discovery followed every page at the then-current production page size of 64; the current 128-tool first-page boundary and its proof are recorded in the [MCP guide](mcp.md#discovery-page-boundary). The independent documentation inventory compares the actual complete SDK catalog with the table.

Backup/inspection/inactive-restore integration passes race checks in 12.199 seconds, preserving full editorial history and captured association titles through rename/archive. Resealed inconsistent current/history records are refused. Browser TypeScript and 198 unit tests pass, including strict map API and literal rendering cases. Native map deep-link/history route checks pass race tests in 1.021 seconds. These are the focused checkpoint results; later full-build and real packaged CLI/MCP/Chromium results are recorded under [executed packaged proof](#executed-packaged-proof) below.

The read-only browser adds Map lists and explicit current/history views using existing panels and navigation. Job references are loaded deliberately. Existing explicit visible-data refresh invalidates scoped map queries; message SSE is unchanged and does not imply automatic live map invalidation. No custom theme or cosmetic redesign is included.

## Executed packaged proof

Clean map checkpoint `cf0baef` passed six actual CLI/operator/MCP groups in 2.812 seconds, using 24 CLI calls, three operator calls and 11 official SDK sessions. At that historical checkpoint, discovery followed both pages at page size 64 (64 and 9 tools). The checks covered original-key replay after lost output, full editorial and phase history, compact pagination, scoped/CAS refusals, captured job-reference titles, restart and backup/inspection/inactive restore. Source, supplied binaries and earlier evidence stayed unchanged; owned processes were joined and no external/provider calls occurred. Independent canonical builds later matched all three supplied binary hashes.

Actual Chromium checks at 1440 and 390 pixels passed the map navigation, history, filtering, recorded job reference, explicit refresh, reload and project-isolation cases. They also found a real narrow-title clipping defect. Checkpoint `22a2ea5` adds only wrapping for that title; a separate packaged Chromium retest inspected current/historical titles at both widths and confirmed full text remained inside the panel without changing its font size. The targeted fix retest does not repin the entire earlier browser matrix.

These checks do not establish full-suite success, physical-phone behavior, fleet throughput, installed deployment or human GUI acceptance. The retained operational website is unchanged. Broad historical map import remains separate.

Clean checkpoint `a61c5fba16b98cd12737ac950bf9546211ead306` subsequently passed canonical `make check build`: TypeScript/198 browser tests, all Go race tests, vet, documentation checks, regenerated assets and three CGO-free binaries. The review-engine package completed in 334.388 seconds without a failure; this does not establish why its earlier setup deadlines failed. The canonical packaged daemon/CLI/MCP smoke then passed in 0.444 seconds. Later source changes require their own relevant validation rather than inheriting an exact-head full-run claim.

## Retained link readability

Stored HTTP link text is validated separately from browser navigation. If browser URL parsing rejects a retained link, the entry and its historical revisions still load; the link label displays as unavailable without an anchor. Safe HTTP/HTTPS links remain clickable, and text is rendered literally. This avoids making a browser-parser difference invalidate the whole stored entry.

Focused response/renderer regressions reproduced unreadable entries and now pass, including strict refusal of script URLs, credentials and whitespace. All 201 browser tests and TypeScript checks pass. Clean `c5d6529c832d98bcea85244b0269e0c380ff4858` then passed actual embedded Chromium checks at 1440px and 390px on the first attempt. Real API-seeded current and historical revisions preserved three non-navigable links and one safe link; both views displayed the unavailable labels and safe anchor. All 55 logical tables, 567 tracked file hashes, embedded assets and tested executable stayed unchanged; browser domain writes and external/provider calls were zero. The tested CGO-free daemon SHA-256 was `aab51331ae064bbb1461c6eb6cce20c694457ae2561b27e51fa8848d56b4b3cc`. This is local synthetic proof, not installed deployment or physical-phone validation.

## Shared browser workspace

The app shell and map page share their session context, paging and read-state helpers through a separate workspace module. The module does not import either page.

Clean checkpoint `2d91c8bed4d9cfd1649ecd87750a10fd00ee5ba1` passed TypeScript and all 201 browser tests, then actual packaged Chromium checks at 1440px and 390px. Current and retained map revisions preserved the safe anchor and three unavailable-link labels. All 55 logical tables, 569 tracked file hashes, embedded assets and supplied inputs stayed unchanged; browser domain writes and external/provider calls were zero. The tested CGO-free daemon SHA-256 was `6ba4716fa4ff50f62002bacdd1ab6ca2f36d24aebaaeae1d5a61b4a4f8f0d881`. This checks behavior after removing the import cycle; it does not establish installed deployment or physical-phone behavior.
