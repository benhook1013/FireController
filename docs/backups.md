# Managed Backups

## Implementation status

Local operator bundle creation, read-only inspection and inactive restore are implemented with focused tests. Full checks and packaged-command proof are pending below. Scheduling, retention, activation and provider reconciliation remain unfinished. This is not yet an installed backup or disaster recovery workflow.

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
```

Each command accepts `--timeout DURATION`, defaults to five minutes and permits a positive duration up to twenty-four hours. Duplicate/unknown flags, positional arguments and invalid durations fail before filesystem work. A backup source must already be a valid private installation; a wrong path cannot silently bootstrap a new database. Restore never overlays an existing destination, including an empty directory.

Commands use explicit local filesystem authority, bounded execution and compact JSON; they never load a provider catalog or make network calls. A caller may retain a long local command in its host background. These are operator actions, not per-worker coordination calls or a reason to return pending receipts for quick local mutations.

## Functional proof

Prove online SQLite snapshot consistency while writes continue; self-contained reopen after removing the source; complete database/reference/artifact validation; unreferenced final-capture preservation; atomic failed publication and no replacement of existing destinations; privacy/symlink/path/hash/schema failures; source unchanged except intended writes; cancellation; inactive restore with new instance and revoked credentials; original IDs, receipts and source provider intents retained; daemon refusal before listener/worker/provider work. Prove actual packaged commands and ordinary startup independently. These functional goals do not assert fleet throughput, cloud durability, scheduled retention or a completed operational disaster cutover.

## Proof status and limits

Focused proof lives in [core snapshot tests](../internal/core/snapshot_test.go), [bundle tests](../internal/backup/bundle_test.go), [schema and capture-reference tests](../internal/backup/inventory_test.go), [inactive restore tests](../internal/backup/restore_test.go), and [operator command tests](../cmd/fcd/backups_test.go). Packaged-command proof and full checks are pending at this checkpoint.

This slice addresses the snapshot and inactive-copy portions of O02 and O06 in [acceptance](acceptance.md). It does not satisfy O07 scheduled-backup status or claim active recovery. No daemon schedule, retention policy, destination transfer, activation command, provider reconciliation, browser backup control or extra agent tool is delivered here. Local bundles remain under the operator's filesystem authority and are not encrypted by this format; use storage protection appropriate to the installation. Windows and macOS runtime durability and access-control behavior remain separate proof.
