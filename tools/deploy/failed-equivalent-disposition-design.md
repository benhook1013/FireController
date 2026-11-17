# Narrow failed-rehearsal duplicate disposition

Design only, 2026-11-17. No production activation or historical cleanup. This is a bounded extension to the existing producer lifecycle, not a general collector, age rule or broader deletion authority.

## Why this narrow case is worthwhile

Live read-only evidence found a 428,281,856-byte historical failed rehearsal DB byte-identical to its preserved canonical source. Its attempt has started.json and exit1, no terminal.json. A second successful rehearsal has different bytes and is not disposable by this rule. The current bounded producer already removes its newly produced identical successful output and refuses a second unresolved workspace. Failed-but-identical output instead occupies the whole slot indefinitely. Handling this one case can recover the slot without discarding novel state or weakening successful-rehearsal proof.

## Contract

1. Only the invocation that exclusively created the operation may elect automatic failure disposition. Never scan/adopt historical attempts, accept an arbitrary path, infer failure from age, or convert a failed attempt into successful verification.
2. The operation layer must return a distinct typed **known-terminal unsuccessful exit** outcome only after child/process-group termination has been established. Supervised handoff ambiguity, launch timeout, wrapper death, surviving descendants, unavailable supervisor receipt or changed lease identity remain unresolved and keep all files.
3. Retain budget and immutable-source custody. Reacquire and verify the same consumer lease identity; no later consumer may start outside that shared custody. Existence of an unlocked file alone is not proof a supervised process cannot start later.
4. Bind operation/root, original source/runner hashes and paths, source revision, start-record hash, work-directory identity and the specific output inode from this invocation. All must still match before disposal. Reject symlinks, hard links, unexpected children, SQLite sidecars, changed source/runner, source overlap and any externally selected output.
5. Compare the complete output bytes with the still-present verified canonical source. If different, preserve it. Source equality preserves all database content; it does not permit removing reports, logs, start/failure receipts, recovery sources or unrelated files.
6. Write and fsync a bounded failure-disposition intent before unlinking only the verified equivalent DB. Retain the canonical source and all failure proof. Record nonzero exit status, original failure classification, exact source/output digest and bytes, original ownership identity, then fsync the containing directories and an explicit disposition receipt. Return the original failure, never success.
7. A future admit pass may free the one-workspace slot only after validating this failure receipt and exact absence of the redundant output, with no unknown files or active consumers. It must not require a passed terminal receipt for this explicitly failed disposition, but it must not accept an arbitrary failed.json either.
8. Interruption before durable completion stays fenced. A narrowly qualified repair can reconcile an exact intent with an absent output and still-identical canonical source; absence alone is never success. If receipt publication fails, report retained/ambiguous cleanup status without changing the failure into a pass.

## Deliberate exclusions

- Historical bf816 files remain untouched. Current evidence supports only a candidate, not final custody or permission to remove it.
- Changed v2 output, canonical recovery source, current651e, conservatively protected predecessor107c55 and prepared/deferreda749 remain protected.
- No recursive work-directory cleanup, log trimming, captures cleanup, image pruning, timer installation or global release-graph expansion.
- No automatic deletion on arbitrary Python exception. Only the exact known-terminal outcome can reach this branch.
- No unconditional timeout relaxation or alteration to primary/offsite backups.

## Smallest implementation/qualification gate

First add typed terminal-failure evidence without changing existing disposal. Test foreground nonzero exit, supervised known terminal vs ambiguous handoff, descendant survival and lease replacement. Then add opt-in producer-owned equality disposition in an isolated worktree, keeping default/current deployment unchanged until independent review.

Tests must cover identical failed output eligible while original failure/proofs survive; changed output remains; source/runner mutation; sidecars/hardlink/symlink/foreign file; no ownership adoption; busy/replaced lease; unknown supervision outcome; actual child process restart before/after intent and unlink; fsync/report failures; subsequent admission after an exact durable failed-disposition receipt. Include an actual failed runner, not only mocked return codes. Every test is synthetic and must preserve its equivalent recovery source.

Operational adoption requires exact actual producer callsite/hash, independent review, a concrete policy decision and one permitted rehearsal showing the old behavior preserved for all excluded cases. A broad recurring collector is not a prerequisite for this bounded producer extension. The full9dac database/filesystem audit and install/recovery dependency closure remain open independently.
