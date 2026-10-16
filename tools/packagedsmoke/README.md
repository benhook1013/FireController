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

## Embedded browser lifecycle

On Linux, `make browser-smoke` runs a separate bounded Chromium case against the existing `fcd`, using the same daemon/bootstrap/authenticated seed helpers. It does not build the daemon or install a browser. Install the Chromium revision selected by the existing pinned Playwright dependency first:

```sh
cd webapp
npx --no-install playwright install chromium
cd ..
make browser-smoke
```

CI installs Chromium and its system dependencies explicitly, then runs this target after the packaged binary smoke. Missing inputs fail rather than silently skipping coverage. The 45-second workload and five-second steps sit inside a 60-second Go-test bound. One narrow browser view checks connection, a separately attributed fixture settings write followed by a live render, full heading bounds, reload session continuity and explicit disconnect cleanup of the session and root appearance. Browser writes are limited to session creation and disconnection. The test uses private disposable credentials, bounded output and owned-process cleanup; it never prints raw browser or daemon output.

`FC_SMOKE_BIN_DIR` selects an absolute already-built input directory as above. An optional absolute `FC_BROWSER_EXECUTABLE` selects an explicitly supplied Chromium executable and is reported as an override; it does not prove the pinned default revision. The ordinary target uses Playwright's installed default. This small regression gate complements the broader executed browser matrices and human GUI review; it does not establish two-project isolation, pixel fidelity, full accessibility, physical-phone behavior, installed hosting or fleet performance. Native Windows/macOS smoke and retained-data container-upgrade validation remain separate release checks.
