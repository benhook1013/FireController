# Operator artifact retention

`artifact_retention.py` is an opt-in, bounded Linux helper for redundant operator files, not a general backup, database, capture, image, repository or directory collector. It defaults to dry-run. Nothing is installed or scheduled automatically. Existing unclassified historical directories are not enrolled by age or name.

The installation owns one existing lock shared by every producer, consumer and cleanup invocation. Cleanup takes it exclusively. Readers, launches, reference changes and writers of both the enrolled artifact and recovery copy must participate, so a reference cannot appear after inventory and before removal. Without that integration the utility must remain dry-run. Run in the host process/mount namespace with permission to inventory processes; an incomplete permission inventory refuses collection. Persistent service references that are not currently open must be excluded from enrollment by the operator. A cleanup manifest is private operator state, never provider-controlled input.

Each enrolled file specifies `path`, `recovery_path`, `sha256`, `revision`, `kind` (`build` or `rehearsal-copy`), `terminal: true` and an `expires_at` Unix timestamp. Paths are relative to the explicitly selected canonical root. Both complete files must match the recorded SHA-256 before removal. The recovery file cannot itself be enrolled for deletion. Missing recovery, changed content, symlinks and path escapes refuse collection. Directories are never deleted. Unique historical snapshots, canonical data, private credentials, managed/offsite stores and durable receipts must not be enrolled.

The command requires the actual current revision and a distinct verified usable rollback revision; both are retained irrespective of age. This is an explicit deployment-owner input, not a guess from directory timestamps. Preserve matching helper binaries and restore prerequisites for both releases. Removing older unique data still requires its own approved disposition; this helper deliberately cannot authorize that removal.

Limits are proportional to actual data: at most 1,024 manifest entries and one MiB of manifest, 64 KiB streaming hash buffers, and one GiB of aggregate source-plus-recovery verification per invocation by default. Remaining entries wait for another invocation; no unbounded catch-up loop. `--min-free-bytes` reports an alert but never expands deletion scope. Configure the site's own reserve; Kubernetes eviction/reclaim settings are not a universal Controller disk requirement. The JSON result gives actual free bytes, verified bytes, file dispositions and allocated bytes reclaimed (hardlinked bytes are not counted as freed).

Use `--apply` only after validating the enrollment and producer-lock contract. Keep compact result receipts outside enrolled paths. Schedule and byte/age budgets belong to the installation; a future deployment producer should enroll temporary copies as part of its terminal receipt, preserving current and previous release recovery. This implementation has not been installed on a production node.

## Managed operation workflow

`artifact_operation.py --root ROOT --lock-file EXISTING_LOCK --manifest ENROLLMENT -- COMMAND...` holds the exclusive lease throughout the foreground producer. An exit-zero process with no remaining process-group descendants issues a new private terminal receipt. The enrollment includes a unique top-level `workflow_receipt` filename. The receipt binds the exact enrollment bytes, canonical root and existing lock inode. Failed operations, changed enrollment, existing receipts and background descendants do not receive a terminal receipt. Commands must not daemonize, escape the process group or launch external asynchronous work; such workflows cannot be enrolled. Use bounded producer commands with their existing runtime limits.

All later artifact consumers must use `artifact_operation.py --root ROOT --lock-file EXISTING_LOCK --consumer -- COMMAND...`, which holds a shared lease and cannot create terminal receipts. The command inherits the lease descriptor, identified by `FC_ARTIFACT_LEASE_FD`, so killing the wrapper does not release the lease while its direct command remains alive. Commands that spawn descendants must explicitly preserve that descriptor in every descendant that can outlive its parent; commands without that audited property are not eligible for this workflow. The collector requires the corresponding verified terminal receipt and exclusive lease before applying. Thus newly managed workflows have concrete producer/consumer entry points, rather than merely a comment recommending a lock. Manual legacy workflows and service-referenced files remain outside automatic enrollment until their consumers are migrated. The manifest and receipts must stay operator-owned and unavailable to provider workers; the helper is not an authorization boundary against the trusted operator forging its own state.

Install only once producer and consumer call sites use these wrappers, with a scheduled bounded collector invocation and durable compact JSON results. Do not report automatic cleanup as enabled merely because these source utilities are present. No production workflow or timer has been changed by this implementation.

