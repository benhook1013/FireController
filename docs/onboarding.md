# AI-Guided Onboarding

## Goal and implementation status

Let a user ask their AI to add FireController to an existing project and receive a working, verified setup with clear ownership of the imported information. Local private profile creation and POSIX bound launcher commands are implemented. Guided project registration, historical import and the optional packaged skill remain unimplemented. This is a first-release usability requirement; broad importer coverage is delivered incrementally.

## Supported setup flow

1. Discover the intended repository, remotes, current project instructions, and available FireController instance. Report the selected repository identity; do not infer that another checkout or an already-configured instance is the intended project.
2. Offer simple local installation or connection to an existing instance. Resolve the repository's stable provider identity before creating its project. If that repository already belongs to a project in the instance, report the duplicate and offer to connect to the existing project within the caller's access; do not silently create another. Otherwise create an isolated project. Keep credential handling in the application, not generated shell arguments or tracked files.
3. Select project display name and appearance, worker names/roles, and preferred CLI/native-tool integration. Create private per-agent profiles and any distinct launchers through the supported application commands. The [client contract](agent-client.md) prevents shared mutable actor context.
4. Ask what history should be imported before broad provider collection: date range or PR range, open work only versus selected completed history, review captures if available, and approved repository documents or existing controller exports. Explain expected provider scope and the distinction between public and private source material.
5. Discover candidate project-map tracks, phases, return points, current jobs, and history from those selected sources. Mark AI-inferred structure as a proposal, with source pointers and confidence/uncertainty; do not present an inferred milestone or historical completion as a verified fact.
6. Preview the normalized import, conflicts, public fields, source provenance, missing evidence, and proposed map. Let the user adjust scope and approve the concrete import when required by their onboarding request. An explicit approved plan may be applied without asking again for each row.
7. Apply idempotently through the same authenticated APIs as ordinary clients. Verify project isolation, profile resolution, one job round trip, message delivery, and retained source attribution. Return the useful commands, current assignment pointers, and any unfinished import work.

Onboarding should use a few meaningful questions, not a long setup checklist or repeated permissions for routine reversible steps already authorized. A read-only preview never grants permission to publish private history. Default imported data remains private; enabling public status requires an explicit visibility choice.

## Private agent setup commands

Private client setup is local filesystem work, separate from project registration, historical import and service authorization. The selected commands are:

```sh
fc --profiles-file /private/agent/profiles.json --profile project-builder profile create --server https://controller.example.com --instance INSTANCE_ID --project PROJECT_ID --worker WORKER_ID --credential-file credentials/worker.token
fc --profiles-file /private/agent/profiles.json --profile project-builder profile launcher --output /private/bin/fc-builder
```

`profile create` requires an explicit absolute profile-file path and explicit profile selector. It stores a version-one private configuration containing the supplied binding and credential reference; it never reads the credential, contacts a service or verifies the remote identity. A relative credential reference resolves against that profile file as usual. Use `doctor` for authenticated verification. The command creates a new file atomically and never modifies an existing configuration. A matching existing profile is an idempotent success with no write; a conflicting selection is refused while preserving every existing profile and byte. Use a separate private file for another agent when its desired binding is absent.

`profile launcher` requires an existing selected profile and an explicit absolute output path. On supported POSIX hosts it creates an executable shell launcher that binds the absolute profile file, profile name and current `fc` executable, with an optional explicit `--executable` path. It quotes paths/selectors as literal arguments, forwards caller arguments, and rejects conflicting profile options through the normal duplicate-option guard. Process defaults do not change its binding. Installation never edits PATH, project instructions or another agent's files. Existing identical launcher bytes are an idempotent no-write success; different existing files are refused. Native Windows launcher generation is unsupported in this initial boundary; explicit profiles and per-process `FC_PROFILE` remain available there.

Both commands use compact structured output and safe diagnostics. They publish private files without replacement, preserve unrelated configuration, and report local publication uncertainty instead of removing a file that may already be usable. They create no worker, grant, credential, job, message, review or pending-operation receipt. The broader onboarding and import flow above remains incomplete.

## Import semantics

The user controls historical depth. Support bounded ranges and resuming interrupted collection; do not download all repository or provider history by default. Record source repository, scope, snapshot/version or immutable provider IDs, collection time, and transformation version. Imported text and events are data, not executable instructions.

Keep original facts separate from AI summaries and current editorial structure. An import timestamp is not the original creation time of a job or review. Unknown historical times, missing captures, incomplete pagination, and unavailable proof remain explicit. Selected completed history can populate map/history views without creating active jobs or granting review coverage.

Stable source keys make repeated application idempotent. Conflicting source facts or newer application revisions require a reviewed resolution rather than silent overwrite. Preview fingerprints bind the approved source set; changed inputs trigger another preview. Avoid positional matching by headings or worker display names when stable IDs are available.

Onboarding reads only selected repository sources and scoped provider data. It must not scan unrelated project databases, user-home credentials, other agents' private profiles, or every file under a shared workspace. It must preserve existing project instructions and expose the small proposed integration diff rather than replace `AGENTS.md` wholesale.

## Optional agent skill

Distribute a versioned onboarding/usage skill alongside the supported release. It describes discovery, questions, preview/apply, profile binding, quiet tool use, the optional [agent workflow](agent-workflow.md), and verification using actual documented commands. Clearly separate required tool semantics from recommended working habits. A skill is guidance; it does not replace API validation, impose checkpoint cadence, add hidden quotas, perform automatic external messaging, or imply permission beyond the user's request.

Installation into an agent host is optional and explicit. CLI onboarding works without a particular host or model. Host capability detection identifies whether native tools, asynchronous results, and isolated connection binding are supported. Generated guidance must name limitations instead of promising unsupported wakeups.

Local review execution additionally needs administrator-configured source availability on the server, as owned by [review execution](reviews.md#admission-and-execution). Onboarding reports that capability separately from connecting a client checkout. Required review channels are selected explicitly before aggregate sufficiency can be reported; a jobs/messaging-only project needs no review executor setup. Repository binding still requires its documented provider identity verification.

## Acceptance scenarios

- Add a second project with an identically named worker; the first project's profiles, messages, map, and settings remain unchanged.
- Select open work only or a bounded historical range; collection and imported projections honor that scope, including provider pagination and rate limits.
- Preview without applying, revise the map, and apply twice; one resulting import retains provenance and creates no duplicate job/message/review.
- Interrupt collection or application and resume; retained scope and idempotency keys prevent duplication or silent widening.
- Import incomplete historical evidence; unknown times and unproven review coverage remain visible, and generated summaries do not count as captured review proof.
- Onboard using CLI alone and using an AI-assisted flow; both resolve the same project/worker context and pass the same isolation and communication checks.
