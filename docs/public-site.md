# Public Project Website

## Implementation status

The service implements administrator-controlled publication, a separate anonymous current/history API and public event stream, and an embedded public browser. Projects remain private until explicitly published. Focused SQLite, HTTP, SSE, backup and browser tests, full repository validation and actual packaged CLI/browser checks pass at the checkpoint below. The installed development site has not been changed by these source checks. This document owns the selected public/private boundary. The [browser contract](browser.md) owns the shared presentation and private application.

## Sharing a project

One hosted service serves a public project website and the authenticated workspace. A project administrator opts into publication and selects a stable public slug; projects remain private until then. The shareable route needs no account, API credential or browser session. A visible sign-in link leads to the private workspace without exposing credentials or bypassing authorization. Multi-project hosting does not make every project discoverable. Public slugs are unique across the instance, including unpublished projects. Disabling publication preserves the configured slug reservation. Only an explicit administrator publication update can replace a project’s slug; replacement releases the previous slug, its URL returns not found until explicitly selected by another publication, and no redirect is created. Retain the slug when shared links must stay tied to the same project.

Retain the default fire theme, compact channel-separated review progression, responsive layouts and Powered by FireController attribution. Public navigation includes project overview, selected map/progress, and selected review queue/history. Merged PRs remain visible as completed history. Publication is application state; viewing or refreshing the public site never calls GitHub or launches a review.

## Field boundary

| Area | Public when explicitly selected | Authenticated workspace only |
| --- | --- | --- |
| Project identity | Approved display name, tagline, description, headings, theme and external project links | Instance/project connection identifiers, private repository bindings, access and provider configuration |
| Project map | Published workstream/return-point titles, descriptions, phases, status and approved links | Draft entries, unpublished revisions, internal triggers, import/source provenance and private links |
| Progress and worker lanes | Approved public lane labels, assignment headlines, broad status and milestones | Worker/principal/session IDs, pause reasons, focus context, inbox counts, job briefs, full updates, checkpoints and private checklist/evidence content |
| Review queue | Selected PR number, approved title, lifecycle, public GitHub link and stored observation time | Private repository destinations, executor settings, account quota, reservations and recovery diagnostics |
| Review progression | Hosted and CLI rounds grouped separately, observed times, known duration, finalized found/accepted/routed counts and honest completeness labels | Raw captures, finding bodies, adjudication reasons/actors, routed fix details and private artifacts |
| History | Explicitly published map/progress revisions and selected review-summary history | Prior private edits, drafts and author attribution |
| Controls | Public navigation, stored-data refresh/reconnect and sign-in | Messages, seen/acknowledge, edits/settings, review requests, provider refresh, import, backup and access management |

A familiar field name does not make its contents safe. PR titles, descriptions, worker labels and URLs may contain private information. The public DTO contains only approved public values; it never serializes the authenticated response and removes a few fields afterward. A shared project-read credential is not public access: it also grants access to private coordination.

## Publication and updates

Publication uses a revision-guarded manifest with selected sections and public item references. Editorial content publishes an approved revision or explicitly authored public summary. Editing a private source later does not silently publish that edit. Preview shows the resulting anonymous fields and links before apply; changed approved editorial/title/link inputs require another preview. Private review-workspace revisions alone do not invalidate that preview. Exact apply retries are idempotent.

An explicitly enrolled PR may follow new stored lifecycle and finalized summary/count observations without manual approval of every review round. Its approved public title and repository link stay fixed until deliberately changed. Enrollment never exposes raw findings, private execution diagnostics or draft editorial content. Enrollment of new PRs is explicit; any future automatic enrollment is a separately selected project policy, not an implicit effect of enabling the project website.

Public references resolve only inside the published projection. Do not expose private snapshot IDs, internal record IDs or links to briefs, notes, conversations and captures. Public URLs allow safe HTTP(S) project destinations and render as links rather than automatic embeds; preview must make each destination visible. Publication cannot claim that a user-approved external URL is publicly accessible or safe merely because its scheme is allowed.

