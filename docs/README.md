# Design and Delivery

## Implementation status

The coordination slice implements a loopback service with scoped projects/workers, basic jobs and durable messages; see the [local API](local-api.md). The standalone browser and remaining orchestration features are not implemented. The initial design is under review in [PR #1](https://github.com/benhook1013/FireController/pull/1). Temporary development tooling uses separate private state and does not establish production readiness.

## Documents

- [Development](development.md): implemented profile-bound CLI reads/mutations, build/check commands, and current proof limits.
- [Incoming message events](message-events.md): scoped inbox snapshots, replay and bounded waits.
- [Local MCP tools](mcp.md): bound read/write stdio tools and protocol/host proof limits.
- [Local API](local-api.md): implemented coordination endpoints, bootstrap and limits.
- [Suggested agent workflow](agent-workflow.md): optional working practices for project instructions and a usage skill, separate from tool contracts.
- [Product scope](product.md) defines the requirements.
- [Architecture](architecture.md) owns service boundaries, project isolation, storage, and provider observation rules.
- [Agent and client contract](agent-client.md) owns profiles, tool calls, subscriptions, cancellation and reconnect semantics.
- [AI usability](ai-usability.md) owns output budgets, agent interaction patterns, and attention-cost proof.
- [Onboarding](onboarding.md) owns AI-guided project setup and historical import.
- [Review orchestration](reviews.md) owns reusable review invariants and configurable policy.
- [Acceptance and performance](acceptance.md) defines observable proof requirements.
- [Inherited evidence](inherited-evidence.md) records source provenance and disposition of existing behavior and tests.
- [Source-pass adjudication](source-review.md) records independent extraction coverage and the main worker's evidence-backed decisions.
- [Design decisions](decisions.md) records the selected technical direction and remaining implementation selection work.
- [Delivery plan](delivery.md) records implementation sequence and actual validation status.

Public documentation must not contain live messages, private job briefs, credentials, installed user paths, or provider captures. Historical code references use the pinned public source snapshot.
