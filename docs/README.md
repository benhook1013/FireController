# Design and Delivery

## Implementation status

The standalone service is not implemented. The initial design is under review in [PR #1](https://github.com/benhook1013/FireController/pull/1). Temporary development tooling uses separate private state and does not establish production readiness.

## Documents

- [Product scope and conversation agreements](product.md) records the requirements selected with the human.
- [Architecture](architecture.md) owns service boundaries, project isolation, storage, and provider observation rules.
- [Agent and client contract](agent-client.md) owns profiles, tool calls, subscriptions, and end-of-work behavior.
- [AI usability](ai-usability.md) owns output budgets, agent interaction patterns, and attention-cost proof.
- [Onboarding](onboarding.md) owns AI-guided project setup and historical import.
- [Review orchestration](reviews.md) owns reusable review invariants and configurable policy.
- [Acceptance and performance](acceptance.md) defines observable proof requirements.
- [Inherited evidence](inherited-evidence.md) records source provenance and disposition of existing behavior and tests.
- [Source-pass adjudication](source-review.md) records independent extraction coverage and the main worker's evidence-backed decisions.
- [Decision record](decisions.md) records proposed technical decisions and unresolved choices.
- [Delivery plan](delivery.md) records implementation sequence and actual validation status.

Public documentation must not contain live messages, private job briefs, credentials, installed user paths, or provider captures. Historical code references use the pinned public source snapshot.
