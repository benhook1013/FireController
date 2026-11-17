# Versioned backup-health publisher source

This is a byte-for-byte source import of the selected live publisher, observed on
2026-11-17. Its SHA256 is recorded in `observed-layout.json`. It does not deploy,
change services, replace the old helper, or establish installation readiness.
The JSON is an observation fixture, not a supported runtime configuration API.

The existing launch contract remains `python3 publish_backup_health.py --fcd
<qualified absolute helper> --sha256 <exact helper hash>`. The separate historical
helper is intentional and is not substituted with the batch executable. The
publisher only invokes `backup-offsite-status`; it never invokes a transfer.
Its provider configuration root, report destinations, three-hour freshness and
report owner remain unchanged. No credentials are included in this directory.

Tests verify exact imported bytes, old helper/path contract, precise status argv,
suppressed child output, successful report publication, and preserving the old
report when the helper fails, a mirror contains unexpected fields, or the prior
report belongs to another instance. Tests mock the child executable and final
chown; fixture ownership is mapped to the test user. They do not qualify actual
old-helper runtime compatibility, real ownership transitions or service startup.

Before activation, the full installation job must package or qualify Python and
its runtime, the exact helper and canonical provider configurations, private
staging/report directories and ownership, service unit/timer/environment, and
current/rollback/recovery readers. Changing the hardcoded paths into a configurable
interface needs its own compatibility tests and migration design. Existing path
checks and helper-hash verification are preserved, not claimed as hostile-writer
custody. The broader install job remains open.

Run: `PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tools/health -p 'test_*.py' -v`.
## Explicit offline helper qualification

`test_offline_qualification.py` is opt-in and requires root in an empty network
namespace. It verifies the exact existing helper hash before making a private
fixture copy. It runs the unchanged publisher with the real old helper against
three populated synthetic ledgers, without transport binary/configuration,
provider credentials or live paths. Real ownership/mode transitions to 65532:65532
and 0600, report fields, original ledger/config bytes and refusal of a mismatched
helper pin are checked. Only the three directory constants are redirected into
the temporary fixture; no helper/process/chown mocks are used in this test.

Qualification command (the helper copy must already have the recorded hash):

```sh
unshare -n env -i PATH=/usr/bin:/bin PYTHONDONTWRITEBYTECODE=1 \
  FC_HEALTH_OFFLINE=1 FC_HEALTH_HELPER=/tmp/health-publisher-6989/fcd \
  /usr/bin/python3 tools/health/test_offline_qualification.py -v
```

The 2026-11-17 isolated WSL run passed in 0.117 seconds. This qualifies this
publisher/helper contract with synthetic inputs and the installed Python runtime,
not target-host Python/library packaging, service startup, all helper generations,
immutable path custody, arbitrary configuration migration or production rollout.
### Candidate isolated interpreter launch (not activated)

The target host's Python 3.12.3 observation loaded startup customization and
system dist-packages in addition to standard-library imports. Those are hidden
runtime dependencies, not required by publisher source. An additional isolated
fixture run with `/usr/bin/python3 -I -S -B` passed the same real-helper/ownership
qualification in 0.119 seconds. `-I -S` excludes environment/user search paths and
site initialization; `-B` prevents bytecode-cache writes. This is a candidate
future launch contract only, not the observed active service command. It still
requires the selected interpreter, standard library, extension modules and shared
libraries. The WSL test is not qualification of the target host's Python version
or a service-unit migration; those remain explicit pre-switch requirements.
## Operator-health bundle (not installed)

`package_bundle.py --helper /absolute/qualified/fcd --output /absolute/new/bundle.tar.gz`
builds an explicit Linux-amd64 operator bundle containing the unchanged publisher,
the exact qualified historical reader, and a dependency manifest. It never fetches
or executes inputs, reads provider credentials, changes a service, or marks an
installation ready. Inputs must match the supported publisher/reader hashes;
substituting a current daemon binary is rejected rather than silently upgrading
the reader. Future reader generations require separate qualification and an
explicit supported-pin change.

