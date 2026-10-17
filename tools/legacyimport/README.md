# Legacy controller migration utility

This optional Go program contains source-specific SQLite knowledge outside the daemon. It accepts an explicitly supplied, completed standalone export; it does not discover installed controllers, open live state, read profiles, contact providers or modify another installation.

## Current implementation

`inspect` reports a compact JSON source digest, byte count, recognized schema versions, table counts and selected migration diagnostics. It checks brief byte limits, unsupported instruction notes and message relationships without printing source paths, identifiers or bodies. Inspection is not complete source validation, conversion, an approved import or proof of native review coverage. Conversion and guarded historical coordination application remain unimplemented.

```sh
go build -trimpath -o bin/legacyimport ./tools/legacyimport
bin/legacyimport inspect --snapshot /private/export/controller.sqlite3
```

Supply a sealed export made by the source's supported backup/export procedure. Merely copying a live SQLite main file can omit committed WAL data. The selected file must be absolute, regular and private, with no final symlink or adjacent WAL, SHM or journal. The command opens SQLite read-only and immutable, then verifies source identity and SHA-256 again before returning success. These checks detect observed change; they cannot establish export provenance or protect against hostile same-user replacement. POSIX permissions are checked on Unix; Windows ACL handling remains the operator's responsibility.

The default budget is thirty seconds, adjustable with `--timeout` up to five minutes. Default limits are 512 MiB source bytes, 100,000 rows per recognized table, 500,000 total rows, 8 MiB per cell and 256 MiB total cell bytes. Explicit positive `--max-*` flags can select other limits; see `--help`. Limits refuse the inspection instead of silently truncating. Ordinary errors contain safe fixed codes/messages. Exit status is 0 for inspection success, 2 for invalid arguments, 3 for inspection/output failure and 130 for cancellation. A successful inspection can still report conversion blockers.

## Verification

Run `go test -race ./tools/legacyimport/...` and `go vet ./tools/legacyimport/...`. Tests use synthetic SQLite exports and cover recognized versions and optional domains, byte boundaries, message diagnostics, unsupported note history, strict bounds, private file admission, punctuation in paths, cancellation, source conservation and absence of sidecars. This is Linux proof of the inspector, not a historical import rehearsal or live cutover.

The [migration contract](../../docs/legacy-adoption.md) owns retention, conversion and application requirements. Native application code must not import this tool's legacy schema package.
