# Development

## Implemented boundary

`fc version` and `fc context` are local commands: they contact no service or provider and write no configuration. `context` validates a selected profile and returns its configured identity, excluding the credential reference. It does not verify server identity, authenticate, or establish a connection. The coordination slice also implements the loopback `fcd` service, scoped projects/workers and access grants, basic jobs with revision history, and durable messages with explicit seen/acknowledgment. These operations are available through the [local API](local-api.md). The profile-bound CLI implements the read commands and explicit-key mutations documented below. The embedded [read-only browser](browser-guide.md) consumes the same API and persistent message stream. [Job checkpoints](job-checkpoints.md) implement guarded progress records, latest reads and explicit archives. The optional native CLI/hosted review pipelines, private operator catalog and bound agent tools are described in [review execution](review-execution.md), including their executed proof and live-provider limits. Local operator [backup bundles and inactive restore](backups.md) and managed scheduling, retention and read-only operator status are implemented with full checks and packaged proof. [Narrative updates and checklist state](job-progress.md) are available through the same API and agent clients. [Optional primary-job focus](worker-focus.md) is available through bound API/client/CLI/MCP operations. CLI/MCP persistent subscriptions, worker pause state, deferred notes and combined resume context remain unimplemented.

## Build and checks

Use Go 1.27, Node.js 22.22 or newer, Make and a C compiler such as `gcc` on Linux/WSL. The race-enabled `make check` requires cgo enabled; CGO-free `make build` needs no C compiler:

```sh
npm ci --ignore-scripts --no-audit --no-fund
npm --prefix webapp ci --ignore-scripts --no-audit --no-fund
make check build
./bin/fc version
./bin/fcd --data-dir /private/firecontroller --listen 127.0.0.1:8080
```

`make check` type-checks the browser, runs its focused API/SSE tests, verifies Go formatting, runs Go tests with the race detector and `go vet`, and checks documentation. `make build` regenerates the embedded browser assets and produces CGO-free `fc`, `fcd` and `fcmcp` binaries. The [MCP guide](mcp.md) describes its local stdio tool interface. CI also cross-compiles Windows amd64 and macOS arm64; compilation is not proof that those platforms' configuration or runtime behavior works. Go's `GO` and `GOFMT` Make variables can select a locally installed toolchain. The browser guide documents committed asset verification and direct Go builds without Node.

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

SQLite uses WAL, FULL synchronization and foreign keys on local storage. Write admission is bounded to two seconds, busy timeout is zero, and contention returns `overloaded` without an automatic retry. Explicit project creation verifies GitHub identity outside transactions with a five-second provider deadline; exact retained retries avoid that call. These bounds and focused tests do not establish fleet throughput or a production latency budget. Basic native Windows bootstrap, health/readiness, authenticated instance inspection and reopening the same durable instance passed after correcting drive-path file URIs. These checks do not establish Windows domain operations or filesystem access-control guarantees. Native macOS execution, power-loss durability, backup/restore and hostile same-user filesystem isolation remain unproven.

## Read-only agent commands

Supply `FC_PROFILE` through the agent host's process environment or select `--profile` explicitly. Profiles remain private, per-process bindings; selecting one never changes another agent's identity. Relative credential paths resolve against the profile file, independent of the command working directory.

| Command | Returned scope |
| --- | --- |
| `fc doctor` | Verified instance/project/worker binding, without credentials or a PR scan. |
| `fc workers list` | One compact worker page in the bound project. |
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
| `fc reviews pull NUMBER` | Explicit PR summary and attempt history; optional `--snapshot`. |
| `fc reviews runs NUMBER` | Stored run summaries, default five; optional `--snapshot`. |
| `fc reviews run ID` | Explicit full findings, decisions and fix evidence; optional `--snapshot`. |
| `fc inbox list` | Bound worker's compact message previews; optional unread/unacknowledged filters. |
| `fc inbox thread ID` | Explicit bounded conversation page with full bodies. |

Review queue/run lists accept `--limit` and `--cursor`; omitted limits select five entries. Review selectors validate before credentials or HTTP, and reads perform no provider refresh or review execution. Source timestamps and completeness remain observations, not readiness.

List/history/thread reads accept `--limit` and `--cursor`; jobs accept `--status`, and `jobs list` also accepts `--worker`. Assigned jobs are a compact assignment list, without primary-job, checkpoint or full resume-context claims. Reads never change seen/ack state, job status or configuration. Unknown flags, repeated options and unsupported commands fail before credential loading or network work.

Service reads share one five-second command budget across the initial authenticated instance check and the requested read. Each scoped request carries the expected instance and bound worker; the server rejects a replacement instance before exposing data. The reusable typed client rejects redirects, invalid Unicode/duplicate or incomplete JSON, contradictory pagination and oversized responses. Explicit detail/history limits are larger than compact lists; errors have a separate small budget and preserve safe codes, current revision and request identity without dumping raw responses.