The three archive entries are regular relative files; source bytes are bounded,
verified before packaging, and the result is deterministic. Publication uses a
private temporary file and atomic no-replace link; an existing destination is
never overwritten. A failure after linking may leave the exact output artifact
present but is not reported as success. The caller owns a private cooperating
build directory; this is not hostile-writer custody or a production installer.

The manifest lists the required system Python runtime, root operator, canonical
private provider configs/ledgers, staging/report ownership and qualified service
and rollout configuration. Those are unresolved prerequisites, not bundled secrets
or implied guarantees. The existing main release package remains unchanged.

### Built-bundle offline qualification

`test_bundle_offline.py` runs the same real-reader, real-ownership synthetic
qualification against bytes taken from the actual deterministic bundle. It checks
the exact three regular members, manifest, lengths and pinned hashes before
loading the extracted publisher and invoking the extracted historical reader.
The existing fixture redirects only private data directories, not subprocess or
ownership behavior. Run it with the same explicit root/network-isolated setup
above and `/usr/bin/python3 -I -S -B`.

This closes the source-versus-packaged-bytes qualification gap. It is still an
isolated Linux fixture, not a target-host installer, service startup test, full
Python/shared-library dependency closure or current/rollback deployment proof.


### Explicit offline operator release assembly

`tools/release/package-operator.py --binary-dir <clean native binaries>
--health-reader <exact qualified historical reader> --revision <full commit>
--output <absolute new archive>` assembles the native client release and health
companion into one checksummed operator artifact. The existing native packager
still verifies each binary's clean Go VCS metadata against the selected revision.
The reader is an explicit offline input: absent or incompatible readers fail
before native packaging; no download or current-daemon substitution occurs.

Assembly happens in a private temporary build directory and publishes the final
archive with a no-replace link. The relationship manifest binds both artifacts
and preserves unresolved runtime/service prerequisites and readiness=false.
A post-link fsync error can leave the complete output present; reconcile it rather
than assume no output. No runtime installer, CI activation, service change or
rollback qualification is included. Adapter unit tests stub native packaging;
they do not establish a new clean-binary end-to-end release qualification.


### Unchanged fixed-path CLI qualification

`test_fixed_path_qualification.py` additionally runs the extracted publisher as a
real CLI with its unchanged fixed paths. Run only with root `unshare -m -n`, an
empty environment, and both `FC_HEALTH_OFFLINE=1` and
`FC_HEALTH_FIXED_PATHS=1`. It refuses the initial mount namespace, makes mounts
private, and mounts a small disposable tmpfs over `/var/lib` inside that namespace.
Synthetic private provider configs/ledgers and report directories then occupy
the exact production-shaped paths without accessing host data. No publisher
constants, subprocesses or ownership operations are mocked. The separate pinned
reader is extracted from the built bundle; correct publication and wrong-pin
refusal preserve source inputs and prior reports.

This passed in 3.000 seconds under isolated Python. It qualifies fixed-path CLI
behavior, not a target-host systemd unit, timer, runtime-library package, rollback
or production namespace. The namespace and tmpfs disappear when the test exits.


### Candidate service override (not installed)

`service_override.render(<absolute qualified bundle directory>)` emits only an
ExecStart reset and replacement. It preserves the separate exact historical
reader and selects the isolated Python flags qualified above. It rejects path
whitespace, traversal, systemd specifiers and variable syntax rather than relying
on shell quoting. It does not read files, grant custody or qualify the supplied
directory. No unit/timer/security settings are installed or changed.

The observed service is oneshot, root:root, umask0077, timeout45s, with PrivateTmp,
ProtectHome, ProtectSystem=strict and NoNewPrivileges. A static systemd-analyze
fixture accepts the override with those settings. Existing ReadWritePaths,
ReadOnlyPaths, InaccessiblePaths and every other unit/drop-in property remain
the installer's obligation to preserve and qualify; the fixture is not a full
unit clone. In particular the observed path lists explicitly name OneDrive/S3,
not Google. No inference of equivalent credential isolation is made for Google.

