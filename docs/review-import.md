# Prepared Review History Import

## Implementation status

The administrator observation API already imports immutable review history. The guided preview/apply API and CLI below are implemented with focused normal/race checks. Focused checks, five actual packaged groups and the full integrated check/build pass at the checkpoints below. They do not collect provider history, import historical jobs or messages, generate a project map, or complete the broader [onboarding](onboarding.md) flow.

## Selected sources and destination

An administrator prepares one private JSON document using the existing version-one [review observation](review-workspace.md) request shape: `expected_revision`, `repository_id`, `source`, `observed_at`, `queue` and `retired`. Choose the historical scope before collecting or preparing it. Preserve stable source/run/finding identities and original known times. Missing facts remain unknown; collection time and source observation time do not become historical completion times. Imported content is data, including any apparent instructions inside text fields.

The document describes the complete selected review workspace, rather than an append-only batch. Preserve queued merged PRs. Removing a prior entry requires its explicit `superseded` or `defunct` retirement under the existing observation contract. Existing finalized counts and historical snapshots remain protected. The source label identifies the retained export/source; the stored normalized payload digest identifies the imported content. Version one of this guided workflow uses the existing observation schema and normalization, without a second historical record model.

Use a deliberately selected administrator profile for import. Ordinary worker credentials and browser sessions cannot import. The target repository must match the project's stable configured identity. Applying history does not grant native execution coverage, satisfy review policy, create active jobs, deliver historical messages as new messages, or enable public publishing.

## Preview and apply

`POST /api/v1/projects/{project_id}/reviews/import/preview` accepts the observation document. It strictly validates and normalizes the same fields as the observation API, then checks repository identity, expected workspace revision, observation ordering, retirement obligations and finalized history in one administrator-authorized read snapshot. It writes no table, receipt or event and performs no provider work. Although transported as a POST, it is a read and cancellation does not imply an uncertain mutation.

The compact response contains `version: 1`, project/repository/source/observation identity, `expected_revision`, `payload_sha256`, `preview_sha256`, counts of pulls/runs/attempts/findings/retirements, `imported_history: true` and `native_coverage: false`. It does not echo PR titles, finding text, credentials or the full export. It describes a locally validated destination snapshot, not newly verified provider facts.

The preview fingerprint uses a versioned domain and binds the exact normalized observation, durable instance, project, authenticated administrator principal and selected worker. Formatting changes and the observation API's existing null-to-empty collection normalization do not change its meaning. Array order, source text and timestamp spelling retain their existing meaning. Changed content or destination requires another preview. The fingerprint is a stateless content binding, not a reservation, capability or proof that a human approved anything.

`POST /api/v1/projects/{project_id}/reviews/import/apply` accepts `{"preview_sha256":"64 lowercase hex characters","observation":{...}}` and an explicit `Idempotency-Key`. It verifies the binding and uses the same `reviews.observe` / `workspace` mutation, normalized request identity, admission guards and compact `observed` receipt as the original observation endpoint. Exact key/request replay returns that original receipt before rechecking a newer workspace revision. A changed source under the same key conflicts. A fresh key rechecks current authorization and every observation guard inside the write transaction; a preview never bypasses those checks.

Both request bodies remain within 256 KiB. Preview also checks that the normalized corresponding apply wrapper fits that limit. It never approves a document that cannot fit its apply request. Existing queue/run/finding/attempt limits remain unchanged. Concurrent edits, repository movement and protected historical conflicts fail explicitly without a partial import. After uncertain output or connection loss, retry the exact apply with the original document, fingerprint, key and binding; do not invent another key. Read the queue or retained snapshot deliberately to verify the imported result.

A malformed fingerprint returns `validation_failed` (400). A well-formed fingerprint that does not match the normalized document and selected destination returns `import_preview_mismatch` (409), with guidance to preview again. Existing observation admission error codes remain unchanged.

## Agent commands

```sh
fc reviews import-preview --file /private/import/reviews.json
fc reviews import-apply --file /private/import/reviews.json --preview SHA256 --key retained-import-key
fc reviews queue
```