Run tests with `python3 -m unittest discover -s tools/deploy -p 'test_artifact_*.py'`.

## Zero-copy producer custody before enrollment

Use `artifact_operation.py --root ROOT --lock-file EXISTING_LOCK --producer-only -- COMMAND...` to adopt the existing exclusive producer lease without enrolling any output. This mode rejects a manifest and cannot be combined with `--consumer`. It returns `producer_finished_unenrolled` only when the foreground command succeeds and leaves no process-group descendants; it never issues a cleanup terminal receipt. Failure preserves existing outputs for inspection. A lease alone does not prove recovery or make a file eligible for collection.

For an existing image-preparation pipeline, the owner can wrap its current foreground image producer and synchronous local containerd import in this mode, leaving the original archive path and bytes unchanged. The producer, its child image builder and every local content-read/import/tag/inspection subprocess must preserve the descriptor using `lease_subprocess_options(required=True)` from `artifact_operation.py`. Combine its returned `pass_fds` tuple with any existing deployment-lock descriptors rather than replacing them. Install both this helper and `artifact_retention.py` beside the prepared callsite; isolated Python entry points need an explicit trusted helper import path. Missing or invalid inherited custody fails before child dispatch. All later local archive readers still require the shared consumer wrapper. Remote asynchronous work and persistent image/service references are not made collectible by a completed local import.

This adoption adds no archive copy, no enrollment, no collector, no new timer and no claimed disk savings. Preserve the installation's deployment lock, immutable image/source guards, headroom checks and external time/memory/process-group supervision. The optional artifact lease does not replace them or reserve space against unrelated writers. Archive recovery, producer/consumer installation and any eventual disposal policy remain separate proofs before enabling cleanup.

`test_artifact_unenrolled.py` verifies zero-copy output, manifest/mode refusal, failure preservation, required-descriptor validation and a real nested subprocess handoff. Killing the outer wrapper leaves the nested importer holding the same lease inode until it finishes; no terminal cleanup receipt appears. This is local custody proof, not live containerd, deployment or recovery qualification.

## Opt-in bounded scheduled collector

The supplied `firecontroller-artifact-retention.service` and `.timer` are deployable templates, not an installation action. The service calls `artifact_schedule.py` once, with a two-minute control-group deadline, 128 MiB memory limit and 25% CPU quota. It uses the host process/mount namespace for conservative reference inspection. The timer schedules one check an hour after completion, with jitter; it does not replay missed hours or loop on failure. Inspect journal failures and headroom alerts through the installation's existing operator monitoring. Exit 2 is a completed check with insufficient headroom; it never broadens deletion.

Install all three Python helpers together in a root-owned, non-worker-writable directory matching the service path. Create the private existing shared lease and private result directory explicitly; the runner never silently creates another lease. Choose a dedicated artifact root containing only enrolled reproducible local operation copies and their protected recovery copies, separate from live data, repositories, backups, captures, credentials and service-referenced files. Keep configuration, lock, results and deployment custody state outside this root. Do not enable the timer until the actual producer and all local consumers have been audited for lease preservation. The package-client and Kubernetes subprocess changes preserve local descriptors, but remote Kubernetes work and persistent service references are not eligible artifacts.

The private mode-0600 JSON configuration has this shape (replace every illustrative value with verified installation facts):

```json
{
  "root": "/var/lib/firecontroller-artifacts",
  "manifest": "/var/lib/firecontroller-artifact-state/enrollment.json",
  "current_revision": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "verified_rollback_revision": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "valid_until": 0,
  "apply": false,
  "max_verify_bytes": 1073741824,
  "min_free_bytes": 5368709120
}
```

`valid_until: 0` intentionally refuses collection. Set a finite future Unix timestamp within 24 hours only after verifying current and usable rollback custody. Deployment and rollback owners must acquire the same exclusive lease, update these revisions before changing release selection, and atomically refresh this configuration after actual verification. All manual release selection changes must participate too. Expiry is a fail-closed backstop, not a substitute for that custody protocol. The timer reads configuration after acquiring the lease and retains it through collection. Absent `apply` means dry-run; only explicitly enrolled and qualified installations may set it true. Unknown fields, stale custody, noncanonical paths, unsafe permissions and excess verification budgets refuse the run.