Actual target service startup/namespace and timeout behavior, input provenance,
atomic switch and rollback remain unqualified. Retaining the old unit/reader
bytes is not itself proof a rollback is safe.


### Disposable local systemd execution proof

`test_service_qualification.py` is explicitly opt-in (`FC_HEALTH_SERVICE=1`
and `FC_HEALTH_OFFLINE=1`) and root-only. It builds and extracts the pinned
companion, then runs its unchanged publisher CLI through disposable, collected
local systemd oneshot units. A synthetic directory is bind-mounted as `/var/lib`
only inside each unit's mount namespace; no host mount or existing unit is changed.
The test uses root ownership, umask0077, 45-second timeout, PrivateTmp, ProtectHome,
ProtectSystem=strict and NoNewPrivileges, plus PrivateNetwork for this fixture.
Only synthetic report/staging paths are writable. Success publishes three exact
reports; a wrong reader pin fails while preserving reports and source inputs.
Both units must be unloaded after execution. This passed in 3.092 seconds.

This qualifies a local synthetic systemd execution context, not an exact clone of
all node path restrictions, target-host runtime provenance, live timer behavior,
atomic deployment switching or rollback. The target's full inaccessible/read-only
path graph and its Google discrepancy remain unresolved.


### Target runtime and publisher launch compatibility checkpoint

A read-only target-node probe on 2026-11-17 verified the exact observed publisher
source hash, compiled it in memory, imported every explicitly declared dependency,
and ran only its `--help` parser with `-I -S -B` (no report publication). The target
was x86_64 Python 3.12.3, cache tag cpython-312, resolved `/usr/bin/python3.12`,
SHA256 `e50d468e8b0adfb05733f5b87b3cff34829c4a8c1aea50c865aa8bdfe4bb150f`.
All imports succeeded from the standard library or built-ins; help exited zero.
This is a timestamped runtime compatibility observation, not immutable custody or
a full transitive-library/provenance guarantee for a future installation.

The disposable local service test additionally runs the legacy plain-Python launch
against reports just written by the packaged isolated launch, then runs the
packaged launch again. All three providers retain identical fields except the
expected observation timestamp, and synthetic source inputs stay byte-identical.
The subsequent wrong-pin refusal preserves the latest reports. Four transient
units, including the failed case, are collected and checked as unloaded. This
round-trip passed in 3.184 seconds. It establishes publisher/report compatibility
for the same pinned publisher and historical reader, not application database
rollback, an atomic unit switch, or exact target-node service restrictions.


### Observed path/capability restriction regression

A fresh read-only service inspection confirmed target systemd 255, AF_UNIX-only
address families, CAP_CHOWN/CAP_DAC_OVERRIDE only, kernel/control-group protections,
PrivateNetwork, the two OneDrive/S3 read-only roots and four inaccessible paths.
The disposable test now applies those observed restrictions to its synthetic
filesystem and supplies only synthetic placeholders at the inaccessible paths.
The packaged→legacy→packaged publication round-trip and wrong-pin refusal still
passed (3.378 seconds); all four transient units were collected. No original
service settings were changed. Google remains absent from the observed explicit
credential mask; the fixture intentionally does not silently amend that policy.

This strengthens compatibility under the observed restrictions on local WSL
systemd, but does not qualify target systemd's exact namespace implementation,
all unspecified defaults, unit-file atomic switching, rollout cancellation, or
automatic rollback. The install plan must preserve and recheck the complete
effective unit/dependency graph before any activation.


### Atomic switch/rollback protocol experiment (test only)

