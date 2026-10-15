# Managed Backups

## Implementation status

Local operator bundle creation, read-only inspection and inactive restore are implemented. Full checks and five packaged proof groups pass as recorded below. Managed scheduling, retention and read-only operator status have focused checks; their full integrated and packaged proof are pending. Activation and provider reconciliation remain unfinished. This is not yet an installed backup or disaster recovery workflow.

## Canonical data and local format

A local version-one bundle is a private immutable directory named `backup_` plus an opaque identity. It contains a self-contained consistent SQLite snapshot, canonical manifest with SHA-256 sidecar, and an inventory of immutable files from the managed `review-captures` and `hosted-review-captures` stores. Include final capture files published before their ledger insert as retained forensic evidence; their presence does not establish review credit. Validate references from native evidence and hosted observations, including allowance baselines. Preserve database resource IDs, messages, jobs/history, receipts, events, review decisions/routes and imported history.

The manifest binds source instance, snapshot start/end window, the snapshot's event sequence, database size/hash and sorted artifact paths/sizes/hashes. It does not invent an exact provider recovery time or include provider bodies in ordinary output. A completed bundle is published only after validation and durable writes; partial staging never becomes a usable backup. Local files remain private and destination paths cannot target the source data directory. Inspect verifies hashes, SQLite integrity/foreign keys, supported schema and artifact references without modifying the bundle. Unknown or modified SQLite triggers and foreign-key definitions are unsupported; the two exact canonical review-revision triggers and supported schema versions remain valid. Hidden/generated columns are not part of this format. The first format rejects source snapshots with restore metadata until multi-generation activation and receipt-origin support exists.

Operator credentials, bootstrap token files, profiles, executables and external catalog/authentication directories are not backup artifacts. Git source caches and derived executor workspaces are operator runtime state rather than retained review evidence. Shared-clone alternates are not copied or represented as self-contained artifacts. Restore does not resume admitted executor work; re-provisioning and exact-source verification are prerequisites to any later executor activation. Missing source stays an explicit hold.

## Inactive restore

Restore accepts a verified bundle and creates a new private destination, never overlays existing data. It preserves canonical records and captured evidence, generates a new instance/signing identity, and revokes every restored API credential before the result can be used. Original grants and credential history remain retained; access is not silently revived. Record the predecessor instance, bundle and recovery window in restored metadata so pending work and receipts keep their originating provenance.

The first restore result is explicitly inactive. Daemon startup rejects an incomplete restore marker and an inactive restored database before worker startup or serving. No bootstrap credential or automatic provider resumption is created by this slice. Original provider intents stay retained and possible post-backup effects remain unresolved. Normal restart of an ordinary installation keeps its instance identity.

