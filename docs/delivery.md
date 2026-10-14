# Delivery Plan and Proof Status

## Current state

The public repository and Apache-2.0 project baseline exist. [PR #1](https://github.com/benhook1013/FireController/pull/1) contains the proposed design and validation scaffold. Its first explicitly requested hosted CodeRabbit review completed with four accepted findings; the design and checker corrections are included in the updated PR. Fresh hosted coverage of those corrections remains pending. No standalone service or browser runtime exists. The CLI foundation implements local version/context commands, with its boundary and proof limits recorded in [development](development.md).

## Development bootstrap

Use a private pinned copy of the legacy controller with a context for this repository and its own SQLite database under this repository's Git common directory. Keep its original source notices and source hash. The distinct `firecontroller-dev` entrypoint selects only this repository's temporary development context.

Jobs, inbox, review stack, reservations, captures, and review records belong to the new repository's private state. Initialize and explicitly migrate base review state before bootstrapping its independent jobs, inbox, map, and records schemas. Prove local create/read plus combined snapshot/restore using a disposable destination. Internal legacy storage labels do not establish project identity; the explicit context and separate Git common directory select this project's state.

The temporary runtime is not shipped as the product and does not implement the new multi-project service. Review thresholds and marker conventions remain its inherited behavior until the replacement is proven. Initial hosted review is requested only after publishing a coherent PR and configuring its isolated stack.

## Sequence

1. Complete feature/test inventory, target contracts, decision record, source provenance, and repo validation. Publish one design/scaffold PR well below the discussed hosted file ceiling and request CodeRabbit explicitly.
2. While that hosted review runs, prepare a separate local implementation branch for dependency-safe work. Do not publish changes to the reviewed head until the review completes. Avoid implementing disputed architecture assumptions.
3. Deliver server/CLI boot, explicit profiles, scoped projects/workers, migrations, and packaging proof. Then deliver jobs, durable messaging/events, reconnect behavior, and the native adapter capability experiment.
4. Deliver live browser and project appearance/editorial settings against the same API. Add provider observations, scoped refresh, and attributable review execution in coherent reviewed slices.
5. Measure concurrency, provider isolation, failure recovery, and complete backup/restore. Only after native capability is proven, migrate this project's development controller.
6. Inventory and preview existing installation and website migrations separately. Human-reviewed cutover selects the replacement and verifies preserved state before legacy source or installation retirement.

## Validation ledger

| Boundary | Current evidence | Remaining proof |
| --- | --- | --- |
| Public project | Repository creation and visibility verified; PR #1 published; documentation CI passed on its initial head | Human design review and CI on subsequent heads |
| Main protection | PR requirement, admin enforcement, conversation resolution, no force/deletion; strict required `validate` check attributed to GitHub Actions | Ongoing enforcement as validation expands |
| CodeRabbit configuration | CLI configuration smoke, doctor, and v2 schema passed; GitHub App performed an explicitly requested substantive hosted review | Fresh review of corrected head, subject to provider quota; automatic and incremental reviews remain disabled |
| Inherited behavior | Two bounded inventories plus two independent source/test-body passes and main-worker adjudication; pinned census hashes/counts verified | Legacy suite was not executed; fresh rebuild scenario implementation and execution |
| Temporary project controller | Separate private pinned runtime/context; independent SQLite bootstrap; job create/read and combined snapshot/restore passed; stack registration, hosted request/capture and CLI run recorded | Replacement by the native service after its proof |
| Design validation | Markdown lint, local links, and seven focused checker tests passed; initial full CLI review reported one missing-lockfile finding, rejected against the committed lockfile and successful CI | These checks do not prove standalone runtime behavior |
| Local CLI foundation | Go race tests and vet passed for profile isolation, strict configuration, unchanged state, credential exclusion, and concise errors | Native Windows/macOS execution, service identity verification, authentication, and API commands |
| Standalone service/browser | None | Runtime scenarios in [acceptance](acceptance.md) |

Record proof in the owning slice and PR; do not silently turn planned commands into passing evidence. Main-branch merge remains a human action.