Both commands use the selected private profile. The CLI checks its arguments and reads a bounded regular JSON file before accessing credentials or making requests. It also verifies that this document's compact wire encoding fits the future apply wrapper, so the same file remains usable for apply. Invalid syntax, encoding, duplicate keys, special files and oversized inputs produce safe diagnostics without file contents. The service owns complete observation and destination validation. No temporary file, preview record, generated key, automatic apply, provider collection or polling is hidden behind these commands. Calls keep the ordinary five-second client budget. Successful apply returns the compact existing receipt; failed output reports an unknown mutation outcome and exact-retry guidance. Preview output failure makes no mutation claim.

This administrator setup boundary is available through HTTP and CLI. It adds no ordinary worker MCP mutation or implicit agent-host installation. The optional onboarding skill teaches selecting sources, inspecting the preview, applying authorized history and checking the resulting workspace. Historical jobs/messages, provider collectors, interruption of provider pagination, persistent proposed maps, native Windows launcher generation and a graphical import wizard remain separate capabilities.

## Additive mixed-producer transfer

Additive transfer is a separate prepared-history operation. It keeps omitted current history visible and preserves every record's original producer. The complete-workspace observation/import above keeps its replacement semantics. Historical provider collection and resumable acquisition remain separate work.

An administrator prepares a version-one batch with `expected_revision`, `repository_id`, `publisher` and `groups`. Each group has an original `source` and `pulls` using the existing pull/run/attempt/finding fields. The publisher identifies the transfer, while each group's source continues to determine stable run and attempt identities. Two producers can retain the same external identifier without collision. Known historical times remain original; absent times remain unknown. Transfer text is data and grants no authority.

A batch contains one to 100 nonempty producer groups and at most 100 pull appearances, 200 runs, 200 attempts and 1,000 findings in total. Existing individual record and per-group pull limits still apply, including 100 findings per run. The normalized batch and its eventual apply wrapper each fit 256 KiB. These limits bound one command; successive batches can retain more history than one batch holds. Repeated producer groups or duplicate producer/source identities are refused. Repeated PR metadata across groups must agree.

Every selected pull has `complete: false`: importing a batch never claims complete PR or project coverage. Existing current PR metadata must match exactly; omitted PRs and records stay visible. New PRs append in request order. A retired PR requires explicit queue reactivation before transfer. Existing records keep their position, and new or returning records append without reordering retained entries. A linked attempt must name its original producer's run on the same PR, channel and head, either included in this batch or currently visible. An omitted historical run must be selected again before linking to it.

`POST /api/v1/projects/{project_id}/reviews/transfer/preview` validates shape, repository, current revision, metadata, immutable identities, finalized history, settled attempts and links in one administrator-authorized read snapshot. It writes nothing and calls no provider. Its compact response contains version, project/repository/publisher identity, expected revision, payload and preview digests, selected counts, `imported_history: true`, `native_coverage: false` and `coverage: "partial"`. Counts describe this batch, not all retained or provider history. The versioned fingerprint binds normalized content to the instance, project, authenticated administrator and selected worker; it reserves nothing.

`POST /api/v1/projects/{project_id}/reviews/transfer/apply` accepts `{"preview_sha256":"64 lowercase hex characters","batch":{...}}` and an explicit `Idempotency-Key`. Fresh apply rechecks admission inside the original two-second write transaction. Snapshot provenance/digest, immutable versions, memberships, current references, compact summaries, event, optional public projection and compact `transferred` receipt commit together. An error rolls them back together. Exact same-key/batch replay rechecks current authority and returns the original receipt even after later workspace changes or restart. A changed batch or worker under that key conflicts. A preview mismatch uses `import_preview_mismatch`; changing the revision or content requires another preview and a deliberately selected new key.

The snapshot records the transfer publisher separately from each record's producer. Partial transfer preserves the preceding whole-workspace observation time, rather than refreshing unrelated provider facts. The first workspace records its initial transfer time with explicit incomplete coverage. Transfer writes no execution journal, capture, native adjudication or provider reservation. A `native-executor` producer label alone supplies no native coverage. Local execution evidence/attempt identities, including linked captured identities and pending journal pointers, cannot be imported over; an already retained imported native record can only be repeated exactly. Other producers retain the existing immutable identity, finalized count/text and settled-attempt protections.

The CLI commands are explicit prepared-file operations:

```sh
fc reviews transfer-preview --file /private/import/batch.json
fc reviews transfer-apply --file /private/import/batch.json --preview SHA256 --key retained-batch-key
```