Activation requires the separately implemented fence/recredential/rebind/reconciliation boundary in [architecture](architecture.md#storage-operations-and-growth). Publishing an inactive restore does not claim that this workflow exists. Old profiles, cursors and retries cannot silently continue against a new instance; a future activation must preserve receipt origins rather than relabel old keys.

## Operator commands

The operator commands are:

```sh
fcd backup --data-dir SOURCE --destination BACKUP_ROOT
fcd backup-inspect --bundle BUNDLE_DIRECTORY
fcd restore --bundle BUNDLE_DIRECTORY --data-dir NEW_DESTINATION
fcd backup-status --data-dir SOURCE
```

Each command accepts `--timeout DURATION`, defaults to five minutes and permits a positive duration up to twenty-four hours. Duplicate/unknown flags, positional arguments and invalid durations fail before filesystem work. The operator opens its live source read-only without initialization, migration or writer admission. A backup source must already be a valid private installation; a wrong path cannot silently bootstrap a new database. Restore never overlays an existing destination, including an empty directory.

Commands use explicit local filesystem authority, bounded execution and compact JSON; they never load a provider catalog or make network calls. A caller may retain a long local command in its host background. These are operator actions, not per-worker coordination calls or a reason to return pending receipts for quick local mutations.

Use a short private destination path. The bundled SQLite VFS imposes a smaller database-path limit than the filesystem; generated bundle and database names also consume that space. An unsupported destination remains a reported backup failure rather than a successful backup.

## Managed scheduling and retention

Scheduling is an explicitly configured daemon feature. The operator supplies `--backup-destination`, `--backup-interval` and `--backup-keep` together; no destination or automatic schedule is invented. The interval is one minute through thirty days and the retained count is one through one thousand. Optional `--backup-timeout` defaults to five minutes and accepts the same positive maximum twenty-four hours as the manual command. Invalid or incomplete configuration fails before creating installation state, including destinations whose encoded status could exceed the output budget. One active service replica remains the supported deployment.

The daemon owns one serial backup worker using the existing store's read pool. After an attempt settles, it waits the configured interval; elapsed periods do not build a queue of catch-up work. On restart a due schedule can run immediately, and an interrupted prior attempt remains a failure rather than invented success. Shutdown cancels and joins the worker before closing storage. Destination failures do not stop ordinary coordination or readiness.

A small versioned local ledger retains configuration, last attempted outcome, last verified success and managed bundle identities. A failed attempt preserves the previous success. Retention failure is reported independently of successful creation. `fcd backup-status --data-dir SOURCE` reads compact stored operator facts without snapshotting, hashing all bundles, calling providers or changing state. Stored success identifies a past verification; `backup-inspect` verifies present bytes. Status output is bounded to 16 KiB so long configured destinations and retained success locations remain reportable; manual backup, inspection and restore summaries keep their 4 KiB bound. It is not a per-worker tool or an automatic notice attached to unrelated calls.

Retention runs only after a newly verified managed success and affects only ledger-owned bundles for that source instance and selected destination. Preserve at least the configured count, including the newest verified copy. Manual, foreign-instance, changed, unrecognized or unverified directories are never adopted or deleted. Verify every protected keeper before pruning older copies, then reinspect each deletion candidate against its stored identity and manifest binding. Suspicious or partial remnants remain explicit cleanup failures; a failed backup does not start pruning. Destination transfer and restore activation remain separate boundaries. These scheduling and retention controls are implemented with focused checks; full integrated and packaged proof remain pending at this checkpoint.

## Functional proof

Prove online SQLite snapshot consistency while writes continue; self-contained reopen after removing the source; complete database/reference/artifact validation; unreferenced final-capture preservation; atomic failed publication and no replacement of existing destinations; privacy/symlink/path/hash/schema failures; source unchanged except intended writes; cancellation; inactive restore with new instance and revoked credentials; original IDs, receipts and source provider intents retained; daemon refusal before listener/worker/provider work. Prove actual packaged commands and ordinary startup independently. Also prove serial scheduling and restart without catch-up, separate creation and retention outcomes, read-only status, unchanged ordinary coordination during blocked or failed backup work, and preservation of valid older copies when protected keepers are damaged or missing. These functional goals do not assert fleet throughput, cloud durability or a completed operational disaster cutover.

## Proof status and limits

Focused proof lives in [core snapshot tests](../internal/core/snapshot_test.go), [bundle tests](../internal/backup/bundle_test.go), [schema and capture-reference tests](../internal/backup/inventory_test.go), [inactive restore tests](../internal/backup/restore_test.go), and [operator command tests](../cmd/fcd/backups_test.go). Clean runtime checkpoint `35aa2b69bb7bd18177c56e74f2c9d2a29b8ded90` passed full `make check build`: 175 browser tests, all Go race tests and vet, documentation/link checks, embedded browser assets and three binaries. Focused tests cover thirteen older component-schema combinations, full rehashed foreign-key cascade rejection before restore writes, and a live held writer while the operator snapshots the committed prefix without writable initialization.

Five actual packaged groups passed in 1.673 seconds using nineteen `fcd` and nineteen `fc` invocations with isolated canonical data. Six immutable artifacts totaling 45,923,657 bytes survived exactly, including referenced hosted observations and unreferenced CLI forensic captures. Read-only inspection and self-contained inactive restore with the source offline preserved jobs, messages, receipts, events, native evidence/routes, imported history and provider intents. Only intended instance/signing/restore metadata and credential revocation changed. Startup refused the inactive result before catalog or listener work; ordinary source restart retained identity.

The backup command took 91.721 ms while nine coordination mutations completed. None completed wholly within its short SQLite snapshot window, so this packaged group does not establish strict snapshot/write overlap; the focused transactional snapshot test covers that boundary. Read-only inspection took 29.940 ms and inactive restore 113.039 ms. These small local samples are not throughput guarantees. Corrupted hashes/references/schema/triggers, wrong source paths, existing destinations, incomplete startup and deadline cleanup failed safely. Source and binary hashes remained unchanged; fixture processes, listeners and state were removed, with zero external/provider/Git/review/allowance calls.

Two initial private-harness failures were preserved before fixing an eager diagnostic read and a local nanosecond timestamp parser. They required no production change, timeout relaxation or weakened assertion.

Focused [scheduler](../internal/backup/managed_scheduler_test.go), [worker isolation](../internal/backup/managed_worker_test.go), [retention](../internal/backup/managed_retention_test.go) and [daemon command](../cmd/fcd/backup_schedule_test.go) checks cover the managed local controls, including real escaped-path output and rejected encoded configurations before state changes. Full combined checks and packaged scheduled-backup proof are pending.

This slice addresses the snapshot and inactive-copy portions of O02 and O06 in [acceptance](acceptance.md). Managed local scheduling and status address the local portion of O07; optional destination-transfer proof and active recovery remain unfinished. No destination transfer, activation command, provider reconciliation, browser backup control or extra agent tool is delivered here. Local bundles remain under the operator's filesystem authority and are not encrypted by this format; use storage protection appropriate to the installation. Windows and macOS runtime durability and access-control behavior remain separate proof.
