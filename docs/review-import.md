# Prepared Review History Import

## Implementation status

The administrator observation API already imports immutable review history. The guided preview/apply API and CLI below are implemented with focused normal/race checks. Five actual packaged groups pass; full integrated validation remains pending. They do not collect provider history, import historical jobs or messages, generate a project map, or complete the broader [onboarding](onboarding.md) flow.

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

## Required proof

Exercise preview conservation of all tables/events/receipts, same-snapshot administrative revocation, strict size/shape failures, normalized fingerprints and destination isolation. Verify apply and the original observation endpoint share receipts in both directions, including replay after later workspace changes and restart. Test changed content, revision movement, repository mismatch, protected finalized counts and explicit retirement, preserving prior snapshot pagination. Actual packaged CLI calls must demonstrate preview/apply/readback, unchanged provider counts, no false native coverage and safe exact recovery after lost output. Separate these executed results from the broader importer and onboarding requirements.

## Focused implementation proof

The full review-observation package passed normal checks in 3.030 seconds and race checks in 17.905 seconds. Focused administrative read reauthorization passed race checks in 1.088 seconds. Tests use actual SQLite and authenticated public handlers to prove preview conservation, scoped normalized fingerprints, shared receipts across both endpoints and restart, movement/retirement/finalized-history refusal, exact wrapper-size bounds, transaction rollback and retained snapshot continuation.

Import client and CLI groups passed normal checks in 0.163 and 5.124 seconds and race checks in 1.681 and 6.623 seconds, with scoped vet/formatting checks. Actual HTTP cases cover read-only preview cancellation/output failure, committed apply with a malformed or lost result followed by exact recovery, preview-again conflict guidance, strict file checks before credentials, and the ordinary five-second preview deadline. Integration inspection caught a valid Unicode-escaped file whose normalized service preview fit while its same-file client apply wrapper exceeded the wire bound. The regression failed before the fix; both CLI and typed preview now check that future wrapper before any request, while a nearby valid file previews and applies unchanged. The separate full integrated build remains pending.

## Packaged proof

Clean combined checkpoint `b0dfcf40b7e54340e4fd201022eb83a73b596f38` passed five actual packaged `fc`/`fcd` groups in 0.776 seconds: invalid inputs before credential/network access, unchanged preview, lost-output apply recovery, content/destination conflicts, and shared receipts/history across advancement and restart. Twenty-two CLI calls made thirty-eight loopback HTTP requests, including identity handshakes. Preview preserved all fifty-one table values. An apply committed before stdout failed; exact retry retained one snapshot, event and receipt. Original source identities, historical times, decisions, counts and snapshot continuation stayed intact. Native evidence and native execution attempts remained empty.

The largest measured CLI call was 8.580 ms; outputs remained at most 1841 bytes. These are small local fixture measurements. All 525 tracked source/assets files and both clean-VCS application binaries stayed unchanged. Owned processes/listeners stopped; private failed and successful fixture state remains offline for inspection. No external provider or executable call occurred. Setup used sixteen authenticated in-process handler calls and two controlled repository lookups, separately accounted from runtime traffic. The first attempt stopped on a fixture assumption about the established CLI error envelope; its script/results were preserved, and only that decoder was corrected before the passing run. No source, assertion or deadline was relaxed.

This fixture contained imported history rather than raw provider captures; it establishes no capture-file migration, general provider collector, historical jobs/messages importer, project map, browser/remote behavior or operational cutover. The repository-wide check/build at the same combined checkpoint is a separate active validation boundary.