### Concrete enrollment sequence

1. Select a bounded reproducible **local copy operation**, for example copying an already hash-verified build artifact to a temporary rehearsal input. Retain the independent verified recovery file. Exclude original/unique archives, canonical backups and any file serving a persistent or remote consumer.
2. Under the existing exclusive lease, prepare a new immutable enrollment manifest before starting the operation. Bind the exact planned relative source/recovery paths, expected recovery SHA-256, full source revision, expiry, supported kind and a new unique top-level `workflow_receipt`. The producer must reproduce the planned bytes. Do not invent terminal receipts for old or failed operations.
3. Execute that actual foreground copy operation through `artifact_operation.py --manifest ...` under the same lease. Its successful termination creates the bound terminal receipt. A failure or surviving process-group child produces no receipt. Every subsequent local consumer uses `--consumer` and preserves the inherited descriptor through all descendants. Commands that escape this contract cannot be enrolled.
4. While holding the lease, point the collector's private configuration at this completed enrollment and freshly verified protected revisions. Keep `apply: false` first, and verify the exact candidates, retained current/rollback files, independent recovery copies and active-reference exclusions. The collector itself rechecks receipt/hash/protection evidence every time.
5. After the responsible deployment owner qualifies that exact enrollment, enable apply in the private configuration under the lease. The timer can then remove only those verified redundant expired files without inventing broader legacy enrollment. A later operation needs its own bound enrollment/receipt; selecting it does not retire earlier unique records.

Results are compact aggregates, never private filenames or exception text. At most `latest.json` and `previous.json` are retained outside the artifact root, with an exclusive `pending.json` used for fsynced atomic publication. A crash can leave this one bounded pending file; the next run refuses before collection until the owner inspects it. Result files are not archive/audit backups and are not enrolled deletion candidates. Lock contention, invalid custody or invalid result storage fails before collection; use the failed service/journal status when no new result is safely writable. No automatic pruning of historical receipts, recovery copies or unknown files is provided.

Production remains gated on installing these templates, migrating the actual audited copy/consumer callsites, maintaining protected release custody and confirming one dry-run then one authorized exact enrollment. No live timer or cleanup was enabled by the source implementation.

### Existing lease ownership

Every producer and collector opens the same pre-existing canonical absolute lease path. Symlink leaves or ancestors, nonregular files (including FIFOs), a different effective owner, and group/world-writable leases are refused before commands or collection. The collector also revalidates the opened lease before reading enrollment. Lock identity remains bound into the terminal receipt. The root and every ancestor directory must be controlled by the trusted installation operator and unavailable for replacement by provider workers or other untrusted users. These checks do not claim protection against a hostile same-UID operator replacing directory entries or deliberately unlocking inherited descriptors. Never unlink or replace the lease during deployment; changing its inode invalidates existing receipts and breaks cooperating custody.

### Qualified local rehearsal-copy boundary

The source test `test_real_copy_consumer_lease_then_exact_redundant_copy_apply` exercises
an actual foreground copy and a separate foreground consumer through the operation
wrapper. The consumer reads and closes its input, signals readiness, then remains
alive under the shared lease. An apply attempt must refuse before collection or
result publication even though the input descriptor has already closed. After the
consumer exits successfully, dry-run retains the copy and apply removes only the
expired redundant copy. The recovery file, current revision, distinct rollback
revision and original terminal receipt remain byte-for-byte unchanged.

This is a temporary synthetic fixture, not production enrollment. Its post-consumer
process inventory is explicitly controlled; host-wide process inspection has separate
tests and remains a required live check. It proves the existing local wrapper custody
boundary, not remote rehearsal, Kubernetes execution, persistent-service ownership or
historical artifact eligibility. No node paths, timer or live cleanup configuration
are changed by this test. Before using the workflow for a real local rehearsal, the
operator must verify the actual foreground consumer and every descendant preserve
the lease, and retain a separately verified recovery copy outside deletion eligibility.

