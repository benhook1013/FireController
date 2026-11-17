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
