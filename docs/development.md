# Development and CLI Foundation

## Implemented boundary

The first Go slice implements `fc version` and `fc context`. These are local commands: they contact no service or provider and write no configuration. `context` validates a selected profile and returns its configured identity, excluding the credential reference. It does not verify server identity, authenticate, or establish a connection. Jobs, messaging, the API, and the browser remain unimplemented.

## Build and checks

Use Go 1.27, Node.js 22 or newer, and Make on Linux/WSL:

```sh
npm ci --ignore-scripts --no-audit --no-fund
make check build
./bin/fc version
```

`make check` verifies Go formatting, runs Go tests with the race detector and `go vet`, and checks documentation. `make build` produces the standalone CLI. CI also cross-compiles Windows amd64 and macOS arm64; compilation is not proof that those platforms' configuration or runtime behavior works. Go's `GO` and `GOFMT` Make variables can select a locally installed toolchain.

## Private profile file

The default file is `firecontroller/profiles.json` under the OS user configuration directory. Linux honors an absolute `XDG_CONFIG_HOME`, otherwise `$HOME/.config`. Windows uses `APPDATA`; macOS uses `$HOME/Library/Application Support`. An unavailable or relative default directory fails rather than selecting a file from the working directory. An explicit `--profiles-file PATH` overrides the file location.

Store this file outside the tracked checkout with permissions suitable for private configuration:

```json
{
  "version": 1,
  "profiles": {
    "alpha-overseer": {
      "server_url": "http://127.0.0.1:8080",
      "instance_id": "instance-a",
      "project_id": "project-alpha",
      "worker_id": "overseer",
      "credential_file": "credentials/alpha.token"
    }
  }
}
```

The file must be strict JSON with known, exactly cased fields and unique keys, at most 1 MiB. All configured profiles currently require valid nonempty identity fields and a credential-file reference. Remote URLs require HTTPS; HTTP is restricted to literal loopback addresses or `localhost`. URLs cannot contain embedded credentials, queries, or fragments. This command never opens the referenced credential file.

```sh
fc --profiles-file /private/profiles.json --profile alpha-overseer context
```

An explicit profile overrides the per-process `FC_PROFILE` default. There is no persisted active profile. The response contains only `profile`, `server_url`, `instance_id`, `project_id`, and `worker_id`; these are local configuration, not verified server facts. Success uses compact JSON on stdout. Failures use a stable code, message, and next action as JSON on stderr with clean stdout. Current exit codes are 0 for success, 2 for arguments, and 3 for configuration/output failures; network and mutation outcomes are outside this slice.

## Proof and limits

Focused tests cover interleaved project identities with the same worker name, explicit flag precedence, unchanged configuration/environment, absent credential files, strict malformed/ambiguous configuration, unsafe URLs, relative default directories, and concise errors without private values. Linux tests exercise default-path behavior. Other OS mappings require native execution before claiming support. The [agent contract](agent-client.md) owns the broader target, including instance verification and authenticated mutations.
