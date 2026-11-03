# Retained Review History

## Storage and authority

A project's review history has no lifetime run or attempt ceiling. Each operation remains bounded, and individual imports retain their request, record and text limits. Retaining more history never grants review coverage, allowance or merge authority. The [review workspace](review-workspace.md) owns HTTP reads and observation input; [native execution](review-execution.md) owns trusted publication and capture authority.

Immutable typed bodies hold PR, run, finding, attempt and retirement versions. Snapshot membership intervals select their visibility and order independently of those bodies. An unchanged body is reused across snapshots; an updated decision or fix creates a new version. Historical snapshot reads cannot join to a mutable latest decision. Original project, producer and source identities remain stable. Imported producer names do not create native journal or capture authority.

Current references, aggregate counters, attempt ordering and run ordering advance in the same transaction as the workspace pointer, receipt and event. Native writes stage only touched records and derive compact summaries from indexed candidates. Their previous compact summaries are checked against the indexed winners before replacement. Full startup and backup validation check the complete reference/cache graph; ordinary writes do not claim to detect damage confined to a nonwinning cache row. Derived caches never replace authoritative source evidence.

## Bounded additive writes

[Selected queue edits](review-workspace.md#selected-queue-edits) maintain metadata, relative ordering, explicit retirement and reactivation after a complete-observation document no longer fits. These edits advance the existing immutable history/current references atomically without provider execution. Prepared additive history transfer remains a separate, unimplemented capability in this slice; complete observations retain their replacement semantics.

## Reads and growth

Queue and PR detail contain bounded summaries: three recent runs, one latest attempt per channel, aggregate retained run/attempt totals and completed-run counts per channel. Completed-channel counts exclude failed, cancelled and unfinished run records; they include completed unfinalized reviews and therefore do not establish final counts or current coverage. Native publication and preparation do not count the lifetime archive to decide whether another bounded operation fits.

Run and attempt archives use explicit newest-first pages tied to a retained snapshot. Attempt identities include their original producer. PR detail never embeds the lifetime attempt archive. The browser opens one attempt page when its archive is expanded and requests another only through explicit pagination. Cached pages survive collapse and expansion. Individual run detail still loads findings deliberately. Signed cursors remain project, filter and snapshot bound.

[Public review windows](public-site.md#bounded-review-windows) independently bound CLI and Hosted summaries and mark omitted older rounds. They do not delete private history or imply exhaustive coverage.

## Migration and backup integrity

Startup converts supported original review tables into immutable versions, interval membership and current references without changing IDs, producers, counts, decisions, proof, receipts or historical HTTP responses. Exclusive backfill validates retained facts before deriving current indexes. A failed migration rolls back schema, metadata and derived rows together. Already-current startup does not repeat the backfill.

Backup inspection is read-only. It checks exact supported table/view/index definitions, grouped foreign keys, interval ordering and overlap, owning PR visibility, current-reference completeness, derived counters/order and optional metadata/attempt reasons. Missing parents, foreign-project references, forged caches and malformed historical metadata refuse before restore writes. Restore preserves retained snapshots while rotating instance/signing identity, so predecessor credentials and signed cursors are invalid; newly authorized reads can resume from the same retained snapshot.

## Focused proof

The [migration tests](../internal/reviews/history_migration_test.go) compare historical responses, cursors and receipts across upgrades, restart and injected rollback. [Native growth tests](../internal/reviews/history_native_growth_test.go) retain hundreds of native-seeded records alongside original imported history and publish new native work without duplicating unchanged finding bodies or imposing a lifetime cap. [Projection tests](../internal/reviewexec/attempt_projection_capacity_test.go) exercise actual controlled driver completion beyond former caps and preserve rollback and reservations. [Backup tests](../internal/backup/history_inventory_test.go) create, inspect, restore and activate canonical retained history, and refuse rehashed schema/reference corruption. These controlled checks do not establish live provider behavior, deployment or fleet capacity.

Interval checks use ordered overlap and owner-visibility boundary scans, so repeated publications do not require comparing every prior pair of intervals.
