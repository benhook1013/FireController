# Offline SQLite storage inventory

`python3 tools/diagnostics/sqlite_storage.py --offline-snapshot /absolute/path/snapshot.sqlite` reports per-table/index page allocation, payload and unused bytes, plus database page/freelist totals. Use only an existing quiescent offline snapshot. This command does not create a backup or copy the database; do not point it at the live service database. No production inventory is established by the synthetic tests.

The immutable read-only connection creates no journals or shared-memory files. Existing WAL, SHM or journal sidecars refuse inspection rather than silently ignoring changes; symlinks and observed source changes also refuse. Keep the snapshot immutable for the whole invocation: stat checks do not replace custody against concurrent writers. Object names are included; row values and SQL definitions are not. Treat output as private operator data.

Inspection has a five-second default SQLite progress deadline, at most thirty seconds, and at most 1,024 reported objects. This output limit does not bound the preceding aggregate scan or impose a hard memory cap; use external memory/process limits for large or untrusted snapshots. Unsupported `dbstat`, interruption, corrupt input or incomplete evidence reports unavailable and exits nonzero, never partial success. The SQLite progress deadline cannot interrupt an operating-system disk read; use normal external process limits for an unresponsive filesystem. No checkpoints, VACUUM, row deletion, retention changes, deployment or background schedule are performed. Free pages and unused bytes are measurements, not authority or advice to delete data. Filesystem allocation, live WAL growth and semantic retention remain separate parts of the broad disk/database investigation.

Run `python3 -m unittest discover -s tools/diagnostics` for synthetic accounting, read-only conservation, sidecar refusal, deadline, object-budget and changed-source tests.

## Table and index attribution

The offline SQLite report includes each object's owning table and type, plus `tables` ranked by total allocated table-and-index bytes. Explicit and SQLite-generated indexes are charged to their actual schema owner, rather than guessed from their names. Table pages and index pages stay separate; free pages are not assigned to tables. `unattributed_bytes` reports any dbstat objects without a recognized table/index mapping. Trigger definitions, SQL and row values are never returned. Names remain private operator metadata.

This makes a dominant database producer visible without creating another database copy or changing retention. It does not identify expendable records, imply histories/receipts may be deleted, or measure live growth. WITHOUT ROWID tables retain SQLite's actual page attribution. Existing snapshot custody, deadline, output-budget and memory caveats still apply.
