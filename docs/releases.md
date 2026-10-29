# Build Publication and Deployment

## Published artifacts

The [CI workflow](../.github/workflows/ci.yml) runs on pull requests and pushes to `develop` and `main`. Its container job builds the Linux/amd64 image once and checks that actual image. After every required check family succeeds, a separate job publishes that same saved image to `ghcr.io/benhook1013/firecontroller:sha-COMMIT`, where `COMMIT` is the full source revision. It does not rebuild after testing. Repository/source labels and the workflow's image-reference artifact bind the source commit to the registry digest. `develop` images are development candidates; publishing from `main` does not create a versioned stable release.

Only trusted branch pushes publish. Pull requests have no registry write permission. The publication job uses its short-lived `GITHUB_TOKEN`, with package write permission limited to that job; no personal registry token or deployment credential is required. The first package is private by GitHub's default. Set its visibility to public in the package settings to permit anonymous pulls; this is independent of the source repository's visibility. The image contains no operator catalog, credentials, provider CLI or live data. Those remain private runtime mounts under the [container contract](container-runtime.md).

The publication summary and `image-reference-COMMIT` artifact contain the full `ghcr.io/...@sha256:...` deployment reference. Use that digest, rather than selecting whichever commit happens to be current later. Revision tags are the lookup convenience; the digest identifies immutable image contents. Image publication does not restart a running installation.

## Native client bundles

Successful Linux qualification and Windows/macOS smoke jobs retain the binaries they tested as `clients-*` workflow artifacts for fourteen days. Each bundle contains `fc`, `fcd`, `fcmcp`, matching public docs/skills, a source/platform/binary-hash manifest and an archive checksum. The packaging helper refuses binaries without the exact clean source revision. Choose artifacts from a fully successful CI run for the intended commit; an artifact from an otherwise failed run is not a qualified release. Workflow artifacts are development downloads, not a permanent public release catalog.

Download an artifact through its Actions run page or `gh run download RUN_ID --repo benhook1013/FireController --name ARTIFACT_NAME`. Verify the adjacent `.sha256` before extracting the archive. Keep each revision's binaries and guidance together. Updating a generated launcher affects future processes; an already-running MCP session needs its normal host reconnect, preserving the same private profile. There is no automatic update of other agents' processes.

## Deployment boundary

Pull the published digest on the target host and use it as the selected image in the existing deployment. For a local Docker installation, use the [container guide](container-runtime.md); an existing Kubernetes installation updates its controller image by digest while preserving its private volumes, runtime bindings and single-writer replacement strategy. The daemon and embedded browser ship together.

The current hosted rollout is operator initiated. It checks the installed revision and data/settings, waits for the configured review activity to become idle, replaces the single writer and verifies recovery. This is a deployment limitation: there is no service-owned admission drain or automatic deployment queue, and continuous work can postpone replacement. An idle check is not an atomic admission barrier. Do not claim that a failed idle check scheduled a future deployment.

A forced restart can interrupt an executing local CLI review. The existing recovery retains possible execution as uncertain and never blindly repeats a provider review; it cannot guarantee a complete result from a process killed before retention. A bounded rollout needs an explicit drain/recovery policy, separate from image publication. Queued work and remotely running Hosted reviews must not be treated as equivalent to an executing local child process. The [execution owner](review-execution.md) and [provider recovery owner](provider-resumption.md) define the durable recovery boundary.
