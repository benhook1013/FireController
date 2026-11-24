# Build Publication and Deployment

## Fast pre-v1 development

Pre-v1 development deploys the pushed `develop` commit with one fixed command, run from a checkout with SSH access to the serving node:

```sh
python3 tools/deploy/deploy.py            # deploy origin/develop
python3 tools/deploy/deploy.py --dry-run  # build and check only
```

For an authorized incident recovery, `python3 tools/deploy/deploy.py --recover-crashloop` replaces the sole existing Controller pod only when the API is unavailable and Kubernetes confirms the container is in `CrashLoopBackOff`. It uses the same deployment, backup and helper custody locks, database snapshot and migration rehearsal, isolated candidate start and post-rollout checks. A responding or running Controller needs the normal drained rollout.

[`deploy.py`](../tools/deploy/deploy.py) requires a checkout with clean tracked files at `origin/develop`. It builds the Linux/amd64 image from the repository [Dockerfile](../Dockerfile), assembles the versioned installation payload, and records the embedded browser asset hashes. The receiving host verifies and materializes the package in a private staging directory. The script verifies the imported image digest before invoking the packaged [`deploy_host.py`](../tools/deploy/deploy_host.py). The dry run verifies and materializes the same package locally. The host side runs the same steps every time:

1. Take the deploy lock, then the stable helper-generation lease exclusively, then the existing shared backup phase lock. Rollback follows the same order. Each helper acquires the generation lease in shared mode before it reads the selector and retains it through `execv`. Deployment and rollback wait for those processes to exit. The existing twenty-minute wait covers both leases. A timeout during first conversion can leave the inert lease inode ready for retry.
2. Snapshot the live database with SQLite's online backup into `/var/lib/firecontroller/predeploy/`. This is the rollback point; the newest two are kept.
3. Run `fcd migrate` from the new binary on a private copy of that snapshot and require a clean `quick_check`. This rehearses the exact migrations the daemon will run at startup, on real data, before anything live changes.
4. Verify that the backup-health and offsite-batch launch commands both select the same immutable old `fcd`. Verify the health hash, stage the candidate by source and digest, and provision the stable lease with read access for the existing phase-lock group. A missing lease on an installation with a selector requires reconciliation. During first conversion, initialize the selector to the verified old helper, install the two static wrappers, and reload systemd. Both roles must be inactive before the first conversion can complete. The installer leaves running units to finish normally. Each wrapper preserves its existing arguments and substitutes the selected `fcd` path and health hash.
5. Record the exact pending deployment in the selector before draining and replacing the controller image with the [rollout helper](../tools/deploy/kubernetes.py). The rollout process inherits the locks until it exits. Verify the deployment, running image digest, `/readyz` and served browser assets, then atomically activate the matching helper generation and clear the hold. The hold keeps both helpers and pruning paused if the deployment process stops before verification.

If a rollout fails or remains unverified, retain the reported prior image and snapshot. Inspect the same deployment and retry the staged `deploy_host.py` command with the exact pending image, source, helper and asset binding. A ready candidate resumes by completing helper selection under both locks. If the prior image is ready, independently verify the replacement request ended before retrying with `--confirm-prior-outcome`. A ready prior image on its own leaves the hold in place because an outstanding replacement can still arrive. `select-previous` requires a settled deployment. It takes the exclusive generation lease and then the phase lock, verifies the retained previous helper bytes, and atomically selects them for fresh health and batch processes. Coordinate that action with the matching application image. When migration changed the schema, restore the pre-deployment snapshot before starting the older image. Keep a reader from the creating generation for backups that require its schema or bundle format. Development deploys may proceed while CI and container publication are pending. Investigate and repair CI failures.

The installed wrappers, deployment and rollback all use the same local leases. Before first conversion, finish any manual helper or rollback invocation that still uses the old launch path. Use the installed commands for later generation changes so all participants share the same custody checks. Deployment retains the private staging directory for retry and recovery. Keep it while a rollout or rollback might still reference it.

The focused source fixtures cover subprocess launch and rollback ordering, first-conversion refusal, and persistent recovery after an interrupted rollout. Installed qualification requires the effective systemd units, the packaged `fcd` running with its real arguments, and an isolated coordinated deployment and rollback.

## Versioned installation payload

Use [the generation packager](../tools/install/generation_package.py) to stage one Linux/amd64 payload containing the image's exact `fcd`, the qualified rclone binary, and the deployment helper archive from the same Git revision. Supply a locally loaded image digest and independently verified executable hashes. The packager verifies the image's source label and extracts `fcd` from an immutable, never-started local container. The package records the OCI image reference so the image can be retrieved separately by digest. Service configuration, credentials, database files and backups stay outside the payload.

