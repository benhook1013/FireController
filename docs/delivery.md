# Delivery Plan and Proof Status

## Current state

The public repository and Apache-2.0 baseline exist. [PR #1](https://github.com/benhook1013/FireController/pull/1) contains the design and repository validation scaffold. Repeated deliberately requested hosted and CLI reviews have produced recorded corrections; current-head coverage and outstanding obligations belong to the PR's review evidence, rather than a fixed round count in this document. Three serial Sol high whole-PR direction passes support the core design; the first two identified narrow contracts now incorporated, and the third found no new material gap. Implementation arrives in stacked PRs, with executed proof and remaining limits recorded in each owning contract. The temporary controller remains the development runtime until replacement proof and explicit cutover.

[PR #2](https://github.com/benhook1013/FireController/pull/2) combines the local CLI/profile foundation with the loopback service, scoped projects/workers, basic jobs and durable messages. Its implemented contract and proof limits are in the [local API](local-api.md) and [development guide](development.md). The new browser and later orchestration features remain unimplemented.

## Development bootstrap

Use a private pinned copy of the legacy controller with a context for this repository and its own SQLite database under this repository's Git common directory. Keep its original source notices and source hash. The distinct `firecontroller-dev` entrypoint selects only this repository's temporary development context.

Jobs, inbox, review stack, reservations, captures, and review records belong to the new repository's private state. Initialize and explicitly migrate base review state before bootstrapping its independent jobs, inbox, map, and records schemas. Prove local create/read plus combined snapshot/restore using a disposable destination. Internal legacy storage labels do not establish project identity; the explicit context and separate Git common directory select this project's state.

The temporary runtime is not shipped as the product and does not implement the new multi-project service. Review thresholds and marker conventions remain its inherited behavior until the replacement is proven. Initial hosted review is requested only after publishing a coherent PR and configuring its isolated stack.

## Sequence

1. Complete feature/test inventory, target contracts, decision record, source provenance, and repo validation. Publish one design/scaffold PR well below the discussed hosted file ceiling and request CodeRabbit explicitly.
2. While that hosted review runs, prepare a separate local implementation branch for dependency-safe work. Do not publish changes to the reviewed head until the review completes. Avoid implementing disputed architecture assumptions.
3. Deliver server/CLI boot, explicit profiles, scoped projects/workers, migrations, and packaging proof. Run a small actual-host adapter capability experiment alongside the foundation before depending on asynchronous completion. Then deliver jobs, durable messaging/events and reconnect behavior.
4. Deliver live browser and project appearance/editorial settings against the same API. Add provider observations, scoped refresh, and attributable review execution in coherent reviewed slices.
5. Measure concurrency, provider isolation, failure recovery, and complete backup/restore. Only after native capability is proven, migrate this project's development controller.
6. Inventory and preview existing installation and website migrations separately. Human-reviewed cutover selects the replacement and verifies preserved state before legacy source or installation retirement.

## Validation ledger

| Boundary | Current evidence | Remaining proof |
| --- | --- | --- |
| Public project | Repository creation and visibility verified; PR #1 published; documentation `validate` passed; per-head coverage is recorded in PR evidence | Design PR merge and validation of every later head |
| Main protection | PR requirement, admin enforcement, conversation resolution, no force/deletion; strict required `validate` check attributed to GitHub Actions | Ongoing enforcement as validation expands |
| CodeRabbit configuration | CLI configuration smoke, doctor, and v2 schema passed; GitHub App and CLI completed deliberately requested reviews; independent channel cycles and observed quotas | Fresh provider coverage after corrections; automatic and incremental reviews remain disabled |
| Inherited behavior | Two bounded inventories plus two independent source/test-body passes and main-worker adjudication; pinned census hashes/counts verified | Legacy suite was not executed; fresh rebuild scenario implementation and execution |
| Temporary project controller | Separate private native runtime/context; own SQLite bootstrap and combined snapshot/restore passed; queue/review records and worker lane verified; copied site has 200 passing tests and desktop/mobile route proof | Replacement by the native service after its proof |
| Design validation | Markdown lint, local links, and focused checker tests passed, including nested badge and reference-definition targets; first CLI missing-lockfile claim rejected against committed source/CI | Every later head needs validation; these checks do not prove standalone runtime behavior |
| Local CLI foundation | PR #2 local profile/version commands; malformed UTF-8/FIFO corrections and focused native Windows service/client integration passed | Native macOS and broader platform/access-control guarantees remain unproven |
| Coordination service | Scoped project/access/worker APIs, guarded jobs/history, durable messages and atomic receipts/events; race/vet/docs, packaged Linux restart, native Windows bootstrap/health/identity/reopening plus domain service/client/MCP integration, and CGO-free Windows/macOS cross-builds passed | Published PR/CI and provider review coverage, filesystem access-control guarantees, native macOS execution, performance calibration and remaining [acceptance](acceptance.md) scenarios |
| Standalone browser | None; the temporary private legacy dashboard remains operational with this project's own worker/review state | New live browser and event transport |

Record proof in the owning slice and PR; do not silently turn planned commands into passing evidence. Main-branch merge remains a human action.