### Migration rehearsal is not automatically an equivalent copy

A successful consumer may legitimately mutate its input. In particular, the current
release-specific migration rehearsal opens the copied SQLite database and runs schema
initializers. Logical row preservation does not establish byte equality with the
pre-rehearsal recovery file. A terminal copy receipt and successful consumer exit do
not override the collector's independent source/recovery hash comparison.

The SQLite consumer regression runs a real schema change on a copied database while
preserving the original row. Collection then refuses and retains both files and the
original receipt. Do not enroll changed rehearsal output using its pre-rehearsal hash,
refresh that hash to hide the mismatch, or claim the remote rehearsal participates in
the local lease workflow. Actual adoption requires a repository-owned, audited local
call site and a separately verified equivalent recovery artifact, or a separately
reviewed disposal policy for changed derived data. Neither is supplied by this proof.

### Host-local rehearsal provenance prototype

`rehearsal_operation.py` is a separate non-deleting operation entry point. Run it on the same Linux host and in the same process/mount namespace as its existing private lease and the foreground runner. It verifies a standalone immutable snapshot and exact runner hashes, creates a new private operation directory, copies the source, and invokes the real runner using `--data-dir` and `--report` while retaining the exclusive lease. The runner inherits the lease descriptor; all descendants must preserve it and must not escape their process group. A killed wrapper therefore does not release custody while its cooperating child remains alive. The runner is trusted installation code, not a sandboxed provider command. The default foreground mode does not support systemd dispatch, remote SSH dispatch or daemonizing consumers. The separately selected supervised mode below has an explicit host-local handoff; it never forwards a lease over SSH.

A terminal receipt is issued only after foreground success with no surviving process-group descendants, exact bounded schema-proof report, unchanged source and runner hashes, no SQLite sidecars, and a verified output hash. Every attempt has a new operation identity. Existing directories, interrupted attempts and old receipts are never adopted. Failure retains its copy and start record without a terminal receipt. Source, runner, lease and their trusted ancestor paths are outside the operation root; all writers and later consumers must use the same existing lease. A standalone snapshot must already have passed the backup owner's full inspection. The operation does not inspect backup bundles or authorize a restore or deployment.

While the wrapper is alive, the default foreground deadline is 180 seconds, never more than ten minutes; output streams are discarded rather than publishing private failure text. Wrapper death is not a hard aggregate deadline: a surviving child deliberately retains the inherited lease, and may keep it indefinitely. The installation must enforce an external process-group or control-group deadline, terminate surviving consumers and verify custody before recovery; this prototype does not install that supervisor. The one-MiB report bound and input/output size checks bound validation. Before copying, free space must cover the configured maximum database size plus report budget and reserve. This is admission accounting, not a filesystem quota or protection against unrelated writers or a runner that temporarily expands beyond its eventual output. A live adopter must retain the existing process/filesystem resource controls and qualify actual peak usage. Failed or timed-out runs are not cleaned automatically.

Recovery means repeating the exact pinned runner on a new copy of the preserved source. The real SQLite fixture mutates physical bytes, then independently repeats the operation from the original source and verifies the same logical table proof and retained original row. Receipts explicitly state `byte_equivalent_recovery: false` and `kind: replayable-rehearsal-not-enrolled`. They are not the terminal receipts accepted by the equivalent-copy collector, and no deletion eligibility follows from this proof. Current and rollback release files, source recovery files, and all outputs remain untouched. A different derived-output disposal policy would require its own reviewed implementation and authorized adoption, not a changed hash or a broader existing collector.

This establishes a repository-owned local entry point and synthetic recovery/custody proof only. The existing remote deployment wrapper has not been switched to it, no timer or enrollment has been enabled, and no historical operation is eligible because this utility exists. Actual remote adoption still needs the deployment owner to qualify the same-host lease boundary, the immutable source/runner retention policy and resource controls. Until then, automatic artifact retention remains disabled.

### Supervised remote-host callsite

The `--supervised` mode is intended to replace only the isolated-copy/run/final-validation portion of the deployment owner's existing remote rehearsal. It executes **on the remote host**, after that owner has acquired the existing deployment lock, verified actual backup/worker inactivity and headroom, and completed the exact candidate's full backup inspection. It does not acquire deployment authority, select a backup, stop timers, change the running release, or inspect provider state. Those existing callsite safeguards remain mandatory and are not replaced by an input hash.

