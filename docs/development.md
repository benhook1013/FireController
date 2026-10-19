# Development

## Implemented boundary

`fc version` and `fc context` are local commands: they contact no service or provider and write no configuration. `context` validates a selected profile and returns its configured identity, excluding the credential reference. It does not verify server identity, authenticate, or establish a connection. The coordination slice also implements the loopback `fcd` service, scoped projects/workers and access grants, basic jobs with revision history, and durable messages with explicit seen/acknowledgment. These operations are available through the [local API](local-api.md). The profile-bound CLI implements the read commands and explicit-key mutations documented below. The embedded [browser workspace](browser-guide.md) consumes the same API and persistent message stream. [Job checkpoints](job-checkpoints.md) implement guarded progress records, latest reads and explicit archives. The optional native CLI/hosted review pipelines, private operator catalog and bound agent tools are described in [review execution](review-execution.md), including their executed proof and live-provider limits. Local operator [backup bundles and inactive restore](backups.md) and managed scheduling, retention and read-only operator status are implemented with full checks and packaged proof. [Narrative updates and checklist state](job-progress.md) are available through the same API and agent clients. [Optional primary-job focus](worker-focus.md) is available through bound API/client/CLI/MCP operations. [Worker pause](worker-pause.md) and [deferred notes](notes.md) have API/client/CLI/MCP implementations; each owner records executed proof and remaining boundaries. [Job resume context](job-context.md) combines current state and selected pending coordination through an explicit read; its owner records validation status. The [quiet message wait](message-wait.md) provides one bounded CLI/MCP subscription per call; across-turn host delivery remains unproved.

## Build and checks

Use Go 1.27, Node.js 22.22 or newer, Make and a C compiler such as `gcc` on Linux/WSL. `make check` also requires Python 3.9 or newer for source-converter tests and cgo enabled for race tests; CGO-free `make build` needs no C compiler:

```sh
npm ci --ignore-scripts --no-audit --no-fund
npm --prefix webapp ci --ignore-scripts --no-audit --no-fund
make check build
make smoke
./bin/fc version
./bin/fcd --data-dir /private/firecontroller --listen 127.0.0.1:8080
```

`make check` validates the workflow with pinned actionlint, type-checks the browser, runs its focused API/SSE tests, verifies Go formatting, runs Go tests with the race detector using at most two concurrent packages and an explicit twenty-minute per-package timeout, separately runs the strict maximum-transfer deadline gate without race instrumentation, runs `go vet`, and checks documentation. Local `make test` and `make check` cover every Go package by default; `GO_TEST_PACKAGES` selects an explicit package set for both tests and vet. Linux CI uses six jobs: `rest`, `backup-0`, `backup-1`, `reviewexec-0`, `reviewexec-1` and `reviewruntime`. The package selector uses the successful `go list ./...` result; `reviewruntime` runs its owning package and `rest` receives every other package except backup and reviewexec, including newly added packages. Each backup and reviewexec pair discovers its package’s runnable top-level tests, examples and fuzz seeds, sorts their names and selects complementary alternating halves with anchored exact-name expressions. Both halves in each family must be nonempty, disjoint and cover every discovered name; selecting a parent retains all its subtests and fuzz seeds, while benchmarks are excluded. New tests join exactly one half of their owning family. Each shard preserves the race detector and twenty-minute test budget; all shards finish independently after a failure. `GO_TEST_RUN` defaults to `.` for full local test coverage and carries only the generated exact-name expression in backup and reviewexec CI. The `rest` job also runs browser, formatting, workflow and documentation checks, builds the binaries, and runs packaged, selected legacy-adoption and browser smoke, embedded-asset verification and cross-build checks.

