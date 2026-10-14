# Delivery Plan and Proof Status

## Current state

The public repository and Apache-2.0 project baseline exist. Design documents are being assembled on `codex/design-and-bootstrap`; no application runtime exists. The CodeRabbit CLI configuration smoke completed with zero findings, and the published v2 schema accepted the configuration. This is proof of the configuration invocation only, not the full design or hosted repository integration.

## Development bootstrap

Use a private pinned copy of the legacy controller with a context for this repository and its own SQLite database under this repository's Git common directory. Keep its original source notices and source hash. The distinct `firecontroller-dev` entrypoint must not replace `firemud-controller`, `firemud-pr-review`, FireMUD's selected runtimes, or backup/site selectors.

Jobs, inbox, review stack, reservations, captures, and review records belong to the new repository's private state. Initialize and explicitly migrate base review state before bootstrapping its independent jobs, inbox, map, and records schemas. Prove local create/read plus combined snapshot/restore using a disposable destination. The old internal `firemud` directory label is legacy implementation detail inside a different Git common directory; it is not a reference to FireMUD's actual database.

The temporary runtime is not shipped as the product and does not implement the new multi-project service. Review thresholds and marker conventions remain its inherited behavior until the replacement is proven. Initial hosted review is requested only after publishing a coherent PR and configuring its isolated stack.

## Sequence

1. Complete feature/test inventory, target contracts, decision record, source provenance, and repo validation. Publish one design/scaffold PR well below the discussed hosted file ceiling and request CodeRabbit explicitly.
2. While that hosted review runs, prepare a separate local implementation branch for dependency-safe work. Do not publish changes to the reviewed head until the review completes. Avoid implementing disputed architecture assumptions.
3. Deliver server/CLI boot, explicit profiles, scoped projects/workers, migrations, and packaging proof. Then deliver jobs, durable messaging/events, reconnect behavior, and the native adapter capability experiment.
4. Deliver live browser and project appearance/editorial settings against the same API. Add provider observations, scoped refresh, and attributable review execution in coherent reviewed slices.
5. Measure concurrency, provider isolation, failure recovery, and complete backup/restore. Only after native capability is proven, migrate this project's development controller.
6. Inventory and preview FireMUD/private-site migration separately. Human-reviewed cutover selects the replacement and verifies preserved state before legacy source or installation retirement.

## Validation ledger

| Boundary | Current evidence | Remaining proof |
| --- | --- | --- |
| Public project | Repository creation and visibility verified through GitHub API | Initial design PR review |
| Main protection | PR requirement, admin enforcement, conversation resolution, no force/deletion verified | Required check names after first CI run |
| CodeRabbit configuration | CLI review completed, zero findings; doctor nine passes; v2 schema validation passed | Hosted GitHub App access and substantive design review |
| Inherited behavior | Two bounded read-only inventories against pinned source; test names read | Rebuild scenario implementation and execution |
| Temporary project controller | Separate private pinned runtime/context; canonical SQLite cutover; jobs/inbox/map/records bootstrap; current job create/read and combined snapshot/restore passed | First PR review registration and hosted provider proof |
| Standalone runtime | None | All scenarios in [acceptance](acceptance.md) |

Record proof in the owning slice and PR; do not silently turn planned commands into passing evidence. Main-branch merge remains a human action.