Public revision, freshness timestamps, live delivery and cursors belong to the published projection. Never copy a private workspace revision, snapshot ID or recorded time: internal attempts can advance those without changing any public fact. Public live delivery has its own authorized-by-publication feed and cursor namespace. Only a change to the published projection emits public activity. A private message, draft edit or internal review attempt must not expose its existence through public event IDs, timing or sequence gaps. Replay and bounded reconnect read only retained public projections. A public refresh is a stored-data read, not a provider refresh.

Unpublishing immediately rejects subsequent anonymous data, history and event requests and terminates active public delivery. Initially use no-store responses so browser/proxy caches do not continue serving unpublished content. Content already viewed or copied cannot be recalled. Restored installations retain the inactive recovery boundary; publication is not permission to start serving restored data before activation.

## Functional proof

Use distinct private sentinel values in messages, briefs, checkpoints, note bodies, draft maps, private history, actor/session IDs, provider captures and another project. Inspect anonymous HTML, JSON, events, errors and links to prove that only the approved projection appears. Test both unpublished projects and published projects containing unpublished records.

Prove preview/apply retries, changed source revisions, a later private edit, selected PR summary updates, merged history retention and honest unknown/stale labels. Public reads must leave private data and provider-call counts unchanged. A private attempt advancing the review workspace must leave anonymous bytes, freshness metadata and events unchanged. Exercise unpublish during an open stream and reject replay afterward. Reuse the default layout at desktop and phone widths, with separate public/private navigation and no credential requirement for the shared route. The proof status below distinguishes executed checks from remaining validation.

## Publication API and administration

An administrator bound to an explicit project and worker reads `GET /api/v1/projects/{project_id}/publication`. The initial configuration has revision zero, publication disabled and no manifest. A complete approved input contains `expected_revision`, `enabled` and `manifest`; omitted editorial sections normalize to empty collections. The manifest contains the public slug, validated project presentation, description, approved links, map/progress cards, worker lanes and explicitly enrolled PR numbers with approved titles and GitHub URLs.

`POST /publication/preview` returns the normalized manifest, current eligible summary content and `preview_sha256`. `PUT /publication` accepts the same approved input plus that hash and an explicit `Idempotency-Key`. The hash binds the normalized editorial input and selected administrator/project/worker, not a private review snapshot. Apply checks the configuration revision; exact retained retries return the original outcome even after later changes. First publication, re-enabling or changing the slug starts a new public generation. Existing private credentials never appear in public links.

`POST /publication/unpublish` accepts `expected_revision` and an explicit `Idempotency-Key`; no preview or complete manifest is needed to withdraw access. Publishing is optional and administrative. Routine agent reads and job/message mutations do not include manifests or public content.

## Agent setup commands

Use a deliberately selected administrator profile, separate from ordinary worker credentials:

```sh
fc --profile project-admin publication read
fc --profile project-admin publication preview --file /private/publication.json
fc --profile project-admin publication apply --file /private/publication.json --preview-sha256 SHA256_FROM_PREVIEW --key publish-website-1
fc --profile project-admin publication unpublish --expect-revision 1 --key withdraw-website-1
```

The JSON file is the complete approved input, including the observed configuration revision. Select an absolute owner-private regular file of at most 256 KiB; symlinks and unsupported file kinds are refused before loading credentials or contacting the service. Preview also reserves the eventual apply-hash wrapper space. The CLI uses the ordinary five-second service budget and returns compact outcomes for writes. Keep the exact selected file, preview hash, key and binding for uncertain-outcome recovery. Preview content is an explicit full read; routine commands never append it.

A minimal file can select the default fire presentation with a deliberate public name, for example:

```json
{
  "expected_revision": 0,
  "enabled": true,
  "manifest": {
    "slug": "my-project",
    "presentation": { "name": "My project" },
    "description": "Public development progress"
  }
}
```

Preview shows normalized empty sections and the default presentation. Add only text and destinations intended for anonymous readers. Publishing a private source's title or content requires that explicit selection; importing it privately does not imply permission to publish it.

