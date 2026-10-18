# Design and Delivery

## Implementation status

The service implements project-scoped jobs, durable messages and native CLI/hosted review orchestration, with profile-bound CLI and MCP clients. The embedded browser provides jobs, workers, conversations and review inspection; see the [local API](local-api.md), [browser guide](browser-guide.md) and [execution contract](review-execution.md) for exact boundaries. The [delivery plan](delivery.md) records proof and unfinished work. The retained development controller remains primary until implementation is complete and the new GUI passes human review.

## Documents

- [Delivery and Project website](browser-experience.md): default worker lanes and review queue, merge-head hero and flexible editorial project presentation.

- [Legacy adoption](legacy-adoption.md): optional source-specific tooling, selected retention and guarded native historical coordination imports, with explicit implementation gaps.
- [Per-PR review rounds](review-round-controls.md): retained round limits, taper, channel closure and independent queue scheduling for adoption.
- [Public project website](public-site.md): shareable project showcase, publication selection and the public/private field boundary.
- [Project presentation and metadata](project-presentation.md): selected appearance records, semantic theme validation and isolated live project updates.
- [Project map](project-map.md): revisioned workstreams, return points and explicit job references, with implementation status at the owner.
- [Browser session renewal](browser-session-renewal.md): activity-based cookie renewal, scoped authority and lost-response recovery.
- [Browser guide](browser-guide.md): embedded UI, connection privacy and build/proof limits.
- [Browser coordination views](browser-coordination.md): explicit note/history and worker pause/focus views over the existing read contracts.
- [Development](development.md): implemented profile-bound CLI reads/mutations, build/check commands, and current proof limits.
- [Managed backups](backups.md): verified local bundles, managed scheduling/retention and inactive restore, with activation limits.
- [Provider resumption](provider-resumption.md): explicit operator reconciliation, retained-work abandonment and release after restore; implementation and proof status are recorded at that owner.
- [Container runtime](container-runtime.md): Linux/amd64 image, Git HTTPS dependencies, private volumes and optional operator review capabilities.
- [Hosting and HTTPS](hosting.md): local/remote deployment, infrastructure-owned certificates, persistence and executed hosted proof.
- [Quiet agent message wait](message-wait.md): selected bounded CLI/MCP subscription behavior, output and host proof limits.
- [Incoming message events](message-events.md): scoped inbox snapshots, replay and bounded waits.
- [Local MCP tools](mcp.md): bound read/write stdio tools and protocol/host proof limits.
- [Local API](local-api.md): implemented coordination endpoints, bootstrap and limits.
- [Retained review history](review-history-growth.md): lifetime growth, original producers, immutable versions and bounded transfer/read requirements.
- [Native review workspace](review-workspace.md): stored queue, PR/run history, findings, fix proof and explicit administrator observations.
- [Public review checkpoints](review-publication.md): explicit historical source summaries, durable posting intent and recovery without blind reposting.
- [Observed review readiness](review-readiness.md): explicit thread/merge-state collection and provider-free combined assessment.
- [Required CI observations](review-ci.md): explicit collection, current workflow identity and compact stored assessment.
- [Native review evidence controls](review-controls.md): explicit retained capture reads, guarded finding decisions, fix references and source finalization.
- [Same-project finding routes](review-routes.md): immutable source counts, receiving decisions/fix proof, guarded rerouting and incoming-policy holds.
- [Hosted capacity settlement](hosted-settlement.md): guarded administrator recovery after verified external termination, preserving evidence and preventing automatic reposts.
- [Native review execution](review-execution.md): durable operations, provider adapters, isolated workspaces and the remaining dispatch/cutover boundary.
- [Source refresh and observed policy](review-source.md): explicit background source reads, retained observations and provider-free policy assessment.
- [Job checkpoints](job-checkpoints.md): immutable progress records, guarded appends, explicit latest/archive reads and integration status.
- [Job progress](job-progress.md): separate narrative updates and guarded checklist state, history and proof requirements.
- [Worker pause state](worker-pause.md): explicit coordination pause independent of job status, focus and execution.
- [Worker primary job](worker-focus.md): selected optional resume-focus contract, independent of concurrent jobs and status.
- [Job resume context](job-context.md): one deliberate current brief/checkpoint and pending-coordination read with explicit completeness and continuation.
- [Deferred notes](notes.md): scoped reminder/source records, compact reads and guarded history; the owning contract records validation status.
- [Suggested agent workflow](agent-workflow.md): optional working practices for project instructions and a usage skill, separate from tool contracts.
- [Product scope](product.md) defines the requirements.
- [Architecture](architecture.md) owns service boundaries, project isolation, storage, and provider observation rules.
- [Agent and client contract](agent-client.md) owns profiles, tool calls, subscriptions, cancellation and reconnect semantics.
- [AI usability](ai-usability.md) owns output budgets, agent interaction patterns, and attention-cost proof.
- [Prepared map import](map-import.md): additive map preview/application, stable source mappings and private provenance/history.
- [Selected history collection](history-collection.md): bounded GitHub acquisition, durable source captures and explicit resume; implementation and proof status are recorded at the owner.
- [Prepared review import](review-import.md): scoped administrator preview/apply of complete exports or bounded additive mixed-producer batches, with explicit implementation and proof status.
- [Onboarding](onboarding.md) owns AI-guided project setup and historical import; the bundled [onboarding skill](../skills/firecontroller-onboard/SKILL.md) guides the supported setup.
- [Review orchestration](reviews.md) owns reusable review invariants and configurable policy.
- [Acceptance and performance](acceptance.md) defines observable proof requirements.
- [Inherited evidence](inherited-evidence.md) records source provenance and disposition of existing behavior and tests.
- [Source-pass adjudication](source-review.md) records independent extraction coverage and the main worker's evidence-backed decisions.
- [Design decisions](decisions.md) records the selected technical direction and remaining implementation selection work.
- [Delivery plan](delivery.md) records implementation sequence and actual validation status.

Public documentation must not contain live messages, private job briefs, credentials, installed user paths, or provider captures. Historical code references use the pinned public source snapshot.
