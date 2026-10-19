# Legacy controller migration utility

This optional Go program contains source-specific SQLite knowledge outside the daemon. It accepts an explicitly supplied, completed standalone export; it does not discover installed controllers, open live state, read profiles, contact providers or modify another installation.

## Current implementation

`inspect` reports a compact JSON source digest, byte count, recognized schema versions, table counts and selected migration diagnostics. It checks brief byte limits, unsupported instruction notes and message relationships without printing source paths, identifiers or bodies. Inspection is not complete source validation, an approved import or proof of native review coverage. `prepare-jobs` converts explicitly selected current jobs and checkpoints into the [native prepared import format](../../docs/legacy-adoption.md#selected-job-snapshot-import); other historical coordination classes remain unfinished.

```sh
go build -trimpath -o bin/legacyimport ./tools/legacyimport
bin/legacyimport inspect --snapshot /private/export/controller.sqlite3
```

Supply a sealed export made by the source's supported backup/export procedure. Merely copying a live SQLite main file can omit committed WAL data. The selected file must be absolute, regular and private, with no final symlink or adjacent WAL, SHM or journal. The command opens SQLite read-only and immutable, then verifies source identity and SHA-256 again before returning success. These checks detect observed change; they cannot establish export provenance or protect against hostile same-user replacement. POSIX permissions are checked on Unix; Windows ACL handling remains the operator's responsibility.

The default budget is thirty seconds, adjustable with `--timeout` up to five minutes. Default limits are 512 MiB source bytes, 100,000 rows per recognized table, 500,000 total rows, 8 MiB per cell and 256 MiB total cell bytes. Explicit positive `--max-*` flags can select other limits; see `--help`. Limits refuse the inspection instead of silently truncating. Ordinary errors contain safe fixed codes/messages. Exit status is 0 for inspection success, 2 for invalid arguments, 3 for inspection/output failure and 130 for cancellation. A successful inspection can still report conversion blockers.

## Prepare selected jobs

Create a private strict JSON selection with `version: 1`, the destination project's GitHub `repository_id`, the current `expected_revision` from `fc import state`, a stable source namespace, `workers` mapping exact old worker names to explicitly configured native IDs, selected stable `job_ids` and `checkpoint_mode` of `none`, `latest` or `all`. At most twenty-five jobs and one hundred selected checkpoints fit a batch. All other source classes stay excluded; this command does not import a full job revision timeline or choose a source scope automatically.

```sh
bin/legacyimport prepare-jobs --snapshot /private/export/controller.sqlite3 --selection /private/import/selection.json --output /private/import/jobs.json
```

Preparation shares inspection's admitted read-only handle, limits and final identity/hash checks. It accepts the same five explicit positive overrides: `--max-database-bytes`, `--max-rows-per-table`, `--max-total-rows`, `--max-cell-bytes` and `--max-total-cell-bytes`. Omitting a flag retains its safe inspection default; an explicitly supplied zero or negative bound is invalid. Raising inspection bounds does not relax the native import's content or batch limits. It preserves selected text/times and refuses absent worker mappings, missing dependencies, unsupported content and native byte limits. Source checkpoints have no author field, so their explicit `imported-history` attribution means original author unavailable. Their pointer objects remain inert original JSON, including non-string values, when they fit the native evidence limit. The output is private JSON in an existing private directory; publication never replaces a different existing file. An identical rerun preserves its bytes. Stdout reports only counts and the source digest. A failed stdout write can leave the valid prepared file available; retry with the exact selection/path or inspect that file. No service or provider is contacted, and applying the result is a separate explicit native command.

## Verification

Run `go test -race ./tools/legacyimport/...` and `go vet ./tools/legacyimport/...`. Tests use synthetic SQLite exports and cover recognized versions and optional domains, byte boundaries, message diagnostics, unsupported note history, strict bounds, private file admission, punctuation in paths, cancellation, source conservation and absence of sidecars. `prepare-jobs` coverage also includes the `none`, `latest` and `all` checkpoint modes, explicit worker mappings and missing-mapping refusals, ambiguous or coerced source facts, private output publication, exact repeats and conflicting outputs, recovery after stdout loss, and explicit limit overrides. This is Linux proof of the inspector and converter, not a historical import rehearsal or live cutover.

The [migration contract](../../docs/legacy-adoption.md) owns retention, conversion and application requirements. Native application code must not import this tool's legacy schema package.
