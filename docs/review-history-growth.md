# Retained Review History

## Implementation status

The current review projection copies snapshot-qualified history and refuses growth beyond its fixed count limits. Whole-document imports also have a 256 KiB transport bound and cannot preserve mixed producers. The normalized versioned history and bounded transfer described here are not implemented yet. The first implementation boundary adds explicit attempt pages and browser run/attempt continuation as described in the [review workspace](review-workspace.md#bounded-archive-implementation); it retains the existing publication limits until storage and transfer evolve together. Current data remains retained; a capacity hold is not permission to delete history or continue an unrecorded provider action.

## Goal

An ongoing project keeps its completed PRs, review rounds, attempts, decisions and fix evidence without reaching a lifetime review quota imposed by its dashboard. An individual command still reads or writes a bounded amount of data. Ordinary jobs, messages and review reads stay independent of provider calls.

## Historical authority

A stable historical identity includes its project, original producer and source record identity. The transfer publisher is separate provenance. Importing a record whose producer describes a native executor never creates native execution authority or coverage; those claims remain bound to actual local journal operations and captured evidence.

Store immutable versions of changed records and reference those versions from retained workspace snapshots. Do not copy unchanged finding bodies for every unrelated attempt transition. Preserve resource IDs and exact historical snapshot reads, including signed continuations. An old snapshot cannot join to a mutable latest decision or attempt and silently change its meaning.

Finalized counts, run ownership and known finding identities/text remain protected. Enrichment and reviewed decision/fix corrections create another version. Settled attempts cannot reopen, and an existing identity cannot move between PRs, channels, heads or scopes. Explicit superseded/defunct queue retirement changes visibility without erasing historical evidence; merging a PR keeps it visible as completed work.

## Bounded changes and transfer

Native publication updates only its touched historical records and the necessary snapshot references in the same transaction as its journal changes. It performs no provider or filesystem wait inside that transaction. Replace aggregate lifetime count checks with bounded per-operation admission and finite individual record/detail limits.

Historical transfer uses explicit bounded additive batches with stable producer/source keys. Omitted history is retained. A batch binds its selected destination, current revision and normalized content; exact same-key replay returns the original compact receipt under current authorization. A changed request, identity conflict or concurrent revision refuses atomically. Accepted batches can be visible before a larger collection completes, with explicit partial coverage and continuation. An interrupted collection or apply never claims all selected history was imported. Collection remains separate from approval and application.

## Bounded reads

Queue and recent-run summaries remain compact. Run and attempt archives have explicit snapshot-bound pagination, including browser continuation; a PR detail does not return every lifetime attempt. Latest-attempt facts remain useful without downloading that archive. Public views use an explicit bounded history window and coverage/continuation metadata rather than failing after one hundred rounds or silently claiming a complete history.

Native comparison and policy assessment query the relevant evidence set without an unrelated all-head lifetime ceiling. Preserve detection of pending decisions and fixes on earlier heads; a bounded query cannot turn unexamined history into passing evidence. Current policy and native journal/capture authority remain separate from imported display history.

## Migration and proof

Migrate supported retained snapshots and records without changing IDs, producers, counts, decisions, proof or historical bytes. Update schema ownership, canonical backup columns/keys/foreign keys/triggers and logical reference validation together. Restore must retain every referenced historical version and reject missing or cross-project references.

Prove growth beyond current total and per-PR count boundaries, immutable earlier snapshot/cursor reads, finalized identity protection, exact retry, concurrent revision refusal, rollback and restart. Exercise mixed imported/native producers without granting imported coverage. Include actual bounded CLI/MCP/browser pages and public history continuation. Measure touched-record publication and retained bytes on a multi-PR synthetic history; correctness at a higher arbitrary cap is not this proof. The [delivery plan](delivery.md) records completion and installed validation separately.