`make test` excludes only the exact top-level `TestReviewTransferMaximumBatchUsesOrdinaryWriteBudget` name from its raced invocation. If successful `go list $(GO_TEST_PACKAGES)` includes `github.com/benhook1013/FireController/internal/reviews`, it first requires that exact test name to be discovered, then runs that same unchanged test explicitly with `-race=false -v -count=1 -timeout=20m`, preserving the actual two-second handler deadline, maximum batch and conservation assertions. Package or exact-test discovery failure fails the target; backup-only and reviewexec-only selections do not compile or test reviews. `GO_TEST_RUN` filters the functional/race selection; the ordinary deadline gate always runs when reviews is selected, even with a custom functional filter. `make check` inherits this behavior. This separates timing qualification from race detection, whose [documented runtime overhead](https://go.dev/doc/articles/race_detector#Runtime_Overheads) can affect [timeout-sensitive tests](https://go.dev/doc/articles/race_detector#Excluding_Tests); it does not relax application deadlines or suppress other tests.

`make build` regenerates the embedded browser assets and produces CGO-free `fc`, `fcd` and `fcmcp` binaries. `make smoke` runs the built daemon, CLI and MCP binaries together in a private synthetic fixture, checking bootstrap, exact mutation replay, bounded stream cancellation, same-session reuse and restart. It never rebuilds the binaries; the [packaged smoke guide](../tools/packagedsmoke/README.md) owns its budgets and proof limits. The [MCP guide](mcp.md) describes its local stdio tool interface. CI also cross-compiles Windows amd64 and macOS arm64 and runs the packaged workload on native Windows/macOS runners. The required `validate` result requires all six Linux shards and both native jobs to pass. The packaged smoke guide records actual native outcomes and remaining limits; cross-compilation alone is not runtime proof. Go's `GO` and `GOFMT` Make variables can select a locally installed toolchain. The browser guide documents committed asset verification and direct Go builds without Node.

On Linux, install the Chromium revision for the pinned Playwright dependency and run `make browser-smoke` for the separate actual embedded-browser lifecycle check. The [packaged smoke guide](../tools/packagedsmoke/README.md#embedded-browser-lifecycle) owns setup, budgets and limitations. Linux CI's `rest` job runs `make smoke`, `make legacyimport migration-smoke` and `make browser-smoke` after the service build. Native Windows/macOS CI runs the coordination and upgrade packaged workloads; it does not run the Linux migration or browser targets.

Explicit [browser coordination views](browser-coordination.md) read worker pause/focus and note current/history data without mutation or automatic archive expansion.

## Private profile file

Use the local [profile setup commands](onboarding.md#private-agent-setup-commands) to create a private configuration and optional POSIX bound launcher. Setup returns the configured identity with `verified: false`; `doctor` performs the separate authenticated check. Creation requires explicit profile selectors, preserves existing files, and never provisions or reads credentials.

The default file is `firecontroller/profiles.json` under the OS user configuration directory. Linux honors an absolute `XDG_CONFIG_HOME`, otherwise `$HOME/.config`. Windows uses `APPDATA`; macOS uses `$HOME/Library/Application Support`. An unavailable or relative default directory fails rather than selecting a file from the working directory. An explicit `--profiles-file PATH` overrides the file location. CLI and MCP use one shared resolver. Windows prefers the exact `APPDATA` spelling and selects a stable case variant only when it is absent; neither interface depends on map iteration order. The shared resolver table passed on Linux and in a native Windows Go test executable; this is focused path-selection proof, not a new claim about Windows filesystem isolation.

Store this file outside the tracked checkout with permissions suitable for private configuration:

```json
{
  "version": 1,
  "profiles": {
    "project-builder": {
      "server_url": "http://127.0.0.1:8080",
      "instance_id": "instance-local",
      "project_id": "project-example",
      "worker_id": "builder",
      "credential_file": "credentials/builder.token"
    }
  }
}
```

The configuration must be a regular file containing valid UTF-8 and strict JSON with known, exactly cased fields and unique keys, at most 1 MiB. The CLI checks file kind before opening and again before reading; FIFO, device, and directory inputs fail with `profiles_not_regular`. Malformed UTF-8 bytes and unpaired Unicode surrogate escapes fail with `profiles_invalid_encoding` before JSON decoding can repair identity text. Valid surrogate pairs, Unicode literals (including U+FFFD), and escaped-backslash literal text remain valid. All configured profiles currently require valid nonempty identity fields and a credential-file reference. Remote URLs require HTTPS; HTTP is restricted to literal loopback addresses or `localhost`. URLs cannot contain embedded credentials, queries, or fragments. The local `context` command validates the credential reference as required text and never resolves or opens it. Service commands resolve a relative reference against the profile file directory and load a bounded owner-private regular credential file; they never evaluate shell or environment expressions.

```sh
fc --profiles-file /private/profiles.json --profile project-builder context
```

An explicit profile overrides the per-process `FC_PROFILE` default. There is no persisted active profile. The response contains only `profile`, `server_url`, `instance_id`, `project_id`, and `worker_id`; these are local configuration, not verified server facts. Success uses compact JSON on stdout. Failures use a stable code, message, and next action as JSON on stderr with clean stdout. Exit codes are 0 for success, 2 for arguments or invalid operation options, 3 for local configuration/credential/output failures, 4 for instance mismatch or conflicts, 5 for connection/service failure or the service deadline, and 6 for explicit caller/signal cancellation. Mutation recovery uses the caller's explicit idempotency key.

## Proof and limits

Focused tests cover interleaved project identities with the same worker name on both shared and distinct service instances, explicit flag precedence, unchanged configuration/environment, absent credential files, strict malformed/ambiguous configuration, malformed raw UTF-8 identity bytes, unpaired surrogate escapes in identities and profile names, preservation of valid Unicode and escape text, unsafe URLs, relative default directories, non-regular configuration paths, and concise errors without private values. Linux tests exercise default-path behavior and FIFO rejection through a deadline-bounded child process, so a missing file-kind guard cannot hang the suite. The file-kind checks address accidental unsupported inputs; they do not protect against hostile same-user filesystem replacement races or establish a deadline for arbitrary filesystem I/O. Native Windows explicit/default APPDATA profile lookup, valid Unicode and quiet configuration errors passed; broader platform/filesystem guarantees remain outside this proof. The [agent contract](agent-client.md) owns the broader target, including instance verification and authenticated mutations.

## Coordination service proof

Public HTTP tests exercise bootstrap/restart identity, private credentials, project grants and revocation, stable verified repository binding, strict request parsing, project/principal/filter cursor isolation, retained idempotency receipts, revision races, and atomic domain/history/event writes. Basic job and message operations perform no provider work. A repository resolver is injected for those tests; live GitHub behavior is not claimed by the offline suite. Packaged Linux startup and shutdown checks cover readiness, authenticated instance inspection and domain route registration.

SQLite uses WAL, FULL synchronization and foreign keys on local storage. Write admission is bounded to two seconds, busy timeout is zero, and contention returns `overloaded` without an automatic retry. Explicit project creation verifies GitHub identity outside transactions with a five-second provider deadline; exact retained retries avoid that call. These bounds and focused tests do not establish fleet throughput or a production latency budget. Basic native Windows bootstrap, health/readiness, authenticated instance inspection and reopening the same durable instance passed after correcting drive-path file URIs. These checks do not establish Windows domain operations or filesystem access-control guarantees. The [packaged smoke guide](../tools/packagedsmoke/README.md#native-platform-gate) records newer Windows/macOS runner coverage. Power-loss durability, off-host backup durability, operational disaster cutover and hostile same-user filesystem isolation remain unproven. Executed local backup/restore checks and their limits are recorded in [backup proof](backups.md#proof-status-and-limits).

## Read-only agent commands

Supply `FC_PROFILE` through the agent host's process environment or select `--profile` explicitly. Profiles remain private, per-process bindings; selecting one never changes another agent's identity. Relative credential paths resolve against the profile file, independent of the command working directory.

| Command | Returned scope |
| --- | --- |
| `fc doctor` | Verified instance/project/worker binding, without credentials or a PR scan. |
| `fc workers list` | One compact worker page in the bound project. |
| `fc jobs context JOB_ID [--limit N]` | Complete current brief/checkpoint/checklist plus bound-worker focus/pause and pending previews, explicit omissions, continuation cursors and incoming-event watermark. |
| `fc notes read NOTE_ID` | One full current note. |
| `fc notes list` | One compact pending page for the bound worker and its currently assigned jobs; explicit scope/filter flags narrow or broaden selection. |
| `fc notes history NOTE_ID` | One compact immutable-revision page. |
| `fc notes revision NOTE_ID REVISION` | One full selected revision. |
| `fc workers pause-state` | Bound worker’s explicit pause, reason and independent revision. |
| `fc workers pause-history` | One explicit bounded pause-history page. |
| `fc jobs list` | One compact job page; optional worker/status filters. |
| `fc jobs primary` | Bound worker’s compact primary-job selection and independent revision. |
| `fc jobs primary-history` | One explicit bounded page of primary-job changes. |
| `fc jobs assigned` | One compact page filtered to the bound worker; optional status filter. |
| `fc jobs read ID` | Explicit full job detail and brief. |
| `fc jobs history ID` | Explicit bounded revision-history page. |
| `fc jobs checkpoint-latest ID` | Explicit latest checkpoint with current job revision, or an explicit null checkpoint. |
| `fc jobs checkpoints ID` | Explicit newest-first checkpoint archive page. |
| `fc jobs updates ID` | Explicit newest-first narrative-update page. |
| `fc jobs checklist ID` | Current ordered checklist, its revision and the current job revision. |
| `fc jobs checklist-history ID` | Explicit newest-first full checklist revision page. |
| `fc reviews execution` | Stored project execution configuration and revision; no provider check. |
| `fc reviews operation ID` | Explicit retained native operation state and reason. |
| `fc reviews queue` | Stored review queue; defaults to five compact PR entries. |
| `fc reviews pull NUMBER` | Compact PR summary without loading attempt history; optional `--snapshot`. |
| `fc reviews attempts NUMBER` | One newest-first retained attempt page with original producer identity, default five; optional `--snapshot`, `--limit` and `--cursor`, without automatic paging. |
| `fc reviews runs NUMBER` | Stored run summaries, default five; optional `--snapshot`. |
| `fc reviews run ID` | Explicit full findings, decisions and fix evidence; optional `--snapshot`. |
| `fc inbox list` | Bound worker's compact message previews; optional unread/unacknowledged filters. |
| `fc inbox thread ID` | Explicit bounded conversation page with full bodies. |

Administrator setup also supports [prepared review import](review-import.md) through `fc reviews import-preview --file ABSOLUTE_PATH` and `fc reviews import-apply --file ABSOLUTE_PATH --preview SHA256 --key KEY`. Preview is provider-free and read-only; apply uses the existing guarded observation receipt. These commands require deliberately selected administrator access rather than an ordinary worker credential.

Opt-in [selected history collection](history-collection.md#operator-and-agent-interfaces) adds administrator `fc history` commands for explicit PR acquisition, bounded waits, retained operation/page reads, guarded resume/cancel and private capture export. It performs no automatic import and adds no MCP inventory. Its owning contract records current proof and deployment limits.

Review queue/run lists accept `--limit` and `--cursor`; omitted limits select five entries. Review selectors validate before credentials or HTTP, and reads perform no provider refresh or review execution. Source timestamps and completeness remain observations, not readiness.

List/history/thread reads accept `--limit` and `--cursor`; jobs accept `--status`, and `jobs list` also accepts `--worker`. The list-only `--status open` filter selects active, blocked and parked jobs in priority order across pages; see [job read filters](local-api.md#basic-jobs). MCP `jobs_list` and `jobs_assigned` support the same filter, while job mutations accept only stored statuses. Assigned jobs are a compact assignment list, without primary-job, checkpoint or full resume-context claims. Reads never change seen/ack state, job status or configuration. Unknown flags, repeated options and unsupported commands fail before credential loading or network work.

Service reads share one five-second command budget across the initial authenticated instance check and the requested read. Each scoped request carries the expected instance and bound worker; the server rejects a replacement instance before exposing data. The reusable typed client rejects redirects, invalid Unicode/duplicate or incomplete JSON, contradictory pagination and oversized responses. Explicit detail/history limits are larger than compact lists; errors have a separate small budget and preserve safe codes, current revision and request identity without dumping raw responses.

The integrated Linux/WSL binaries passed 17 hands-on checks using a real loopback service and read-only GitHub repository verification for two projects. The checks cover profile interleaving, private relative credentials, compact and explicit reads, scoped pagination, unchanged read state, restart, malformed expected-instance rejection, redirect/privacy failures, Ctrl-C cancellation and the total five-second command deadline. Public unit/race tests and vet provide complementary failure-path evidence; successful cross-compilation is not native execution.

Profile/credential errors and local command results remain separate from service authentication errors. Linux ordinary private-file controls do not establish hostile same-user filesystem isolation. Native Windows CLI/profile and MCP stdio smoke passed. Fifteen additional native Windows service-backed groups passed for doctor, scoped job/message reads and mutations, original-key replay, recipient seen/ack, MCP reads and job-create/replay, and service restart retaining instance identity and receipts. Platform access-control strength and real host asynchronous delivery remain unproven; the packaged smoke guide records newer native macOS workload proof.

Native execution reads and explicit-key requests are described in [review execution](review-execution.md#bound-execution-tools). Native CLI/hosted evidence reads and guarded decision/fix/finalization commands are described in [review evidence controls](review-controls.md#bound-cli-and-mcp). They operate on native evidence IDs, independently of imported browser run IDs.

## Explicit agent mutations

[Primary-job replacement](worker-focus.md#agent-surfaces-and-proof) uses `jobs primary-set JOB_ID` or `jobs primary-clear`, with the independent focus `--expect-revision` and explicit `--key`. The target worker comes from the profile. It never changes job status or selects a primary automatically.

Quick local mutations use the same private profile and one five-second command budget. They send one request and return a compact completed resource/revision result; they do not reread full history. Such a result is not a pending-operation receipt. Variable-duration [review execution](review-execution.md#collecting-review-results), [source refresh](review-source.md) and explicit [public review checkpoints](review-publication.md) wait briefly for a useful result, with an operation receipt as fallback and an explicit wait override. Each invocation requires an explicit `--key` of at most 128 printable ASCII bytes without surrounding whitespace. Use a distinct key for each intended action and preserve it with the exact normalized request and binding for recovery. No command silently generates a replacement key or retries after a transport failure.

| Command | Required and optional request fields |
| --- | --- |
| `fc notes add --body TEXT --key KEY` | Reminder for the bound worker by default; explicit job/phase/worker and body-file selection follow the [note contract](notes.md). |
| `fc notes update NOTE_ID --expect-revision N --key KEY` | Patch only supplied scope, kind, body or status fields; consuming a note requires no unchanged body echo. |
| `fc workers pause --reason TEXT --key KEY --expect-revision N` | Guarded bound-worker coordination pause; reason is explicit, including an empty string. |
| `fc workers resume --key KEY --expect-revision N` | Explicitly clear the pause and reason without changing jobs or focus. |
| `fc jobs create --key KEY --title TITLE` | Optional `--brief TEXT`, `--worker ID`, `--status STATUS`; defaults to active. |
| `fc jobs edit ID --key KEY --expect-revision N` | At least one of `--title`, `--brief`, `--worker`, `--status`; empty brief or worker explicitly clears that field. |
| `fc jobs checkpoint ID --key KEY --expect-revision N --done TEXT --next TEXT` | Optional `--blocker TEXT`, repeatable `--evidence REF`; append immutable progress without editing the job. |
| `fc jobs update ID --key KEY --expect-revision N --body TEXT` | Optional repeatable `--evidence REF`; append a narrative update without editing the job or checkpoint. |
| `fc jobs checklist-set ID --key KEY --expect-revision N --expect-checklist-revision C` | Exactly one of `--items JSON` or `--items-file PATH`; guarded ordered replacement, with unchanged content reported explicitly. |
| `fc inbox send --key KEY --to WORKER_ID --body TEXT` | Optional `--reply-to MESSAGE_ID`; the bound worker is the sender. |
| `fc inbox seen MESSAGE_ID --key KEY` | Only the bound actual recipient can change seen state. |
| `fc inbox ack MESSAGE_ID --key KEY` | Only the bound actual recipient can acknowledge; acknowledgment also establishes seen state. |

The parser rejects incomplete, repeated, oversized and malformed arguments before opening profiles or credentials or contacting the service. Brief and body text remain exact UTF-8; selected title/assignment normalization follows the API. Omitted edit fields stay unchanged. Guarded edits return the current revision on conflict. A retained key replays the original receipt after current authorization; it never overwrites a newer revision.

A disconnected, timed-out or canceled write can already have committed. Such diagnostics include `outcome_unknown: true` and instruct the caller to recover with the same key, request and instance/worker binding. Invalid receipts and failures writing a successful receipt to stdout also preserve that uncertainty. Pre-dispatch cancellation and validated request refusals do not claim an uncertain commit. Never turn an unknown outcome into a new action by creating another key. The server remains the authority for receipt replay and access.

This slice does not persist a client retry journal. Callers retain the key, request and originating binding; changing a profile or rebinding to a restored instance is not recovery of that pending action.

Administrator [selected queue edits](review-workspace.md#selected-queue-edits) use `reviews queue-edit --file /private/queue-edit.json --key KEY`. They change only selected PR metadata and visibility, preserving omitted work and review history. They keep the same explicit-key recovery and ordinary command budget; they do not request reviews or provider refreshes.

The [project-map agent commands](project-map.md#agent-commands) add explicit compact map reads, full guarded editorial replacements and independent job/workstream references. Ordinary job/context reads do not expand map content.

## Incoming message replay

`fc inbox snapshot` and `fc inbox events` expose the scoped [message-event API](message-events.md). Snapshot includes a compact inbox page and its same-transaction event cursor. Events replays one page or waits up to four seconds for a relevant change, within the normal five-second command budget. Full message bodies and explicit seen/ack actions remain separate calls. The explicit [quiet wait](message-wait.md), `fc inbox wait --cursor CURSOR`, consumes the persistent stream and returns the first new incoming-message event page or a bounded timeout; its default wait is 25 seconds, with no intermediate heartbeat output.

## Combined client, transport and browser proof

The combined implementation checkpoint passed `make check build` and reproducible embedded-asset checks, followed by seven actual browser groups and seven actual MCP groups against the same packaged service. Both suites preserved source and binary hashes and cleaned their owned processes/state. The browser run also inspected seven desktop/phone screenshots, observed zero browser writes and zero JSON polling during a 16.2-second idle window, and exercised the first-read/live-reply race and real service restart.

The combined 64-file Go/module digest was `a894280351e85316561b1850d18c4a776411be59c99454f288960bc7f76f5bf2`; the 83-file manifest including browser source/config/locks/embedded assets was `f33e653c8bac544fa6dff0052cfd06af825003e5e185d8bab17881c6b64a6a2f`. These identify the tested bytes rather than imply proof for subsequent edits. The earlier detailed [MCP](mcp.md#proof-and-limitations) and [browser](browser-guide.md#proof-and-limits) scenarios and platform/host limitations still apply. Browser testing used the available Chromium 153 headless shell with Playwright 1.64.1; local stdio concurrency does not establish installed-host wakeup behavior.

## Small agent-call calibration

On the checkpoint source identified in [checkpoint proof](job-checkpoints.md#proof-and-limits), thirty actual CLI latest-checkpoint reads had a 4.51 ms median, 7.73 ms observed p95 and 8.13 ms maximum. Thirty sequential reads through one actual MCP stdio connection had a 1.83 ms median, 2.57 ms observed p95 and 2.83 ms maximum. All sixty succeeded; six additional outstanding MCP reads completed in 4.87 ms total. The fixture's SQLite row counts stayed unchanged.

Each compact checkpoint contained 560 bytes of structured JSON; CLI stdout added one newline, while MCP's structured result plus compatibility text and JSON-RPC framing totaled 1,267–1,268 bytes. Transport size is not measured model-token cost. The reference was WSL2 x86-64 on an i7-12700KF, ten exposed logical CPUs, about 9.7 GiB memory, Go 1.27.2 and the packaged development binaries. Source and binary hashes stayed unchanged throughout; disposable state/processes were cleaned.

These small samples use one small checkpoint on one developer machine. They establish neither fleet throughput, a release latency budget, remote-network behavior, provider isolation measurements nor installed-model scheduling. The broader [performance plan](acceptance.md#performance-proof) remains unmeasured.

## Public website administration

The profile-bound `fc publication read`, `preview`, `apply` and `unpublish` commands configure opt-in anonymous project content under an administrator profile. The [public website contract](public-site.md#agent-setup-commands) owns the input format, preview hash, exact retry and private/public field boundary. Routine worker commands do not load publication manifests or expose private records to the public site.

`make legacyimport` separately builds the optional source-specific [migration utility](../tools/legacyimport/README.md). Linux CI's `rest` job builds the utility and runs `make migration-smoke` with the built utility and service binaries against a synthetic selected-job import workload. This does not qualify a real historical import or live cutover. The ordinary service build has no runtime dependency on a legacy controller. Native `fc import` setup commands support selected jobs/checkpoints and historical messages; their scope is documented at the [adoption owner](legacy-adoption.md). The separate Python control adapter requires an explicitly provided canonical source frame; `make source-converter-check` covers standalone input/output contracts, with source-helper tests enabled only by an explicit trusted source root.

## Per-PR review round controls

The [round-control guide](review-round-controls.md#bound-commands-and-api) owns administrator `fc reviews rounds read|grant|close`, additional minimum/maximum/exact semantics, MCP arguments and exact-key recovery. These are compact local operations; granting or closing a channel does not itself start or cancel provider work.

## Start the next review

The bound command selects one channel’s earliest unfinished target through the service. An expected PR is optional and asserts the selection. Each call authorizes at most one review, using an explicit key; ordinary inspection reads stay provider-free. The [round-control contract](review-round-controls.md#one-explicit-channel-front-run) owns selection, waiting, replay and recovery.

```sh
fc reviews start --channel cli --key next-cli-review
fc reviews start --channel hosted --expect-pr 42 --key hosted-42 --wait-seconds 900
fc reviews run-read --selection-id review_selection_0123456789abcdef0123456789abcdef
```

The default short wait returns a useful terminal result when available, or the original compact selection receipt when work remains pending. Immediate return uses `--wait-seconds 0`. Preserve the original key and arguments after uncertain output; another key authorizes another selection. Historical `reviews run RUN_ID` remains a separate retained-history read.
