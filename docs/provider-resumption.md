# Provider Resumption After Restore

## Implementation status

The local reconciliation helper, operator command and release provenance are implemented. Focused normal/race checks cover the boundaries below; the full integrated build and packaged recovery proof remain pending. Coordination activation and its durable provider hold already have their separate proof. This document does not claim completed operational recovery or cutover.

## Operator boundary

A restored instance resumes provider work only through the explicit local operator command `fcd restore-resume-providers`. Local filesystem authority is required. Stop the destination daemon first, fence the predecessor, and independently reconcile all external provider effects, including the possible interval after the selected backup. Every relevant provider action must have ended or been otherwise settled before making this attestation. The command records the operator's assertion; it does not contact, stop or independently verify another process or provider.

The command takes the expected restored instance, exact fenced predecessor, explicit `--provider-effects-settled` and `--abandon-retained-work` assertions, a nonblank reason and an evidence reference. Reason/reference are private retained plain text, at most 256 UTF-8 bytes each, without controls. The command never dereferences the reference, loads credentials/catalogs, runs Git or invokes a provider. It refuses ordinary and inactive installations and mismatched recovery identities. Existing operator duration bounds apply.

## Atomic local reconciliation

One transaction retains the original activation identity and cancels the instance's retained pending provider work before releasing the hold. Queued reviews without attempts become held with `restore_reconciled`; preparing, posting, active and uncertain attempts become cancelled, preserving their original anchor, intent, external ID, evidence and complete transition history. Posting passes through uncertain within the same transaction. Only the corresponding verified reservations are released. Existing terminal reviews, source decisions, capture bytes, routes and imported history remain unchanged; no review is marked completed or finalized.

Queued/running source refreshes become held without source/CI/readiness results. Every unpublished checkpoint publication becomes cancelled; its original payload, unposted/issued distinction, intents and revisions remain retained. Cancelled publication is terminal and cannot be recovered or reposted through the ordinary recovery API. These are local abandonments after the operator's verified settlement, not external cancellation requests or claims that no POST occurred. A hosted allowance query may already have posted while its review attempt was preparing; its intent and captures are retained.

Every unpaused project's latest execution configuration receives a new immutable paused revision and configuration event. An already paused configuration stays unchanged. An explicit fresh configuration change is required before new review dispatch. Old keys and profiles remain bound to their original instance; release does not reauthorize or replay them. New source reads still require an explicit current-instance request. The operation handles at most 10,000 selected records in each work family and 10,000 configured projects; exceeding a bound or finding inconsistent retained state rolls the entire transaction back.

Present work domains must have the current schema. If an older supported bundle needs migration, first run the current daemon while the provider hold is still in place, stop it, then retry the operator command. The command itself never initializes or migrates a domain.

## Release record and startup

The current recovery metadata keeps its coordination-active state and activation credential identity. `provider_state` changes from held to released and one canonical versioned private release record binds the instance, predecessor, bundle, activation time, reconciliation time, exact assertions/reason/reference and reconciliation counts. The complete archived metadata must fit the existing 4 KiB recovery-history row budget, including JSON escaping, before commit. No capture or full pending-work payload is copied into this compact record.

An exact retry returns the historical release result without touching access, configurations or newly created work. Changed arguments conflict. The release timestamp never predates activation. A lost result is recovered with the exact same request; no new key or action is invented.

The operator restarts with freshly supplied provider configuration. The command does not hot-start workers. Startup may then construct the explicitly configured executors/observers, while the paused project configurations prevent automatic dispatch. Release neither restores credential files nor proves live provider authorization or correct deployment configuration.

An invocation after independently completing those operator checks is:

```sh
fcd restore-resume-providers --data-dir RESTORED --expect-instance NEW_INSTANCE --fenced-predecessor SOURCE_INSTANCE --provider-effects-settled --abandon-retained-work --reason 'External effects reconciled' --evidence-reference 'private-recovery-record'
```

Backups retain this release provenance. A later restore archives the exact prior held or released generation, creates a fresh inactive identity and revokes copied credentials. Coordination activation of that generation starts held again. Old release records never grant release to another instance. Managed backup scheduling identifies ordinary installations separately from restored generations; a released generation cannot adopt a predecessor's schedule by appearing ordinary.

## Required proof

Proof must cover refusal and whole-transaction rollback, every pending work family and issued-query distinction, original reservations/history/captures/receipts, paused configuration revisions, unknown outcome/exact replay, malformed and oversized metadata, clock rollback, held/released generations and restore-backup-restore, daemon held versus explicitly resumed startup, and a real packaged local recovery with zero external calls during reconciliation. Evidence from controlled fixtures does not establish a real external settlement, an independently verified predecessor fence, fleet scale or operational cutover.

## Focused validation

The reconciliation helper and affected publication/client tests passed focused race suites in 30.403 and 1.278 seconds. They cover preparing and admitted reservations, an already-issued allowance query, unknown-state refusal, late corruption/capacity rollback, native attempt projection, preserved captures/intents/history, configuration events, already-paused replay and source-clock rollback. Cancelled publications retain both known-unposted and issued-intent distinctions; normal recovery and restart start no replacement action.

The final backup resumption tests passed normally in 9.575 seconds and under the race detector in 54.287 seconds. They cover exact replay after later work and credential revocation, concurrent operators, escaped 4 KiB archive limits, clock rollback, malformed/oversized metadata, released-generation backup/inspection/restore and scheduling ownership. A real cancellation regression showed that the SQLite driver's busy wait could delay manual cancellation. The operator now retries only acquisition of its immediate transaction, with cancellable ten-millisecond waits and a five-second bound; it never retries a reconciliation mutation.

The operator command passed focused race checks in 11.842 seconds after the final helper freeze. Actual restored-state calls cover strict pre-filesystem arguments, held versus released startup, original-result recovery after failed or short output, exact replay and changed-input conflict. Scoped vet, formatting and documentation/link checks passed. These are in-process real SQLite/command/API checks. Full packaged recovery remains a separate pending boundary.
