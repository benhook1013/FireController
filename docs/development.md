# Development

## Implemented boundary

The first Go slice implements `fc version` and `fc context`. These are local commands: they contact no service or provider and write no configuration. `context` validates a selected profile and returns its configured identity, excluding the credential reference. It does not verify server identity, authenticate, or establish a connection. The coordination slice also implements the loopback `fcd` service, scoped projects/workers and access grants, basic jobs with revision history, and durable messages with explicit seen/acknowledgment. These operations are available through the [local API](local-api.md); the CLI still has no service commands. The new browser, live subscriptions, provider review engine, MCP adapter, backups, and advanced job features remain unimplemented.

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

The configuration must be a regular file containing valid UTF-8 and strict JSON with known, exactly cased fields and unique keys, at most 1 MiB. The CLI checks file kind before opening and again before reading; FIFO, device, and directory inputs fail with `profiles_not_regular`. Malformed UTF-8 bytes and unpaired Unicode surrogate escapes fail with `profiles_invalid_encoding` before JSON decoding can repair identity text. Valid surrogate pairs, Unicode literals (including U+FFFD), and escaped-backslash literal text remain valid. All configured profiles currently require valid nonempty identity fields and a credential-file reference. Remote URLs require HTTPS; HTTP is restricted to literal loopback addresses or `localhost`. URLs cannot contain embedded credentials, queries, or fragments. This command validates the credential reference as required text but never resolves or opens it; credential loading remains outside this slice.

```sh
fc --profiles-file /private/profiles.json --profile project-builder context
```

An explicit profile overrides the per-process `FC_PROFILE` default. There is no persisted active profile. The response contains only `profile`, `server_url`, `instance_id`, `project_id`, and `worker_id`; these are local configuration, not verified server facts. Success uses compact JSON on stdout. Failures use a stable code, message, and next action as JSON on stderr with clean stdout. Current exit codes are 0 for success, 2 for arguments, and 3 for configuration/output failures; network and mutation outcomes are outside this slice.

## Proof and limits

Focused tests cover interleaved project identities with the same worker name on both shared and distinct service instances, explicit flag precedence, unchanged configuration/environment, absent credential files, strict malformed/ambiguous configuration, malformed raw UTF-8 identity bytes, unpaired surrogate escapes in identities and profile names, preservation of valid Unicode and escape text, unsafe URLs, relative default directories, non-regular configuration paths, and concise errors without private values. Linux tests exercise default-path behavior and FIFO rejection through a deadline-bounded child process, so a missing file-kind guard cannot hang the suite. The file-kind checks address accidental unsupported inputs; they do not protect against hostile same-user filesystem replacement races or establish a deadline for arbitrary filesystem I/O. Other OS mappings require native execution before claiming support. The [agent contract](agent-client.md) owns the broader target, including instance verification and authenticated mutations.

## Coordination service proof

Public HTTP tests exercise bootstrap/restart identity, private credentials, project grants and revocation, stable verified repository binding, strict request parsing, project/principal/filter cursor isolation, retained idempotency receipts, revision races, and atomic domain/history/event writes. Basic job and message operations perform no provider work. A repository resolver is injected for those tests; live GitHub behavior is not claimed by the offline suite. Packaged Linux startup and shutdown checks cover readiness, authenticated instance inspection and domain route registration.

SQLite uses WAL, FULL synchronization and foreign keys on local storage. Write admission is bounded to two seconds, busy timeout is zero, and contention returns `overloaded` without an automatic retry. Explicit project creation verifies GitHub identity outside transactions with a five-second provider deadline; exact retained retries avoid that call. These bounds and focused tests do not establish fleet throughput or a production latency budget. Basic native Windows bootstrap, health/readiness, authenticated instance inspection and reopening the same durable instance passed after correcting drive-path file URIs. These checks do not establish Windows domain operations or filesystem access-control guarantees. Native macOS execution, power-loss durability, backup/restore and hostile same-user filesystem isolation remain unproven.
