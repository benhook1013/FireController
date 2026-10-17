# Legacy controller adoption

## Implementation status

The optional [legacy migration utility](../tools/legacyimport/README.md) implements bounded read-only inspection of an explicitly supplied standalone SQLite export and preparation of selected current job snapshots/checkpoints. The native administrator API and bound CLI implement guarded preview/apply for that prepared job format. Focused synthetic checks cover source preservation, public reads, current authorization, replay and rollback; packaged end-to-end and backup integration proof is still in progress. Messages, notes, updates, checklists, full prior job histories and the [review round controls](review-round-controls.md) remain implementation work. Prepared review and project-map imports already have separate guarded application paths. Native backup restore and native schema upgrades do not convert a legacy controller.

## Selected job snapshot import

The version-one prepared batch contains `version`, positive `repository_id`, explicit `expected_revision`, a stable `source` namespace, sealed `source_sha256` and one to twenty-five `jobs`. Each job contains an opaque unique `source_key`, original `source_revision`, title/brief/status, an explicitly mapped native `worker_id`, original creation/update timestamps and selected checkpoints. There are at most one hundred checkpoints per batch, and the complete normalized apply wrapper fits 256 KiB. Ordinary native text/byte/time constraints apply; a source document that needs summarizing or editing is refused rather than silently rewritten.

This format imports an explicitly selected current job snapshot as native revision 1. Original source revision remains in provenance; this is not a reconstruction of every prior native revision. Selected checkpoints retain their source keys, original text, timestamp and input order. Their native `job_revision` is 1, the selected imported snapshot. A source without recorded checkpoint authors uses the reserved `imported-history` attribution with no actor worker, explicitly meaning unavailable original attribution. It does not guess authors from current assignment. Original named source pointer JSON is retained as inert evidence text when it fits native limits.

`GET /api/v1/projects/{project_id}/coordination/imports` returns the compact current import revision, initially 0. Administrator `POST .../coordination/imports/preview` accepts the prepared batch and returns selected/new/unchanged counts with payload and preview fingerprints. `POST .../coordination/imports/apply` accepts `{preview_sha256,batch}` and an explicit idempotency key. All require current administrator authority and explicit importing-worker context. Preview binds content, instance, project, principal and worker. New apply rechecks repository identity, import revision, worker references and source mappings in the ordinary two-second transaction. It calls no provider.

Stable source identities produce project-bound native job/checkpoint IDs. Source mappings, normalized originals, sealed source digest, original source revision and import actor/time remain private durable provenance. Existing mappings accept identical source items without overwriting later native edits; conflicting items refuse. Same-key exact replay returns the original compact receipt before checking a newer import revision, with current authorization still required. A new-key unchanged import records a receipt without advancing import revision, duplicating records or emitting another event. A changed import commits native records, provenance, revision, receipt and one compact `coordination.imported` metadata event atomically. Active job views refresh; historical message-delivery events are never replayed.

The bound administrator commands are:

```sh
fc import state
fc import preview --file /private/import/jobs.json
fc import apply --file /private/import/jobs.json --preview SHA256 --key selected-import-key
```

The CLI checks strict bounded private input before credentials or service access and uses the ordinary five-second client budget. Keep the exact file, fingerprint, key and binding for uncertain-output recovery. These setup operations do not add worker MCP mutation tools or make imported source history native review coverage.

## Retention and conversion

Import useful selected project state rather than requiring every historical detail. The operator selects the source export, destination project and historical scope. Preview reports selected, excluded and unsupported record classes explicitly. Never silently truncate a brief, invent a missing author/time, convert an instruction note into another kind or mark old messages handled to simplify migration. Keep original source bytes privately with their digest when a selected fact cannot be represented faithfully.

The source-specific converter lives under `tools/`; the daemon accepts a versioned native prepared format through reusable administrator-only facilities. It must not depend on legacy table names, paths, Python modules or installed project conventions. Ordinary job creation and message sending are not historical import APIs: they allocate new identities/times and have current operational effects.

Selected coordination records preserve stable source keys, original timestamps, worker attribution, job state, brief/checkpoint/update/checklist relationships and message reply/handling facts where representable. Native identities can differ, but durable explicit source mappings must make retries and references unambiguous. Names are display data, not implicit identity matching. Source authors that are not known workers need an explicit mapping or an unsupported-record result. Unselected parents/dependencies must be included explicitly or cause a visible conflict; do not quietly fabricate them.

Historical text is inert imported data. Import does not send messages again, acknowledge an inbox, resume a worker, run a provider, publish private information or turn a historical review into native captured evidence. Current primary-job, pause and appearance settings are deliberate configuration choices, separate from retained historical facts. Imported operational review settings follow the selected round-control contract; retrospective allocation/credit bookkeeping can remain in the private archive.

## Guarded application

Preview is read-only and binds normalized content, source provenance, destination instance/project and current administrator/worker identity. Apply uses an explicit idempotency key and rechecks current authorization, conflicts and expected destination state within the write transaction. Same-key exact replay returns the retained result; changed content or conflicting mappings refuse without partial effects. Imported source keys cannot overwrite newer ordinary edits. Bounded batches and explicit continuation make large imports resumable without treating a partial batch as complete coverage.

Current/history rows, source mappings, import provenance and receipt commit together. Historical source times remain separate from application time. Emit only the compact import notification needed to refresh affected native views, rather than replaying historical message events to attached agents. Imported pending coordination must remain deliberately discoverable through the supported native context/inbox reads. Public visibility stays unchanged.

## Required proof before adoption

Use synthetic legacy exports to prove conversion, exact retry/restart, changed-input refusal, dependency/conflict handling, rollback, source conservation, original time/identity attribution, byte-limit failures, project isolation and no provider/send/publication side effects. Exercise the actual optional utility and packaged service/CLI, then inspect populated browser views. Rehearse an explicitly authorized real-project load separately before cutover; inspection success, synthetic proof and review approval do not authorize that load or operational replacement.
