# Legacy controller adoption

## Implementation status

The optional [legacy migration utility](../tools/legacyimport/README.md) implements bounded read-only inspection of an explicitly supplied standalone SQLite export. Prepared review and project-map imports already have separate guarded application paths. Historical coordination conversion/application and the [review round controls](review-round-controls.md) remain implementation work. Native backup restore and native schema upgrades do not convert a legacy controller.

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
