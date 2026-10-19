# Legacy controller migration utility

These optional tools keep source-specific SQLite and policy knowledge outside the daemon. The inspection and conversion commands accept explicitly supplied sealed exports without provider access. The separate source-owner acquisition command deliberately queries GitHub and allows Git proof writes only in an independently owned scratch clone; it never discovers an installation or opens live source state.

## Current implementation

`inspect` reports a compact JSON source digest, byte count, recognized schema versions, table counts and selected migration diagnostics. It checks brief byte limits, unsupported instruction notes and message relationships without printing source paths, identifiers or bodies. Inspection is not complete source validation, an approved import or proof of native review coverage. `prepare-jobs` converts explicitly selected current jobs and checkpoints into the [native prepared import format](../../docs/legacy-adoption.md#selected-job-snapshot-import); `prepare-messages` converts explicitly selected historical messages into the same native batch. The separate `prepare_source_reviews.py` adapter prepares explicitly selected completed review runs and findings as additive native transfer batches. Notes, updates, checklists and full old job histories remain outside these selected paths.

```sh
go build -trimpath -o bin/legacyimport ./tools/legacyimport
bin/legacyimport inspect --snapshot /private/export/controller.sqlite3
```

Supply a sealed export made by the source's supported backup/export procedure. Merely copying a live SQLite main file can omit committed WAL data. The selected file must be absolute, regular and private, with no final symlink or adjacent WAL, SHM or journal. The command opens SQLite read-only and immutable, then verifies source identity and SHA-256 again before returning success. These checks detect observed change; they cannot establish export provenance or protect against hostile same-user replacement. POSIX permissions are checked on Unix; Windows ACL handling remains the operator's responsibility.

The default budget is thirty seconds, adjustable with `--timeout` up to five minutes. Default limits are 512 MiB source bytes, 100,000 rows per recognized table, 500,000 total rows, 8 MiB per cell and 256 MiB total cell bytes. Explicit positive `--max-*` flags can select other limits; see `--help`. Limits refuse the inspection instead of silently truncating. Ordinary errors contain safe fixed codes/messages. Exit status is 0 for inspection success, 2 for invalid arguments, 3 for inspection/output failure and 130 for cancellation. A successful inspection can still report conversion blockers.

## Prepare selected jobs

Create a private strict JSON selection with `version: 1`, the destination `repository_id` and `expected_revision` from `fc import state`, a stable source namespace, `workers` mapping exact old worker names to explicitly configured native IDs, selected stable `job_ids` and `checkpoint_mode` of `none`, `latest` or `all`. At most twenty-five jobs and one hundred selected checkpoints fit a batch. All other source classes stay excluded; this command does not import a full job revision timeline or choose a source scope automatically.

```sh
bin/legacyimport prepare-jobs --snapshot /private/export/controller.sqlite3 --selection /private/import/selection.json --output /private/import/jobs.json
```

Preparation shares inspection's admitted read-only handle, limits and final identity/hash checks. It accepts the same five explicit positive overrides: `--max-database-bytes`, `--max-rows-per-table`, `--max-total-rows`, `--max-cell-bytes` and `--max-total-cell-bytes`. Omitting a flag retains its safe inspection default; an explicitly supplied zero or negative bound is invalid. Raising inspection bounds does not relax the native import's content or batch limits. It preserves selected text/times and refuses absent worker mappings, missing dependencies, unsupported content and native byte limits. Source checkpoints have no author field, so their explicit `imported-history` attribution means original author unavailable. Their pointer objects remain inert original JSON, including non-string values, when they fit the native evidence limit. The output is private JSON in an existing private directory; publication never replaces a different existing file. An identical rerun preserves its bytes. Stdout reports only counts and the source digest. A failed stdout write can leave the valid prepared file available; retry with the exact selection/path or inspect that file. No service or provider is contacted, and applying the result is a separate explicit native command.

## Prepare selected messages

Use `prepare-messages` with the same absolute snapshot, selection and output flags. Its private selection contains `version: 1`, `repository_id`, current coordination import `expected_revision`, stable `source`, explicit `message_ids`, exact original-name-to-native-ID `workers` mappings and `association_policy: "archive"`. Optional `unknown_author_worker_id` explicitly maps an unavailable original author; it is never inferred. Optional `retained_parent_ids` names source parents already imported into the destination. A reply otherwise requires its parent in the selected set.

```sh
bin/legacyimport prepare-messages --snapshot /private/export/controller.sqlite3 --selection /private/import/messages-selection.json --output /private/import/messages.json
fc import preview --file /private/import/messages.json
fc import apply --file /private/import/messages.json --preview SHA256 --key selected-messages
```

The command preserves selected body, creation/handling times, reply identities and known pending/handled state, with at most one hundred messages and the native 16 KiB body limit. Unsupported source job/PR message annotations remain in the sealed private archive; the required archive policy and compact count make that omission explicit. Retained-parent traversal stops at an explicitly named source boundary; native application checks its existing destination mapping and conversation graph. Missing mappings, unsupported timing/content, cyclic or unselected dependencies refuse preparation. No historical send or acknowledgement is replayed.

## Prepare selected completed review history

Use `prepare_source_reviews.py` for selected completed, finalized review runs and their known findings. This is an optional source-specific adapter, separate from the daemon, provider collection and effective control preparation. Supply an explicitly trusted source package and a sealed private snapshot. The tool loads source helper code deliberately and reads the admitted snapshot immutably; it does not discover a live controller, read credentials or contact a provider.

```sh
python3 tools/legacyimport/prepare_source_reviews.py --source-root /private/source-controller --snapshot /private/export/controller.sqlite3 --selection /private/import/review-selection.json --output /private/import/review-batch.json --provenance /private/import/review-provenance.json
fc reviews transfer-preview --file /private/import/review-batch.json
fc reviews transfer-apply --file /private/import/review-batch.json --preview SHA256 --key selected-review-history
```

The version-one selection supplies `repository_id`, destination `expected_revision`, transfer `publisher`, stable original producer `source` and `pulls`. Each selected pull supplies the native metadata fields `number`, `title`, `state`, `head_sha`, `base_ref`, `base_sha`, `ci_state` and `mergeability`, plus explicit `run_ids`. These offline metadata and repository assertions are included in the provenance binding; preparation does not establish that they are current provider facts. Native preview checks the actual destination and matching retained PR metadata. Choose the same producer identity for successive batches of the same source history.

The output is one additive transfer batch with `complete: false` on each PR. Known finding text and source-supplied original finalized counts stay intact; when original counts are unavailable, canonical exported counts remain exact and provenance records that limitation. The converter never reconstructs missing original counts. Later effective decision/fix facts retain their original attribution and correction provenance. A bounded coverage note explains original-count versus later-correction differences; original source coverage limits remain in private provenance. Genuine start and finish times are required for selected completed runs. Unrepresentable text, counts, correction chains, timestamps or native bounds refuse rather than being truncated, guessed or dropped. Attempts, captures and receiving-route ledger history remain archived, with explicit omissions. No native coverage, review allocation or operational control is created by preparing or applying this history.

Source admission is bounded to a 32 MiB snapshot, 256 KiB selection, at most one hundred tables/columns, five thousand rows per table/twenty thousand total rows, 256 KiB per cell and 8 MiB total cell bytes. The selected source history projection fits 1 MiB, and the native batch plus its apply wrapper each fit 256 KiB. Helper SQL has a fifteen-second/two-million-step bound. One batch selects at most one hundred PRs, two hundred runs and one thousand findings, within the native per-PR/per-run limits. A larger source or selected record refuses explicitly; choose a separately prepared supported selection or retain it in the archive rather than raising bounds silently.

Output and private provenance use create-only publication. Preserve the exact inputs, output paths and provenance for repeatable recovery; differing existing files refuse. A stdout failure can leave valid prepared files available for exact retry. Native transfer preview/apply remains a separate administrator operation with its own payload, revision, fingerprint and idempotency guards. The [migration contract](../../docs/legacy-adoption.md#selected-completed-review-history) owns the retained facts and omissions.

## Acquire a source-owner history frame

`acquire_source_history.py` composes the explicitly selected trusted source controller's canonical reconciliation, history readers and obligation audit. Run it as a dedicated POSIX Python process. This optional migration tool is separate from normal agent commands and the service build. Loading source helpers executes trusted source code; select that code deliberately.

The source owner pauses writers and review requests, settles accepted findings and both incoming/outgoing routes, and supplies a completed standalone SQLite export with its original capture associations. Preserve the canonical handoff layout: `COMMON/firemud/pr-review-stack.sqlite3`, the logical `COMMON/firemud/pr-review-stack.json/` directory, and captures under `COMMON/firemud/` and optional `COMMON/coderabbit-review-logs/`. All handoff roots and selected files are private and physically contained; symlinks, special files and SQLite sidecars refuse. Do not copy a live main database and assume committed WAL contents are included. The pause is an explicit owner assertion; the tool observes locks, requests and conservation but cannot establish a durable writer fence.

The Git proof root must be an exclusively owned private clone of the exact repository, outside the handoff and source package. Shared worktrees, alternates, redirected Git metadata, multiply linked mutable metadata and repository/config environment overrides refuse. The source adapter may fetch and construct Git proof objects there. No source checkout writes are permitted. GitHub numeric repository identity and the default branch are verified explicitly.

Supply a private strict JSON plan with `version: 1`, `repository`, numeric `repository_id`, `default_base_ref`, `paused: true`, `scratch_git_writes: true`, and `prs: [{"pull_number": 7, "channels": ["cli", "hosted"]}]`. Select only the PRs and channels whose settled source state is needed. Missing, uncertain, unassociated or unsupported evidence refuses; exhausted round limits alone do not prove findings are resolved.

```sh
python3 tools/legacyimport/acquire_source_history.py --source-root /private/source-controller --common /private/handoff/common --snapshot /private/handoff/common/firemud/pr-review-stack.sqlite3 --git-root /private/proof-clone --plan /private/import/acquisition.json --output /private/import/effective-history.json --provenance /private/import/acquisition-provenance.json
```

The output and provenance must be separate files outside the source and handoff roots, in existing private directories. Canonical source audit, transfer validation, snapshot identity and bounded capture conservation finish before publication. Provenance retains the exact acquired frame before its separate output is published. After interrupted publication, the same request and unchanged selected inputs recover that original frame without new provider/Git work; `replayed: true` means recovery rather than a fresh external audit. Different existing files are never replaced. Preserve the exact source package, plan, handoff and outputs; a fresh acquisition uses a deliberate new output/provenance pair. Native preparation/application remains separate.

Bounds are 32 MiB snapshot, 16 KiB plan, 1 MiB frame, and 128 MiB/20,000 capture entries/depth 64 per fifteen-second inventory pass. Scratch metadata admission has 200,000 entries/ten seconds; provider/Git work uses the source helper's 120-second budget. Refusal prints a compact fixed diagnostic without captured provider text. Successful stdout reports only scope counts, digest, publication outcome and replay state. Conservation proves observed equality, not protection against a hostile same-user replacement. Live provider/Git qualification and real-project acquisition remain separate from synthetic proof.

## Prepare effective review controls

The separate stdlib Python adapter uses an explicitly supplied trusted source package and an owner-exported canonical effective-history frame. Source code is executable input. It never discovers an installation, constructs live provider/Git adapters, or obtains that frame itself. A source owner must acquire and seal the complete frame while operations are quiescent; a main SQLite file or ordinary status output alone is insufficient. The source owner must also establish authoritative accepted-finding resolution and incoming/outgoing routed-obligation clearance; unavailable or unknown clearance is not clear. This settled prerequisite is independent of quiescence. The completeness, quiescence and `obligations_clear` assertions are checked as input fields and recorded in private provenance, not independently established by this adapter; the tool does not acquire the audit.

```sh
python3 tools/legacyimport/prepare_source_controls.py --source-root /private/source-controller --snapshot /private/export/controller.sqlite3 --history-file /private/export/effective-history.json --selection /private/import/control-selection.json --output /private/import/control.json --provenance /private/import/control-provenance.json
fc reviews rounds adopt preview --file /private/import/control.json
fc reviews rounds adopt apply --file /private/import/control.json --preview SHA256 --key selected-control
```

The selection has `version: 1`, `repository_id`, destination `expected_revision: 0`, stable `source` and `source_key`, `pull_number` and `channel`. The history envelope has `version: 1`, the same repository identity, exact `snapshot_sha256`, `projection_complete: true`, `quiescent: true`, `obligations_clear: true`, and `prs: [{pull_number, channels: {cli: [effective rows], hosted: [effective rows]}}]`. Each selected channel must be present. These are canonical source policy rows, not native imported run rows or an arbitrary status summary. The private provenance file binds the exact snapshot, history, selection and helper-code hashes.

The adapter calls the selected source's own bounded/default-period and taper helpers, preserving remaining bounds, partial dry streak, threshold and reached latch. It refuses missing, false or nonboolean obligation clearance before preparing any numeric rule or closure result. Selected rows with a present `source_resolution_status` must say `resolved`; completed attributable accepted-finding rows that are not provisional, corrections or noncounting must explicitly carry that resolved status. Pending, unknown or unavailable resolution refuses with `controls_unsettled`. It also refuses unknown or active work, ambiguous evidence, missing source context and unsupported unbounded/oversized rules. A durable source stop with unknown counters returns `closure_only` without a prepared numeric record. Preserve that closure with the ordinary explicit native `reviews rounds close` command and retain its private source evidence; do not invent consumed counts. Closing an unconfigured channel grants no review work. The [native adoption contract](../../docs/review-round-controls.md#prepared-operational-adoption) owns application and subsequent accounting.

Snapshot, history and selection inputs are bounded to 32 MiB, 1 MiB and 16 KiB respectively. Outputs require private existing directories and never replace differing files. An interrupted two-file publication can leave provenance before the prepared file; exact retry completes publication. This optional adapter is qualified on Linux with synthetic inputs, separately from the portable Go utility.

## Verification

Run `go test -race ./tools/legacyimport/...` and `go vet ./tools/legacyimport/...`. Tests use synthetic SQLite exports and cover recognized versions and optional domains, byte boundaries, message diagnostics, unsupported note history, strict bounds, private file admission, punctuation in paths, cancellation, source conservation and absence of sidecars. `prepare-jobs` coverage also includes the `none`, `latest` and `all` checkpoint modes, explicit worker mappings and missing-mapping refusals, ambiguous or coerced source facts, private output publication, exact repeats and conflicting outputs, recovery after stdout loss, and explicit limit overrides. This is synthetic proof of selected conversion, not a real-project import rehearsal or live cutover. Run `python3 -m unittest discover -s tools/legacyimport -p 'test_*.py'` for the adapter’s standalone input/output checks. Supplying the explicit `FIRECONTROLLER_SOURCE_ROOT` environment variable also runs its synthetic policy checks against that trusted source package; the ordinary suite reports those optional checks as skipped when no source is supplied.

The [migration contract](../../docs/legacy-adoption.md) owns retention, conversion and application requirements. Native application code must not import this tool's legacy schema package.
