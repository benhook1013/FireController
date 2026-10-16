# Browser Coordination Views

## Implementation status

Read-only browser views over the existing [notes](notes.md), [worker pause](worker-pause.md) and [primary-job focus](worker-focus.md) contracts are implemented. TypeScript and focused client/component tests pass; actual packaged-browser and full integrated validation remain outstanding. This slice changes no domain state, assignment, message handling or provider behavior. Human GUI review and operational cutover remain separate.

## Explicit reads

An explicit worker-details route loads that worker's identity, current pause and current primary-job reference in the connected project. It displays pause and its literal reason as an explicit coordination choice, never inferred presence. Primary focus links to the selected job without loading its brief automatically. Default unpaused and absent-focus states remain clear. Worker lists gain a separate details link without changing their existing inbox selection or adding pause/focus requests per listed worker. Pause and primary-job histories load only when opened, one newest-first page at a time.

A Notes navigation item opens project-scoped pending notes. Explicit worker, job, phase, kind and status filters compose with AND; an optional assigned-jobs expansion requires a worker selector and broadens only that worker predicate. Filter changes reset pagination. Worker and job detail pages link to explicitly scoped note lists. The project page does not fetch notes, notes do not imply ownership through assignment, and notes never become inbox messages or standing instructions.

The `notes` heading uses the same project presentation record as the other navigation headings. Adding this view introduces no separate branding configuration or translation system.

List pages show twenty compact previews with explicit truncation and next/previous navigation; they never expand full bodies or archives automatically. Selected note pages display the exact current body, all scope selectors, kind/status and dismissal reason. An opened history shows compact attributed rows; selecting one revision fetches its immutable full content with a historical label and a link to current state. Opening or refreshing a note never consumes or dismisses it.

Every query binds the existing connection nonce, instance and project, plus the selected resource and effective filters. Resource or filter changes remount their paging state. Old connection work is cancelled and cannot populate a new project's view. Responses must validate identities, required fields, known enums, positive safe revisions, bounded text/previews and paging before rendering. Unknown, malformed and foreign results fail visibly. Rich note/reason text is escaped plain text; it never supplies HTML, links, commands or authority.

All new requests are finite reads under the existing cancellation and response budgets. No automatic provider refresh, JSON polling, note consumption or new live feed is added. Existing message and project metadata feeds retain their narrow scopes; these views update through the explicit visible-data refresh. Inactive histories and other worker details are not fetched. The existing project theme, typography and responsive components remain the default.

## Proof boundary

Focused browser-client tests must cover route/filter encoding and local validation before fetch, strict current/compact/history/full-revision parsing, malformed or foreign facts, escaped text and independent focus/pause revisions. Component tests cover literal current versus historical rendering, truncation and scope labels. Actual packaged Chromium proof must exercise read-only connection, deliberate paging/history selection, worker/job deep links, explicit refresh after controlled mutations, reload and project switching, narrow-screen bounds and unchanged domain/message state. Record failures honestly; simulation tests do not establish React lifecycle, real cookies or visual acceptance.

## Focused proof

The complete browser check passes 222 tests, including eight new API tests and six component tests. They cover exact scoped GETs, default and guarded filters, paging/response bounds, malformed or foreign records, individual timestamps after clock changes, cancellation, literal note/reason text, current versus historical attribution, truncation and scope links. Presentation model/domain race tests also pass with the new project-configurable Notes heading. These are simulated HTTP and static-render checks, not executed React lifecycle or human visual acceptance.