## Anonymous transport and retention

The browser routes are `/public/{slug}` and `/public/{slug}/history/{revision}?generation={generation}`. The private workspace remains at `/`. Public transport omits credentials and never mounts the private session or query cache.

Anonymous stored reads use `/api/v1/public/projects/{slug}`, `/history` and `/history/{revision}?generation={generation}`. Current responses carry a public generation/revision, publication time, cursor and approved content. History pages contain only public revision/time summaries. An unknown or disabled publication returns an opaque not-found response. Retained history is bounded to the current generation's last 1,000 snapshots and seven days; the current snapshot remains available. Merged PRs remain in the selected queue. Each current channel shows a bounded summary window; older rounds remain in the private archive, and retained published revisions can show earlier public windows. The seven-day/1,000-snapshot retention is not a lossless lifetime public round archive.

`/events?cursor={cursor}` supplies only public revision/time/cursor hints after committed changes. Attachment subscribes before replay; duplicate hints and keepalive comments cause no activity or JSON reads. Expired retention requires a new current snapshot. Bounded reconnect does not poll JSON on a timer. A connection closed by unpublication cannot use its old generation to read history after re-enabling.

Review projection is part of the authoritative review write transaction. An invalid or over-budget optional projection preserves the private review write and last good public snapshot, recording only a private `refresh_error`. A public refresh reads that retained snapshot without provider calls. Backups preserve the approved settings and public snapshots; inactive restores serve no public data. Explicit coordination activation restores access to an enabled retained publication, while disabled publications stay disabled.

## Proof status

Focused tests exercise canonical approved content, real SQLite administration and separate anonymous reads, actual HTTP SSE, post-commit hints, privacy and unchanged freshness under private activity, preview/apply bounds and retry, unpublish, generation isolation and bounded retention. Actual review-domain integration covers observation, native capture publication and adjudication hooks, including finalized eligibility and retained merged history. Backup bundle tests create, inspect and restore the publication domain, reject rehashed malformed schema/content and exercise the inactive/activation boundary. Browser transport/component tests cover credential omission, strict public DTOs, bounded fetch, quiet streaming, cancellation and history rendering. These focused results do not establish deployment, physical-phone behavior, fleet throughput or operational cutover.

### Executed public website checkpoint

Clean source `c0052a470f198434c7414686ec2ad0b7b06a24d0` passed `make check build`: workflow validation, TypeScript and 240 browser tests, every Go package with the race detector, vet, formatting, documentation and generated assets. The backup package completed in 432.119 seconds and review execution in 475.820 seconds; these test durations are not ordinary command latency measurements. The build produced three CGO-free binaries.

The clean built inputs then passed `make smoke browser-smoke`: packaged CLI/MCP/upgrade/publication groups in 0.551 seconds, and private/public Chromium groups in 3.231 seconds. The publication workflow used an actual administrator profile and `fc` process for read, preview, apply, exact retry, unpublish and historical retry; anonymous reads needed no credential, and retrying an old successful apply did not republish withdrawn content. A separate fixture writer supplied private sentinel records and retained review summaries to the browser workload. Real gzip proxy responses, omitted credentials despite an existing private cookie, desktop and 390px layouts, channel-separated rounds, retained merged work, live summary/history refresh, unchanged public freshness after a private edit, frozen historical views and unpublish all passed. Screenshots were visually inspected, including the readable duration text on completed round pills.