The integrated Linux/WSL binaries passed 17 hands-on checks using a real loopback service and read-only GitHub repository verification for two projects. The checks cover profile interleaving, private relative credentials, compact and explicit reads, scoped pagination, unchanged read state, restart, malformed expected-instance rejection, redirect/privacy failures, Ctrl-C cancellation and the total five-second command deadline. Public unit/race tests and vet provide complementary failure-path evidence; successful cross-compilation is not native execution.

Profile/credential errors and local command results remain separate from service authentication errors. Linux ordinary private-file controls do not establish hostile same-user filesystem isolation. Native Windows CLI/profile and MCP stdio smoke passed. Fifteen additional native Windows service-backed groups passed for doctor, scoped job/message reads and mutations, original-key replay, recipient seen/ack, MCP reads and job-create/replay, and service restart retaining instance identity and receipts. Platform access-control strength, native macOS execution and real host asynchronous delivery remain unproven.

Native execution reads and explicit-key requests are described in [review execution](review-execution.md#bound-execution-tools). Native CLI/hosted evidence reads and guarded decision/fix/finalization commands are described in [review evidence controls](review-controls.md#bound-cli-and-mcp). They operate on native evidence IDs, independently of imported browser run IDs.

## Explicit agent mutations

[Primary-job replacement](worker-focus.md#agent-surfaces-and-proof) uses `jobs primary-set JOB_ID` or `jobs primary-clear`, with the independent focus `--expect-revision` and explicit `--key`. The target worker comes from the profile. It never changes job status or selects a primary automatically.

Quick local mutations use the same private profile and one five-second command budget. They send one request and return a compact completed resource/revision result; they do not reread full history. Such a result is not a pending-operation receipt. Variable-duration [source refresh](review-source.md) and explicit [public review checkpoints](review-publication.md) wait briefly for a useful result, with an operation receipt as fallback and an explicit wait override. Each invocation requires an explicit `--key` of at most 128 printable ASCII bytes without surrounding whitespace. Use a distinct key for each intended action and preserve it with the exact normalized request and binding for recovery. No command silently generates a replacement key or retries after a transport failure.

| Command | Required and optional request fields |
| --- | --- |
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

## Incoming message replay

`fc inbox snapshot` and `fc inbox events` expose the scoped [message-event API](message-events.md). Snapshot includes a compact inbox page and its same-transaction event cursor. Events replays one page or waits up to four seconds for a relevant change, within the normal five-second command budget. Full message bodies and explicit seen/ack actions remain separate calls.

## Combined client, transport and browser proof

The combined implementation checkpoint passed `make check build` and reproducible embedded-asset checks, followed by seven actual browser groups and seven actual MCP groups against the same packaged service. Both suites preserved source and binary hashes and cleaned their owned processes/state. The browser run also inspected seven desktop/phone screenshots, observed zero browser writes and zero JSON polling during a 16.2-second idle window, and exercised the first-read/live-reply race and real service restart.

The combined 64-file Go/module digest was `a894280351e85316561b1850d18c4a776411be59c99454f288960bc7f76f5bf2`; the 83-file manifest including browser source/config/locks/embedded assets was `f33e653c8bac544fa6dff0052cfd06af825003e5e185d8bab17881c6b64a6a2f`. These identify the tested bytes rather than imply proof for subsequent edits. The earlier detailed [MCP](mcp.md#proof-and-limitations) and [browser](browser-guide.md#proof-and-limits) scenarios and platform/host limitations still apply. Browser testing used the available Chromium 153 headless shell with Playwright 1.64.1; local stdio concurrency does not establish installed-host wakeup behavior.

## Small agent-call calibration

On the checkpoint source identified in [checkpoint proof](job-checkpoints.md#proof-and-limits), thirty actual CLI latest-checkpoint reads had a 4.51 ms median, 7.73 ms observed p95 and 8.13 ms maximum. Thirty sequential reads through one actual MCP stdio connection had a 1.83 ms median, 2.57 ms observed p95 and 2.83 ms maximum. All sixty succeeded; six additional outstanding MCP reads completed in 4.87 ms total. The fixture's SQLite row counts stayed unchanged.

Each compact checkpoint contained 560 bytes of structured JSON; CLI stdout added one newline, while MCP's structured result plus compatibility text and JSON-RPC framing totaled 1,267–1,268 bytes. Transport size is not measured model-token cost. The reference was WSL2 x86-64 on an i7-12700KF, ten exposed logical CPUs, about 9.7 GiB memory, Go 1.27.2 and the packaged development binaries. Source and binary hashes stayed unchanged throughout; disposable state/processes were cleaned.

These small samples use one small checkpoint on one developer machine. They establish neither fleet throughput, a release latency budget, remote-network behavior, provider isolation measurements nor installed-model scheduling. The broader [performance plan](acceptance.md#performance-proof) remains unmeasured.
