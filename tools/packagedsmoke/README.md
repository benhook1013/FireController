# Packaged binary smoke

Run `make check build` followed by `make smoke`. The smoke target executes the existing `bin/fc`, `bin/fcd` and `bin/fcmcp`; it never builds or changes those binaries. CI runs it after the ordinary checks and build. Missing binaries fail explicitly. The `packaged_smoke` build tag excludes this process-level check from ordinary `go test ./...`; formatting still includes the file.

The single case has a 45-second context and a 60-second test timeout, with five-second command/readiness/shutdown bounds. It verifies actual daemon bootstrap/health and private credentials, actual CLI profile creation/doctor/job mutation/exact replay/read, official Go MCP SDK initialization/discovery/read, cancellation of an established quiet stream with concurrent reads and same-session reuse, and daemon restart retaining identity/state/receipts. It checks only the tools needed by the workload, rather than duplicating the tool catalog or a fixed discovery count.

A stopped fixture is provisioned using four authenticated handler calls and one synthetic repository resolver. This trusted setup is separate from the actual executable proof; it does not claim real GitHub repository verification. No SQL manufactures domain state or receipts. Fixture profiles, credentials and databases are private disposable test data. Child environments contain no installed credentials or provider catalog. Unexpected HTTP uses a local denial proxy; there is no provider, Git or review workload. The check does not claim a hostile-network sandbox.

Child stdout must be JSON/protocol data; diagnostics never echo captured private output. Owned processes and local HTTP servers are joined during cleanup, including failures. Logs report input binary hashes and elapsed time. This is a bounded Linux/WSL and Linux-CI smoke, not browser, container, remote-host, cross-platform runtime, release provenance or throughput proof.

An explicit absolute input directory can validate another already-built set without overwriting the repository outputs:

```sh
FC_SMOKE_BIN_DIR=/absolute/private/bin make smoke
```

Such a run proves only those supplied inputs. Build/source provenance remains the caller's responsibility.
