# Bounded failed-rehearsal lifecycle: adoption and recovery

## Candidate boundaries

The helper candidate remains unactivated. Its deployment-tool subtree applies without source drift to live application source c43eba23824a530ce869f2a3b4fb3faeec73dc1e; no application binary, database schema, backup cap or backup format change is required. There is no new PR. Existing default behavior is unchanged.

For a future bounded rehearsal, the explicit configuration `failed_equivalent_cleanup: true` enables both retained supervisor observation and equivalent-copy disposal. The preparer refuses missing or changed runtime helpers against the selected package manifest. The runtime closure is `artifact_retention.py`, `rehearsal_operation.py`, `bounded_rehearsal.py`, `failed_equivalent.py`, `supervised_failure.py` and `supervised_equivalent.py`; the package also needs the updated preparer and its existing successor/template dependencies. Never install only one changed Python file into the active package. Use a new immutable versioned helper directory, verify every manifest hash, preserve the current package as rollback, and render a fresh operation with the existing approved deployment supervisor. Do not repoint active or pending operations.

The option does not make a failed rehearsal pass. A completed disposition retains its original failure, canonical source, original runner, start/supervisor/failure proof, durable disposition intent and receipt. Only the two freshly created, byte-identical database and runner copies can be removed. Unknown service outcomes, changed custody, unique output, sidecars or unexpected sandbox artifacts are retained. Historical failed operations are never enrolled.

## Interrupted operation

An intent without the final `supervised-equivalent-disposal.json` receipt remains blocked. This is intentional: do not delete the intent, bypass admission, rerun the old operation or recursively remove its directory. Reacquire the exact operation's budget, immutable-source and consumer leases before investigation. Verify all protected source hashes and current service invocation/cgroup identity; a missing unit or idle lease alone is not termination proof.

Inspect the two named duplicate paths against the durable intent:

- Both present: no retry of deletion is authorized by this protocol; retain the operation for explicit disposition after fresh equivalence checks.
- Database absent, runner present: database unlink happened but runner disposition is incomplete. Preserve the runner and original inputs. The next bounded rehearsal remains fenced.
- Both absent, receipt missing: an unlink or synchronization may have finished, but completion was not recorded. Preserve all evidence; there is no automatic supervised settlement adapter yet. Do not synthesize a receipt from absence alone.
- Complete, valid receipt: the bounded producer accepts the retained small proof/scaffolding as a resolved slot. This does not authorize removing the canonical source, application rollback image or any other operation.

Do not reset a failed systemd unit merely to make a dashboard green. Terminal-unit retirement must be limited to the same captured invocation after fresh verification under the owning operation's custody; changed or uncertain units remain unresolved. Existing systemd observation is not a general service-cleanup tool.

The isolated suite covers real child exits after intent, each unlink and completed receipt, plus injected directory-sync and receipt-write errors. It proves source preservation and conservative blocking under those failures, not arbitrary host-power-loss simulation. Production adoption still requires final exact package review, a guarded canary rehearsal and explicit enablement of this default-off option. The full filesystem/database storage job remains open.

## Reproduce the isolated sandbox qualification

Build `tools/deploy/fixtures/failed_rehearsal_runner.go` with the existing approved Go toolchain and `CGO_ENABLED=0`, producing a root-owned static executable outside the disposable sandbox. This fixture only verifies the bind-mounted consumer lease device/inode, acquires it, waits briefly and exits with status 7. It has no production input or network dependency.

The privileged tests are explicit opt-ins: set `FC_ROOTDIRECTORY_RUNNER` to that executable for `test_rootdirectory_failure.py`, `test_rootdirectory_disposal.py`, `test_supervised_disposal_interruptions.py` and `test_packaged_failed_cleanup.py`. They create random disposable local systemd units and synthetic files, and clean only those units. The ordinary test suite intentionally skips these without the variable. Do not supply a production migration runner or live database as fixture inputs.
