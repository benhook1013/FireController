# Development

## Implemented boundary

`fc version` and `fc context` are local commands: they contact no service or provider and write no configuration. `context` validates a selected profile and returns its configured identity, excluding the credential reference. It does not verify server identity, authenticate, or establish a connection. The coordination slice also implements the loopback `fcd` service, scoped projects/workers and access grants, basic jobs with revision history, and durable messages with explicit seen/acknowledgment. These operations are available through the [local API](local-api.md). The profile-bound CLI implements the read commands below; mutating CLI commands remain unimplemented. The new browser, live subscriptions, provider review engine, MCP adapter, backups, and advanced job features remain unimplemented.

## Build and checks

Use Go 1.27, Node.js 22 or newer, and Make on Linux/WSL:

```sh
npm ci --ignore-scripts --no-audit --no-fund
make check build
./bin/fc version
./bin/fcd --data-dir /private/firecontroller --listen 127.0.0.1:8080
```

`make check` verifies Go formatting, runs Go tests with the race detector and `go vet`, and checks documentation. `make build` produces CGO-free `fc` and `fcd` binaries. CI also cross-compiles Windows amd64 and macOS arm64; compilation is not proof that those platforms' configuration or runtime behavior works. Go's `GO` and `GOFMT` Make variables can select a locally installed toolchain.

## Private profile file

The default file is `firecontroller/profiles.json` under the OS user configuration directory. Linux honors an absolute `XDG_CONFIG_HOME`, otherwise `$HOME/.config`. Windows uses `APPDATA`; macOS uses `$HOME/Library/Application Support`. An unavailable or relative default directory fails rather than selecting a file from the working directory. An explicit `--profiles-file PATH` overrides the file location.

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

An explicit profile overrides the per-process `FC_PROFILE` default. There is no persisted active profile. The response contains only `profile`, `server_url`, `instance_id`, `project_id`, and `worker_id`; these are local configuration, not verified server facts. Success uses compact JSON on stdout. Failures use a stable code, message, and next action as JSON on stderr with clean stdout. Exit codes are 0 for success, 2 for arguments or invalid read options, 3 for local configuration/credential/output failures, 4 for instance mismatch or conflicts, 5 for connection/service failure or the service deadline, and 6 for explicit caller/signal cancellation. Mutating CLI commands remain unsupported.

## Proof and limits

Focused tests cover interleaved project identities with the same worker name on both shared and distinct service instances, explicit flag precedence, unchanged configuration/environment, absent credential files, strict malformed/ambiguous configuration, malformed raw UTF-8 identity bytes, unpaired surrogate escapes in identities and profile names, preservation of valid Unicode and escape text, unsafe URLs, relative default directories, non-regular configuration paths, and concise errors without private values. Linux tests exercise default-path behavior and FIFO rejection through a deadline-bounded child process, so a missing file-kind guard cannot hang the suite. The file-kind checks address accidental unsupported inputs; they do not protect against hostile same-user filesystem replacement races or establish a deadline for arbitrary filesystem I/O. Other OS mappings require native execution before claiming support. The [agent contract](agent-client.md) owns the broader target, including instance verification and authenticated mutations.

## Coordination service proof

Public HTTP tests exercise bootstrap/restart identity, private credentials, project grants and revocation, stable verified repository binding, strict request parsing, project/principal/filter cursor isolation, retained idempotency receipts, revision races, and atomic domain/history/event writes. Basic job and message operations perform no provider work. A repository resolver is injected for those tests; live GitHub behavior is not claimed by the offline suite. Packaged Linux startup and shutdown checks cover readiness, authenticated instance inspection and domain route registration.

SQLite uses WAL, FULL synchronization and foreign keys on local storage. Write admission is bounded to two seconds, busy timeout is zero, and contention returns `overloaded` without an automatic retry. Explicit project creation verifies GitHub identity outside transactions with a five-second provider deadline; exact retained retries avoid that call. These bounds and focused tests do not establish fleet throughput or a production latency budget. Native Windows/macOS execution, power-loss durability, backup/restore and hostile same-user filesystem isolation remain unproven.

## Read-only agent commands

Supply `FC_PROFILE` through the agent host's process environment or select `--profile` explicitly. Profiles remain private, per-process bindings; selecting one never changes another agent's identity. Relative credential paths resolve against the profile file, independent of the command working directory.

| Command | Returned scope |
| --- | --- |
| `fc doctor` | Verified instance/project/worker binding, without credentials or a PR scan. |
| `fc workers list` | One compact worker page in the bound project. |
| `fc jobs list` | One compact job page; optional worker/status filters. |
| `fc jobs assigned` | One compact page filtered to the bound worker; optional status filter. |
| `fc jobs read ID` | Explicit full job detail and brief. |
| `fc jobs history ID` | Explicit bounded revision-history page. |
| `fc inbox list` | Bound worker's compact message previews; optional unread/unacknowledged filters. |
| `fc inbox thread ID` | Explicit bounded conversation page with full bodies. |

List/history/thread reads accept `--limit` and `--cursor`; jobs accept `--status`, and `jobs list` also accepts `--worker`. Assigned jobs are a compact assignment list, without primary-job, checkpoint or full resume-context claims. Reads never change seen/ack state, job status or configuration. Unknown flags, repeated options and unimplemented mutations fail before credential loading or network work.

Service reads share one five-second command budget across the initial authenticated instance check and the requested read. Each scoped request carries the expected instance and bound worker; the server rejects a replacement instance before exposing data. The reusable typed client rejects redirects, invalid Unicode/duplicate or incomplete JSON, contradictory pagination and oversized responses. Explicit detail/history limits are larger than compact lists; errors have a separate small budget and preserve safe codes, current revision and request identity without dumping raw responses.

Profile/credential errors and local command results remain separate from service authentication errors. Linux ordinary private-file controls do not establish hostile same-user filesystem isolation. Native Windows/macOS execution and real host asynchronous delivery remain unproven.
