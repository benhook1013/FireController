# Build Publication and Deployment

## Published artifacts

The [CI workflow](../.github/workflows/ci.yml) runs on pull requests and pushes to `develop` and `main`. Its container job builds the Linux/amd64 image once and checks that actual image. After the required check families succeed, a separate job publishes that same saved image to `ghcr.io/benhook1013/firecontroller:sha-COMMIT`, where `COMMIT` is the full source revision. It does not rebuild after testing. Publication pulls the pushed tag back and selects its registry-qualified digest; a local Docker alias cannot become a deployment reference. Repository/source labels and the workflow's image-reference artifact bind the source commit to the registry digest. `develop` images are temporary development candidates; publishing from `main` does not create a versioned stable release.

Superseded runs are cancelled per branch or pull request, so rapid development pushes do not accumulate obsolete full builds. Only pushes to the selected branches publish; pull requests have no registry write permission. The current unprotected `develop` publisher is a temporary pre-v1 arrangement for this installation. Before official releases, publication must be restricted to protected `main` and its workflow/artifact trust boundary reviewed; an unprotected development branch is not a trusted release source. The publication job uses its short-lived `GITHUB_TOKEN`, with package write permission limited to that job; no personal registry token or deployment credential is required. The first package is private by GitHub's default. Set its visibility to public in the package settings to permit anonymous pulls; this is independent of the source repository's visibility. The image contains no operator catalog, credentials, provider CLI or live data. Those remain private runtime mounts under the [container contract](container-runtime.md).

Development website-only pushes can reuse backend qualification from the latest successful `develop` push run. The selector compares that run's exact source with the candidate, not just the latest commit or push: only `webapp/`, embedded browser assets and the browser-experience documentation may differ. Source/browser checks, packaged qualification and the actual container check still run. Backend race tests and Windows/macOS native smoke are omitted only in that case; their code matches the already qualified source. Mixed/backend/workflow changes, missing or unavailable baselines, pull requests and `main` retain the full suite. The required `validate` gate accepts those two omitted families only for an explicitly selected website-only development push. No additional publisher, deployment credential or automatic rollout is introduced.

The publication summary and `image-reference-COMMIT` artifact contain the full `ghcr.io/...@sha256:...` deployment reference. Use that digest, rather than selecting whichever commit happens to be current later. Revision tags are the lookup convenience; the digest identifies immutable image contents. Image publication does not restart a running installation.

## Native client bundles

Successful Linux qualification and Windows/macOS smoke jobs retain the binaries they tested as `clients-*` workflow artifacts for fourteen days. Each bundle contains `fc`, `fcd`, `fcmcp`, matching public docs/skills, a source/platform/binary-hash manifest and an archive checksum. The packaging helper refuses binaries without the exact clean source revision. Choose artifacts from a fully successful CI run for the intended commit; an artifact from an otherwise failed run is not a qualified release. Workflow artifacts are development downloads, not a permanent public release catalog.

Download an artifact through its Actions run page or `gh run download RUN_ID --repo benhook1013/FireController --name ARTIFACT_NAME`. Verify the adjacent `.sha256` before extracting the archive. Keep each revision's binaries and guidance together. Updating a generated launcher affects future processes; an already-running MCP session needs its normal host reconnect, preserving the same private profile. There is no automatic update of other agents' processes.

## Deployment boundary

Pull the published digest on the target host and use it as the selected image in the existing deployment. For a local Docker installation, use the [container guide](container-runtime.md); an existing Kubernetes installation updates its controller image by digest while preserving its private volumes, runtime bindings and single-writer replacement strategy. The daemon and embedded browser ship together.

## Bounded Kubernetes rollout

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