The tested `fcd` SHA-256 was `bee913356376524442cc2a7e97e4f1597b4e5f38dc616caa2e3a67be364e13d9`, with clean VCS metadata for that source. Executable hashes stayed unchanged during the workloads; owned processes and disposable fixture state were cleaned up. No live provider, installed deployment, physical phone, fleet throughput or operational cutover was exercised. Later inherited scheduler/profile corrections retain their separate focused proof at the [backup owner](backups.md#terminal-scheduler-result-recovery); they do not change the source identity of this full-run checkpoint.

## Bounded review windows

CLI and Hosted selection are independent. The current projection reads at most 100 eligible finalized/completed summaries plus one overflow probe per channel for each enrolled PR. Approved summaries retained in the preceding current-generation publication are merged by opaque round key, so private omission does not withdraw visible merged history or a known history-outside-view flag. Incoming same-key facts can correct the safe summary; approved titles and links remain editorial values. Source selection reads current indexed references and scalar versions, without capture bodies or provider work.

Each channel displays at most 100 rounds. The whole approved content retains its 256 KiB encoded limit; if needed, the builder removes oldest displayed rounds from the largest channel windows until it fits, preserving every selected PR and approved editorial section. Known completion times and deterministic undated-key ordering remain distinct; unknown times do not acquire fabricated chronology. A channel shortened by source selection or output budgeting sets `cli_has_more` or `hosted_has_more`. These optional booleans disclose only selected eligible public summaries. They are not private attempt counts, native coverage or a completeness claim about provider history. If editorial content plus honest coverage flags cannot fit even with empty windows, the existing optional-projection refusal preserves the private write and last good public view.

The browser identifies a shortened window without presenting its first item as lifetime round one. Its history link opens the existing published-revision browser. Earlier revisions may show earlier public windows; the complete private review archive and public snapshot retention remain separate. Unpublish, generation changes and explicit enrollment withdrawal retain their existing access boundaries.

Both canonical historical decoders use the strict retained-content builder, never the current-window trimmer. False coverage flags are omitted, preserving genuine predecessor canonical JSON bytes. Current-window construction validates the bounded union before trimming and computes encoded-budget changes from individual round bytes, rather than serializing the whole project after every removal. Unchanged eligible facts must leave public bytes, revision, publication time, cursor and hints unchanged.

Focused model, adapter, service and browser checks cover overflow, channel independence, omission/correction, old canonical snapshots, encoded byte accounting, exact safe fields, private-only quiet behavior and generation isolation. Trusted archive fixtures test source selection beyond current native admission limits; they do not prove those limits are removed. Backup roundtrips and the expanded actual Chromium workload are recorded below. This source boundary is not an installed upgrade or operational cutover.

Focused normal/race checks now pass for the pure window model (race 1.678 seconds), indexed source adapter including existing actual privacy/hooks (race 8.162 seconds), and the complete public-site service group (race 3.478 seconds). Canonical backup roundtrips pass normally and under race (12.921 seconds), preserving genuine prior no-flag JSON and newer limited snapshots through inactive restore; seven resealed malformed-flag bundles are refused before destination writes. Browser type checking and 310 unit tests pass. The complete Go suite also passed without race; full CI remains separate.

Clean source `75e23ffa992b79bab9a4ae7c321e9d6b83a7641d` passed all seven actual packaged cases: CLI/MCP/upgrade/publication/map smoke in 1.038 seconds, then private/public/archive Chromium in 9.742 seconds. The public browser used ordinary bounded observations to advance three previously published Hosted rounds with 100 new ones: the current window retained 100, set its coverage flag and preserved the separate CLI lane. Its notice opened retained published history, whose first revision kept its original one-round-per-channel bytes. A private job edit preserved public content, revision, publication time, cursor and page request count. Unpublish and credential-omission assertions remained enforced. All 725 tracked files and three binaries stayed unchanged; the tested daemon SHA-256 was `d4db8b333304643530cd45762347bcd90dbe6bf664e2b3f796d9cfe039147767`.

An earlier combined browser run at `cfc594c00aeae8f24fc889bdd6735b055eabfc9b` failed in the existing private message-send phase; its expanded public and archive cases reported no failure. The wrapper had discarded the exact assertion. Narrow fixed checkpoints and numeric/boolean diagnostics were added without changing application code, assertions or deadlines. A diagnostic private run and the subsequent clean combined run passed; the original failure remains unexplained, rather than claimed fixed. These local results do not establish live providers, installed hosting, physical phones, fleet performance or cutover.