`test_switch_protocol.py` exercises file-level drop-in replacement in private
temporary directories, never `/etc` or a service manager. Under the explicit
single cooperating writer assumption it persists exact old/new bytes and their
hashes, then stages and atomically replaces only the selected drop-in. It does
not touch unrelated settings. A new process after an actual forced exit recovers
identity solely from the on-disk operation record. Rollback requires the exact
known candidate; unknown external edits or corrupt intent are refused unchanged.
A pre-replace crash leaves the old file intact and fences its ambiguous staged
file instead of silently removing it. Four tests passed in 0.233 seconds.

This is deliberately not a production installer: the fixture uses synthetic
launch text, no writer lock implementation, no hostile pathname-race defense,
no complete prepared-directory repair and no power-loss durability qualification.
Its operation record is retained only for the disposable test lifetime.

A safe production service switch additionally needs these separate gates:
1. Bind the exact installed unit/drop-ins, bundle, helper, target runtime and
   effective restrictions to an approved immutable operation.
2. Hold deployment/source custody and recheck the exact preimage immediately
   before replacement; preserve unrelated unit properties.
3. Treat file replacement and systemd reload/start as separate transitions.
   A lost response must not be treated as proof either old or new code is active.
4. Reconcile loaded unit properties, process executable/arguments and report
   ownership/binding before selecting forward completion or exact rollback.
5. Roll back only after revalidating the old reader/runtime/input compatibility;
   preserve unknown edits and avoid disrupting an already-running publisher.

No automatic rollback or activation is authorized by these experiments, and
application/database rollback remains outside this health-publisher proof.


### Running-generation reconciliation experiment (test only)

`test_service_reconciliation.py` requires root and explicit
`FC_HEALTH_RECONCILE=1`. It starts only randomly named disposable local synthetic
units, with no publisher or production data. A private flock excludes a real
competing process. Under that custody it reads systemd's InvocationID/MainPID and
active/running state, compares actual `/proc` arguments, and rechecks the same
observation. Reusing the same unit name for a different generation must reject the
old receipt. Desired arguments do not override the actual running generation;
completed/unloaded units cannot satisfy a running receipt. The test passed in
0.230 seconds and verifies all test units unload.

This is a reconciliation design proof, not daemon-reload or a production switch.
Invocation/PID/argv observations are not executable-byte custody, a defense
against hostile PID/path reuse, or a guarantee after releasing the lock. The
fixture's lock is private to cooperating test writers and has not been wired to
the existing deployment producer's real custody. A production implementation
must bind that custody and exact executable/dependency identity, and distinguish
a oneshot service that legitimately completed from one still executing.


### Joined file/running-generation qualification

`test_joined_switch_qualification.py` combines the two test-only protocols under
one private cooperating lock. With the old synthetic generation actually running,
switching the file to new must still reject a claim that new is running. After
normal completion, a new invocation is observed. Rolling the file back while new
is running likewise cannot claim runtime rollback; only a subsequently observed
fresh old-generation invocation satisfies that comparison. All random local
units are unloaded. The two-test run passed in 0.533 seconds.

The selected file contains synthetic fixture text mapped to exact test worker
arguments, not a production unit parser or installer. There is no combined
process-crash/reload proof, executable-byte custody, live unit update or production
activation. Idempotent file-state acceptance alone also does not clean unresolved
staged files. These experiments remain evidence for an eventual installer, not
a replacement for its complete deployment and recovery contract.


### Existing producer custody integration

`tools/release/test_operator_producer_custody.py` runs the assembler as a real
child of the existing `artifact_operation.execute(..., producer_only=True)`.
The native-packaging fixture checks that its subprocess inherits the usable
lease FD and that an independent open cannot acquire the same exclusive lease.
The same check runs after final archive publication. The existing wrapper returns
`producer_finished_unenrolled`, the operation contains only the resulting archive,
and the lease is reacquirable after completion. The test passed in 3.014 seconds.

Native binary packaging is explicitly stubbed in this custody test; the earlier
clean-binary assembly/smoke proof covers a separate boundary. No automatic
collector enrollment is invented for an installed or externally consumed release.
This does not protect a future deployment after producer custody is released.