The repository-owned Linux static runner is `tools/rehearsal`. Build it from the exact qualified source with `CGO_ENABLED=0`, `-trimpath` and `-ldflags '-X main.revision=FULL_SOURCE_REVISION'`; retain the resulting hash alongside the source-matched daemon and recovery input. An unset or malformed revision refuses execution. This runner preserves the existing schema initialization sequence, complete table/count/logical-digest comparison, no-provider construction and 150-second internal context. It adds required exact same-inode lease acquisition before opening the copied database. The legacy runner without these flags cannot satisfy this callsite.

The host coordinator stages a new operation and pinned static runner under the exclusive artifact lease. It then releases that lease before dispatch; the runner opens the **same inode**, bind-mounted as `/artifact.lease` inside its `RootDirectory`, acquires exclusive custody, and checks the explicit host device/inode before opening the copied database. No terminal receipt exists during this handoff, and no output is enrolled. Concurrent cleanup may inspect other enrollments, but cannot treat this pending operation as disposable. After successful service termination, the host reacquires and revalidates the original lease and checks the source, runner, report and output before finalizing provenance. Lease contention, wrong inode, existing operation identity, failed service or uncertain client outcome produces no terminal receipt and is never retried automatically.

The command preserves the actual deployment rehearsal's service isolation and limits: `Type=exec`, isolated `RootDirectory`, working directory `/`, CPU quota 100%, memory 512 MiB, no swap, runtime maximum 180 seconds, stop timeout five seconds, control-group kill, private network and devices, API virtual filesystems and no-new-privileges. Only the existing artifact lease is additionally bind-mounted; canonical recovery input is not mounted into the runner's root. Literal absolute paths exclude systemd specifier and path-list metacharacters. The uniquely named transient unit uses wait/collect, not a timer or persistent service. Its runtime/control-group limit is owned by systemd and survives coordinator/client death. A 210-second client wait does not authorize assuming the service stopped: unresolved outcomes retain the operation and require owner inspection.

Qualification covers real exclusive lease acquisition and expected inode rejection, the unchanged Go rehearsal proofs, and a modeled systemd-dispatch handoff with a real subprocess/SQLite consumer. That model checks every service isolation/resource argument and does not claim an actual mount namespace or control-group test. Synthetic physical qualification on Linux/systemd exercised the exact supervised helper and static runner in real RootDirectory mount and network namespaces: original rehearsal and fresh recovery replay passed with unchanged source; killing the coordinator preserved the child-held same-inode lease, and the fixed 180-second control-group deadline released it after 180.204 seconds. The interrupted operation produced no terminal receipt. This establishes synthetic supervision and custody proof, not production release qualification. The deployment owner must still verify the exact release-matched runner, immutable source and runner retention, same-host lease participation and resource headroom at the live callsite before adoption. No production wrapper, timer, enrollment, release package or historical artifact has been changed by adding this source. The resulting receipts remain replay provenance only; they still do not authorize disposal under the byte-equivalent collector.

The typed runner's v2 logical digest streams each typed row into a SHA-256 digest and orders those fixed-size digests in a private disk-backed SQLite index. Duplicate multiplicities, ordered column names and all table names/counts remain part of comparison. Row and final proof identity rely on SHA-256 collision resistance; digest bytes are versioned and are not interchangeable with the older whole-table JSON digest. Only before/after proofs from the same runner are compared. The scratch index contains row digests/counts, not source payloads, uses a 1 MiB page-cache target and a 256 MiB scratch-file ceiling, and is removed on ordinary return/error. Memory scales with the largest decoded/encoded row plus fixed cache/driver overhead, not aggregate table payload; scratch disk scales with distinct row digests up to that ceiling, after which the proof fails closed. Cancellation, scratch I/O failure or unsupported SQLite values fail the proof rather than skip rows. The existing isolated operation owns any scratch left after forced termination. This does not enlarge the 512 MiB deployment limit or authorize artifact collection; realistic deployment qualification remains required.
