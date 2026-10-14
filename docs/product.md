# Product Scope

## Implementation status

This is the target for the standalone rebuild, not a claim about existing functionality. These requirements were selected in the October 14, 2026 discussion; technical decisions remain subject to the design review recorded in [decisions](decisions.md).

## Mission

Provide dependable self-hosted coordination for humans and AI workers across multiple projects. Jobs, messages, review orchestration, and the browser interface are critical development infrastructure. Reduce hidden work and operational friction through explicit contracts and observable failure behavior.

AI workers are the primary clients. Human browser convenience is secondary to accurate, compact agent context, predictable commands, minimal incidental output, and fast everyday operations. The product must not require agents to repeatedly reconstruct context, manage needless flags, or poll long work solely because the service internally uses operation records.

## Agreed requirements

- One installation supports multiple projects. Project-scoped workers with the same name are distinct; `Project Alpha / Overseer` and `Project Beta / Overseer` never share state implicitly.
- Initially each project binds one GitHub repository. Within an instance, duplicate repository binding is rejected using stable provider identity, including renamed repositories and concurrent creation. Onboarding can connect authorized callers to the existing project.
- The CLI is a thin client to the service. The browser and native agent tools use the same application contracts. Ordinary job and message operations do not call GitHub or inspect unrelated review state.
- The service owns provider observations and refresh decisions. Stored views return promptly with freshness, errors, and scope visible. Consequential actions refresh only the facts their admission contract requires.
- Agents select project and worker through private profiles. Native tools can omit those arguments only when the host provides a safely bound connection. Shared connections require an explicit profile selector. A global mutable current worker or project is forbidden.
- Orchestrators can keep a quiet asynchronous wait pending while working. A relevant message completes the wait; the client then rearms. Host support and delivery timing must be measured rather than assumed. No polling, empty heartbeat output, or pretend wakeup guarantees are required from the model.
- The live browser replaces rebuild-dependent private pages. It reads current service state, subscribes to changes, and supports jobs, inbox conversations, project maps, and review details. Public status publishing is an optional filtered feature.
- Dependable restart, retry, backup, restore, cancellation, isolation, and bounded resource use take priority over framework preference. Throughput claims require measured proof.
- Keep temporary development tooling isolated from the new service. Migration of any existing installation requires a separately reviewed import and cutover after the replacement is proven.
- Publish the standalone repository and use CodeRabbit from the initial design PR. Preserve source provenance and inheritance decisions rather than blindly copying tests or quirks.
- Examine the private website's remaining disk JSON. Mutable application state belongs to the service; operator configuration and editorial material need explicit ownership and import/export rules. Do not assume every JSON file is obsolete.
- Provide AI-guided onboarding for another repository: configure or connect a service, create isolated project/worker profiles, help choose historical import scope, preview the project map and retained history, then verify the resulting workflow. An optional packaged skill teaches the supported process rather than inventing its own migration machinery.

## Initial product boundary

Serve a person or small team operating multiple projects from one machine or one network host. Each project has isolated workers, jobs, messages, map content, review evidence, and provider configuration. Local and remote clients use the same API. Remote access requires scoped authentication and encrypted transport; the first release is not a hostile public multi-customer SaaS platform.

Jobs retain briefs, updates, checkpoints, checklist state, pause/block/completion, and deferred notes. A worker can designate one optional primary job as its resume focus while keeping other jobs active. Messages retain conversations and separate seen and handled states. Review management supports hosted and local executors, attributable evidence, finding decisions, and exact-target safety. The [inherited evidence inventory](inherited-evidence.md) records the legacy dispositions selected by the owning designs.

## Deferred capabilities

Multiple service replicas, distributed review execution, automatic agent-host wakeups, broad provider integrations, and a public marketplace are future capabilities. Design their boundaries now where necessary, but do not claim their implementation or add speculative infrastructure to the first release.

## Licensing

New FireController project material uses Apache-2.0. Referenced historical source retains its original license. The temporary copied legacy runtime is private development tooling, retains its source notices, and is not distributed as the rebuilt product. Any later imported code needs a recorded rights and attribution decision.