```sh
python3 tools/install/generation_package.py assemble --repository . \
  --source "$RELEASE_SOURCE" --image "$RELEASE_IMAGE" \
  --fcd-sha256 "$RELEASE_FCD_SHA256" \
  --rclone "$RELEASE_RCLONE" --rclone-sha256 "$RELEASE_RCLONE_SHA256" \
  --output "$RELEASE_PACKAGE"
python3 tools/install/generation_package.py verify --package "$RELEASE_PACKAGE" \
  --package-sha256 "$RELEASE_PACKAGE_SHA256" --source "$RELEASE_SOURCE" --image "$RELEASE_IMAGE"
```

Keep the package digest with the release receipt and supply that digest on the receiving host. Verification checks the exact inventory, hashes, executable architecture and nested helper archive. Use `materialize` with the same arguments and `--output` pointing to a new directory under an owner-private parent to make the verified executables and deployment helpers available for preflight. The staged rclone copy is available for recovery. The installed backup jobs continue to use their configured transport and credentials. Installation and activation require the deployment and backup checks above.

## Published artifacts

The [CI workflow](../.github/workflows/ci.yml) runs on pull requests and pushes to `develop` and `main`. Its container job builds the Linux/amd64 image once and checks that actual image. After the required check families succeed, a separate job publishes that same saved image to `ghcr.io/benhook1013/firecontroller:sha-COMMIT`, where `COMMIT` is the full source revision. It does not rebuild after testing. Publication pulls the pushed tag back and selects its registry-qualified digest; a local Docker alias cannot become a deployment reference. Repository/source labels and the workflow's image-reference artifact bind the source commit to the registry digest. `develop` images are temporary development candidates; publishing from `main` does not create a versioned stable release.

A newer branch push supersedes earlier push CI for that branch. See the [CI scheduling contract](../tools/ci/scheduling.md) for shared job lanes and queue limits. Only pushes to the selected branches publish; pull requests have no registry write permission. The current unprotected `develop` publisher is a temporary pre-v1 arrangement for this installation. Before official releases, publication must be restricted to protected `main` and its workflow/artifact trust boundary reviewed; an unprotected development branch is not a trusted release source. The publication job uses its short-lived `GITHUB_TOKEN`, with package write permission limited to that job; no personal registry token or deployment credential is required. The first package is private by GitHub's default. Set its visibility to public in the package settings to permit anonymous pulls; this is independent of the source repository's visibility. The image contains no operator catalog, credentials, provider CLI or live data. Those remain private runtime mounts under the [container contract](container-runtime.md).

Development website-only pushes can reuse backend qualification from the latest successful `develop` push run. The selector compares that run's exact source with the candidate, not just the latest commit or push: only `webapp/`, embedded browser assets and the browser-experience documentation may differ. Source/browser checks, packaged qualification and the actual container check still run. Backend race tests and Windows/macOS native smoke are omitted only in that case; their code matches the already qualified source. Mixed/backend/workflow changes, missing or unavailable baselines, pull requests and `main` retain the full suite. The required `validate` gate accepts those two omitted families only for an explicitly selected website-only development push. No additional publisher, deployment credential or automatic rollout is introduced.

The publication summary and `image-reference-COMMIT` artifact contain the full `ghcr.io/...@sha256:...` deployment reference. Use that digest, rather than selecting whichever commit happens to be current later. Revision tags are the lookup convenience; the digest identifies immutable image contents. Image publication does not restart a running installation.

## Native client bundles

Successful Linux qualification and Windows/macOS smoke jobs retain the binaries they tested as `clients-*` workflow artifacts for fourteen days. Each bundle contains `fc`, `fcd`, `fcmcp`, matching public docs/skills, a source/platform/binary-hash manifest and an archive checksum. The packaging helper refuses binaries without the exact clean source revision. Choose artifacts from a fully successful CI run for the intended commit; an artifact from an otherwise failed run is not a qualified release. Workflow artifacts are development downloads, not a permanent public release catalog.

Download an artifact through its Actions run page or `gh run download RUN_ID --repo benhook1013/FireController --name ARTIFACT_NAME`. Verify the adjacent `.sha256` before extracting the archive. Keep each revision's binaries and guidance together. Refresh any already installed FireController skill directories from that same bundle, including their referenced files; replacing binaries alone leaves agents with stale procedures. Preserve unrelated host metadata and locally added resources. Updating a generated launcher affects future processes; an already-running MCP session needs its normal host reconnect, preserving the same private profile. There is no automatic update of other agents' processes.