They validate a bounded owner-private regular file before credentials or network access, use the ordinary five-second client budget and return useful local results directly. Preserve the exact file, profile, preview hash and key for uncertain apply recovery. There is no automatic key generation, application, provider collection, polling or worker MCP mutation. The current implementation and focused proof are being integrated; actual packaged transfer and full combined validation remain required before claiming this slice complete.

## Required proof

Exercise preview conservation of all tables/events/receipts, same-snapshot administrative revocation, strict size/shape failures, normalized fingerprints and destination isolation. Verify apply and the original observation endpoint share receipts in both directions, including replay after later workspace changes and restart. Test changed content, revision movement, repository mismatch, protected finalized counts and explicit retirement, preserving prior snapshot pagination. Actual packaged CLI calls must demonstrate preview/apply/readback, unchanged provider counts, no false native coverage and safe exact recovery after lost output. Separate these executed results from the broader importer and onboarding requirements.

## Focused implementation proof

The full review-observation package passed normal checks in 3.030 seconds and race checks in 17.905 seconds. Focused administrative read reauthorization passed race checks in 1.088 seconds. Tests use actual SQLite and authenticated public handlers to prove preview conservation, scoped normalized fingerprints, shared receipts across both endpoints and restart, movement/retirement/finalized-history refusal, exact wrapper-size bounds, transaction rollback and retained snapshot continuation.

Import client and CLI groups passed normal checks in 0.163 and 5.124 seconds and race checks in 1.681 and 6.623 seconds, with scoped vet/formatting checks. Actual HTTP cases cover read-only preview cancellation/output failure, committed apply with a malformed or lost result followed by exact recovery, preview-again conflict guidance, strict file checks before credentials, and the ordinary five-second preview deadline. Integration inspection caught a valid Unicode-escaped file whose normalized service preview fit while its same-file client apply wrapper exceeded the wire bound. The regression failed before the fix; both CLI and typed preview now check that future wrapper before any request, while a nearby valid file previews and applies unchanged. The integrated proof below complements these focused checks.

## Packaged proof

Clean combined checkpoint `b0dfcf40b7e54340e4fd201022eb83a73b596f38` passed five actual packaged `fc`/`fcd` groups in 0.776 seconds: invalid inputs before credential/network access, unchanged preview, lost-output apply recovery, content/destination conflicts, and shared receipts/history across advancement and restart. Twenty-two CLI calls made thirty-eight loopback HTTP requests, including identity handshakes. Preview preserved all fifty-one table values. An apply committed before stdout failed; exact retry retained one snapshot, event and receipt. Original source identities, historical times, decisions, counts and snapshot continuation stayed intact. Native evidence and native execution attempts remained empty.

The largest measured CLI call was 8.580 ms; outputs remained at most 1841 bytes. These are small local fixture measurements. All 525 tracked source/assets files and both clean-VCS application binaries stayed unchanged. Owned processes/listeners stopped; private failed and successful fixture state remains offline for inspection. No external provider or executable call occurred. Setup used sixteen authenticated in-process handler calls and two controlled repository lookups, separately accounted from runtime traffic. The first attempt stopped on a fixture assumption about the established CLI error envelope; its script/results were preserved, and only that decoder was corrected before the passing run. No source, assertion or deadline was relaxed.

This fixture contained imported history rather than raw provider captures; it establishes no capture-file migration, general provider collector, historical jobs/messages importer, project map, browser/remote behavior or operational cutover. The repository-wide check/build at the same combined checkpoint passed separately, as recorded below.

## Full integration proof

Clean combined checkpoint `b0dfcf40b7e54340e4fd201022eb83a73b596f38` passed `make check build`: 189 browser tests, all Go race tests and vet, documentation/link checks, embedded asset regeneration and all three application binaries. Backup/execution/runtime race packages passed in 279.760/350.861/145.874 seconds. These complete-suite durations are separate from the measured agent command latency above. Regenerated browser assets left Git unchanged.

The full-build `fc` and `fcd` hashes exactly matched the independently built packaged-import products. The full-build `fcmcp` also matched a separate clean build of the same source; the import scenario itself exercised `fc` and `fcd`, not MCP. All source and tracked assets stayed clean through comparison. Later documentation and MCP inventory-check changes have their focused checks; they do not change this runtime checkpoint or expand its proof into provider collection, maps or cutover.