Bundle generation routes skill links to private `references/docs/` copies of that exact release's canonical documentation, including links within supporting references. Install or update all bundled FireController skill directories by merging their complete contents, creating the package-owned generated documentation while preserving host metadata and custom files; do not copy only `SKILL.md` or create a shared global docs directory. Cross-skill links use those sibling installed skills, and source-code links are pinned to the release revision. Generated documentation is not maintained separately from `docs/`.

A managed installation can keep bound launchers on a stable absolute executable path and atomically move that path to a verified package. This lets future processes select the installed build together and lets existing capable MCP processes report a [launch-target mismatch](agent-client.md#profiles-and-identity) without changing their authenticated binding. Retain executable files needed by running processes. A client-only output or guidance change does not require redeploying an otherwise unchanged daemon.

## Deployment boundary

For the published-artifact path, pull the published digest on the target host and select it in the existing deployment. For a local Docker installation, use the [container guide](container-runtime.md); an existing Kubernetes installation updates its controller image by digest while preserving its private volumes, runtime bindings and single-writer replacement strategy. The daemon and embedded browser ship together.

## Bounded Kubernetes rollout

The optional [prepared image admission helpers](../tools/deploy/prepared-admission.md) separate fresh operation image preparation from guarded rollout admission without changing backup deadlines. They require their own complete reviewed package and are not used by the default helper or installed automatically.

The operator [deployment helper](../tools/deploy/kubernetes.py) takes an already published image digest, the existing service origin/instance and a protected global-administrator credential file. It requires a single-replica `Recreate` deployment, checks the instance and drain capability, then pulls and starts the candidate on the current node in a disposable pod with no live data or credential mounts. A pull/startup failure therefore precedes any drain. The pod is removed afterward. Image contents remain the published artifact; there is no binary copying or rebuild on the server.

The service's global-administrator-only `POST /api/v1/operator/deployment-drain` with `{}` stops **new native dispatch**, atomically with entry to the supervised work section. `GET` returns process-local `draining` and `active_dispatches`; `DELETE` explicitly abandons the drain. Repeated begin/cancel calls are idempotent. The state is reset by a new daemon process, not stored as project policy. Queued requests, jobs, messages, grants, taper and remote Hosted observations continue to exist; ordinary API and website use remain available. Workers do not need a new flag or review-control change. Operation/queued-receipt responses carry the current `X-FC-Deployment-Draining` response header; matching CLI/MCP clients explain the queued wait without changing stored state or requiring an operator-status read. Older clients keep their existing JSON shape.

The helper waits up to **600 seconds by default**, proceeding as soon as local preparation/dispatch/retention finishes. `--drain-seconds` allows an explicit zero-through-3600-second override. Queued requests and externally running Hosted reviews do not extend the wait. At the deadline it replaces the controller using normal graceful termination, with a ninety-second Kubernetes termination grace. Native worker shutdown allows forty seconds for the existing bounded capture retention/publication path. A changed deployment resource version refuses the replacement instead of overwriting another operator's edits. An abandoned pre-replacement attempt cancels the drain; once replacement may have been submitted, an unknown outcome requires inspecting that same deployment rather than reopening old admission blindly.

CLI output is buffered while the child runs. On normal cancellation, the bounded capture is saved using a cancellation-independent retention budget; the recovery worker verifies retained files and settles the original attempt without rerunning a provider review. Partial captures cannot become complete or clean reviews. A hard kill before saving can lose buffered output; restart retains possible execution as uncertain. The rollout reports whether it reached its drain deadline. The [execution owner](review-execution.md) and [provider recovery owner](provider-resumption.md) define result recovery. An independent executor that survives container replacement is outside this rollout mechanism.

Run the helper from an operator host with Kubernetes access and the private administrator file, for example:

```sh
python3 tools/deploy/kubernetes.py \
  --image ghcr.io/benhook1013/firecontroller@sha256:REPLACE_WITH_PUBLISHED_DIGEST \
  --source-revision FULL_SOURCE_COMMIT \
  --url https://controller.example.com \
  --instance-id INSTANCE_ID \
  --credential-file /private/controller-admin.token
```

Installation-specific namespaces, deployment/container names, Kubernetes context and command prefix are optional arguments. The helper does not install credentials or grant access. It refuses servers without the drain endpoint; bootstrap to a drain-capable revision is a one-time operator transition, not a permanent compatibility path. Successful rollout checks the selected image, original instance and reopened admission, allowing a thirty-second read-only verification window for transient gateway/readiness errors after Kubernetes becomes ready. This rechecks health and never repeats replacement or mutating requests; retained-data upgrades still require the independent [backup and restore checks](backups.md).
