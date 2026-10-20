# Public Project Website

## Implementation status

The service implements administrator-controlled publication, a separate anonymous current/history API and public event stream, and an embedded public browser. Projects remain private until explicitly published. Both selected installed publications are now enabled from previewed exact map and job-summary revisions, with an optional derived worker lane. Focused SQLite, HTTP, SSE, backup and browser tests, full repository validation, packaged CLI/browser checks and actual installed anonymous/authenticated browser checks pass at the checkpoints below. This document owns the selected public/private boundary. The [browser contract](browser.md) owns the shared presentation and private application.

## Editorial presentation

Approved descriptions and ordered project-map selections render as safe Markdown narrative sections using the same prose presentation as the private [Project page](browser-experience.md#editorial-project-page). Generic sections need no phase/status/checklist fields. Empty optional editorial collections do not leave empty template cards. Publication preserves the selected public array order; private map position/content changes never republish implicitly. Raw HTML, arbitrary CSS and automatic external image/embed requests remain unavailable. Public field bounds, explicit approval, snapshot history and the allowlist below are unchanged.

## Sharing a project

One hosted service serves a public project website and the authenticated workspace at the same canonical `/projects/{handle}` address, as defined by the [shared experience](shared-project-experience.md). An instance administrator opts into publication and selects a stable public slug; projects remain private until then. The project root needs no account, API credential or browser session when published. A header Sign in action opens private content at that project address; Sign out returns to the public view. Protected detail routes retain their sign-in destination. Legacy `/public/{slug}` links and retained public history remain navigable. Anonymous `GET /api/v1/public/project-locations/{address}` resolves only an enabled published handle, existing project ID or publication slug to `{slug}`; it exposes no private IDs or unpublished project inventory.

Multi-project hosting does not make every project discoverable. Public slugs are unique across the instance, including unpublished projects, and cannot take another project's handle. Claims and replacements commit atomically; an existing or concurrent claim is rejected without changing either project’s publication. Disabling publication preserves the configured slug reservation. Only an explicit instance-administrator publication update can replace a project’s slug; replacement releases the previous slug, its legacy URL returns not found until explicitly selected by another publication, and no redirect is created. Changing the project display name does not change its configured slug or handle. Retain the slug when shared links must stay tied to the same project.

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

The private manifest optionally selects `job_refs: [{key, job_id, summary_revision}]`, `map_refs: [{key, entry_id, revision}]` and `worker_refs: [{key, worker_id, label}]`. Exact source revisions pin approved prose; current enrolled job status and assignment can update mechanically. Job references reuse [optional summaries](job-summaries.md), never private briefs. Map references reuse only the selected title, description and state, with an archived marker when appropriate; they exclude private where/next/trigger text, links and provenance. A worker lane uses its explicitly approved public label, with headline/status from the first selected job currently assigned to it and up to twenty selected job titles as milestones; no matching selected jobs means idle with empty prose. Manifest order chooses priority. Generic authored cards and lanes remain supported. Combined static/linked limits are one hundred map cards, one hundred progress cards and fifty worker lanes, with globally unique public keys. References and source revisions stay in authenticated configuration and backups, never in anonymous DTOs or history. Missing, cleared or foreign sources refuse publication rather than falling back to private fields.

An instance administrator previews and applies the revision-guarded publication manifest, including changes to its selected sections, public item references and enabled state. The instance administrator approves each selected editorial revision or supplies its explicitly authored public summary in the manifest; this flow grants no independent editorial role publication authority. Editing a private source later does not silently publish that edit. Preview shows the resulting anonymous fields and links before apply; changed approved editorial/title/link inputs require another preview. Private review-workspace revisions alone do not invalidate that preview. Exact apply retries are idempotent. Explicit unpublishing also requires instance-administrator authority.

An instance administrator explicitly enrolls a PR through the publication manifest. An enrolled PR may follow new stored lifecycle and finalized summary/count observations without manual approval of every review round. Its approved public title and repository link stay fixed until deliberately changed by an instance administrator. Enrollment never exposes raw findings, private execution diagnostics or draft editorial content. Any future automatic enrollment is a separately selected project policy, not an implicit effect of enabling the project website.

Public references resolve only inside the published projection. Do not expose private snapshot IDs, internal record IDs or links to briefs, notes, conversations and captures. Public URLs allow safe HTTP(S) project destinations and render as links rather than automatic embeds; preview must make each destination visible. Publication cannot claim that a user-approved external URL is publicly accessible or safe merely because its scheme is allowed.

Public revision, freshness timestamps, live delivery and cursors belong to the published projection. Never copy a private workspace revision, snapshot ID or recorded time: internal attempts can advance those without changing any public fact. Public live delivery has its own authorized-by-publication feed and cursor namespace. Only a change to the published projection emits public activity. A private message, draft edit or internal review attempt must not generate public events, change their metadata or delivery cadence, or introduce sequence gaps. This isolates the public feed's logical activity; it does not guarantee anonymous HTTP or stream latency isolation from shared resource contention. Replay and bounded reconnect read only retained public projections. A public refresh is a stored-data read, not a provider refresh.

Unpublishing immediately rejects subsequent anonymous data, history and event requests and terminates active public delivery. Initially use no-store responses so browser/proxy caches do not continue serving unpublished content. Content already viewed or copied cannot be recalled. Restored installations retain the inactive recovery boundary; publication is not permission to start serving restored data before activation.

## Functional proof

Use distinct private sentinel values in messages, briefs, checkpoints, note bodies, draft maps, private history, actor/session IDs, provider captures and another project. Inspect anonymous HTML, JSON, events, errors and links to prove that only the approved projection appears. Test both unpublished projects and published projects containing unpublished records.

Prove that publication and PR enrollment require an instance administrator credential. Cover instance-wide slug collision and concurrent-claim rejection, reservation after unpublish, explicit administrator replacement/release, old-link not-found behavior without redirects, preview/apply retries, changed source revisions, a later private edit, selected PR summary updates, merged history retention and honest unknown/stale labels. Public reads must leave private data and provider-call counts unchanged. An attempt that does not change the published projection must leave anonymous bytes, freshness metadata and events unchanged. Exercise unpublish during an open stream and reject replay afterward. Reuse the default layout at desktop and phone widths, with separate public/private navigation and no credential requirement for the shared route. The proof status below distinguishes executed checks from remaining validation.

## Publication API and administration

Publication requires an instance administrator credential; project `read` or `write` grants do not authorize it. For CLI use, select a profile binding that credential to the intended instance, project and worker. Read `GET /api/v1/projects/{project_id}/publication`. The initial configuration has revision zero, publication disabled and no manifest. A complete approved input contains `expected_revision`, `enabled` and `manifest`; omitted editorial sections normalize to empty collections. The manifest contains the public slug, validated project presentation, description, approved links, map/progress cards, worker lanes and explicitly enrolled PR numbers with approved titles and GitHub URLs.

`POST /publication/preview` returns the normalized manifest, current eligible summary content and `preview_sha256`. `PUT /publication` accepts the same approved input plus that hash and an explicit `Idempotency-Key`. The hash binds the normalized editorial input and selected administrator/project/worker, not a private review snapshot. Apply checks the configuration revision; exact retained retries return the original outcome even after later changes. First publication, re-enabling or changing the slug starts a new public generation. Existing private credentials never appear in public links.

`POST /publication/unpublish` accepts `expected_revision` and an explicit `Idempotency-Key`; no preview or complete manifest is needed to withdraw access. Publishing is optional and administrative. Routine agent reads and job/message mutations do not include manifests or public content.

## Agent setup commands

Use a deliberately selected profile with an instance administrator credential, separate from ordinary worker credentials:

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

Review projection is part of the authoritative review write transaction. An invalid or over-budget optional projection preserves the private review write and last good public snapshot, recording only a private `refresh_error`. Snapshot construction and notification marking share the same savepoint: a missing notification collector during offline recovery also rolls back only the optional projection. A later ordinary write can rebuild it and publish the hint after commit; no public revision advances without its owning notification boundary. A public refresh reads that retained snapshot without provider calls. Backups preserve the approved settings and public snapshots; inactive restores serve no public data. Explicit coordination activation restores access to an enabled retained publication, while disabled publications stay disabled.

## Proof status

### Installed shared-site checkpoint

An actual installed two-project browser workload passed once in 44.896 seconds with 224 same-origin GET/HEAD requests, two session logins, three renewals, two logouts and no other writes. It verified each friendly public route, anonymous omission of authentication and private fields, exact selected DTOs and source revisions, retained public history reload, inline sign-in at the same URL, authenticated deep navigation and two-project renewal/logout isolation. Six desktop and narrow screenshots were inspected without overflow. Published content used explicit map and job-summary references plus an optional derived worker lane; anonymous output contained no private briefs, messages, notes, finding text or source IDs.

The current canonical database backup and inactive restore preserve both enabled publications and all retained captures while execution remains paused. The restored copy disables every restored credential, and the transferred off-host export passed independent inspection against the pinned daemon and expected state. These checks do not establish disaster activation, physical-device behavior, fleet throughput, automatic worker handover, provider resumption or provider execution.

Focused tests exercise canonical approved content, real SQLite administration and separate anonymous reads, actual HTTP SSE, post-commit hints, privacy and unchanged freshness under private activity, preview/apply bounds and retry, unpublish, generation isolation and bounded retention. Actual review-domain integration covers observation, native capture publication and adjudication hooks, including finalized eligibility and retained merged history. Backup bundle tests create, inspect and restore the publication domain, reject rehashed malformed schema/content and exercise the inactive/activation boundary. Browser transport/component tests cover credential omission, strict public DTOs, bounded fetch, quiet streaming, cancellation and history rendering. These focused results do not establish deployment, physical-phone behavior, fleet throughput or operational cutover.

### Executed public website checkpoint

Clean source `c0052a470f198434c7414686ec2ad0b7b06a24d0` passed `make check build`: workflow validation, TypeScript and 240 browser tests, every Go package with the race detector, vet, formatting, documentation and generated assets. The backup package completed in 432.119 seconds and review execution in 475.820 seconds; these test durations are not ordinary command latency measurements. The build produced three CGO-free binaries.

The clean built inputs then passed `make smoke browser-smoke`: packaged CLI/MCP/upgrade/publication groups in 0.551 seconds, and private/public Chromium groups in 3.231 seconds. The publication workflow used an actual administrator profile and `fc` process for read, preview, apply, exact retry, unpublish and historical retry; anonymous reads needed no credential, and retrying an old successful apply did not republish withdrawn content. A separate fixture writer supplied private sentinel records and retained review summaries to the browser workload. Real gzip proxy responses, omitted credentials despite an existing private cookie, desktop and 390px layouts, channel-separated rounds, retained merged work, live summary/history refresh, unchanged public freshness after a private edit, frozen historical views and unpublish all passed. Screenshots were visually inspected, including the readable duration text on completed round pills.

The tested `fcd` SHA-256 was `bee913356376524442cc2a7e97e4f1597b4e5f38dc616caa2e3a67be364e13d9`, with clean VCS metadata for that source. Executable hashes stayed unchanged during the workloads; owned processes and disposable fixture state were cleaned up. No live provider, installed deployment, physical phone, fleet throughput or operational cutover was exercised. Later inherited scheduler/profile corrections retain their separate focused proof at the [backup owner](backups.md#terminal-scheduler-result-recovery); they do not change the source identity of this full-run checkpoint.

## Bounded review windows

CLI and Hosted selection are independent. The current projection reads at most 100 eligible finalized/completed summaries plus one overflow probe per channel for each enrolled PR. Approved summaries retained in the preceding current-generation publication are merged by opaque round key, so private omission does not withdraw visible merged history or a known history-outside-view flag. Incoming same-key facts can correct the safe summary; approved titles and links remain editorial values. Source selection starts with a derived current-run index constrained by project, PR and channel, whose partial eligibility index includes only completed, finalized runs. It selects at most 101 positions before point-joining their authoritative current references and immutable scalar versions; sparse or absent channels do not scan the other channel’s lifetime runs. Every selected row, including the overflow probe, must agree with its authoritative PR, position, channel, outcome and finalization. Disagreement refuses the optional projection rather than exposing an unfinalized run. No capture bodies or provider work participate.

Each channel displays at most 100 rounds. The whole approved content retains its 256 KiB encoded limit; if needed, the builder removes oldest displayed rounds from the largest channel windows until it fits, preserving every selected PR and approved editorial section. Known completion times and deterministic undated-key ordering remain distinct; unknown times do not acquire fabricated chronology. A channel shortened by source selection or output budgeting sets `cli_has_more` or `hosted_has_more`. These optional booleans disclose only selected eligible public summaries. They are not private attempt counts, native coverage or a completeness claim about provider history. If editorial content plus honest coverage flags cannot fit even with empty windows, the existing optional-projection refusal preserves the private write and last good public view.

The browser identifies a shortened window without presenting its first item as lifetime round one. Its history link opens the existing published-revision browser. Earlier revisions may show earlier public windows; the complete private review archive and public snapshot retention remain separate. Unpublish, generation changes and explicit enrollment withdrawal retain their existing access boundaries.

Both canonical historical decoders use the strict retained-content builder, never the current-window trimmer. False coverage flags are omitted, preserving genuine predecessor canonical JSON bytes. Current-window construction validates the bounded union before trimming and computes encoded-budget changes from individual round bytes, rather than serializing the whole project after every removal. Unchanged eligible facts must leave public bytes, revision, publication time, cursor and hints unchanged.

Focused model, adapter, service and browser checks cover overflow, channel independence, omission/correction, old canonical snapshots, encoded byte accounting, exact safe fields, private-only quiet behavior and generation isolation. Trusted archive fixtures test source selection beyond current native admission limits; they do not prove those limits are removed. Backup roundtrips and the expanded actual Chromium workload are recorded below. This source boundary is not an installed upgrade or operational cutover.

Focused normal/race checks now pass for the pure window model (race 1.678 seconds), indexed source adapter including existing actual privacy/hooks (race 8.162 seconds), and the complete public-site service group (race 3.478 seconds). Canonical backup roundtrips pass normally and under race (12.921 seconds), preserving genuine prior no-flag JSON and newer limited snapshots through inactive restore; seven resealed malformed-flag bundles are refused before destination writes. Browser type checking and 310 unit tests pass. The complete Go suite also passed without race; full CI remains separate.

Clean source `75e23ffa992b79bab9a4ae7c321e9d6b83a7641d` passed all seven actual packaged cases: CLI/MCP/upgrade/publication/map smoke in 1.038 seconds, then private/public/archive Chromium in 9.742 seconds. The public browser used ordinary bounded observations to advance three previously published Hosted rounds with 100 new ones: the current window retained 100, set its coverage flag and preserved the separate CLI lane. Its notice opened retained published history, whose first revision kept its original one-round-per-channel bytes. A private job edit preserved public content, revision, publication time, cursor and page request count. Unpublish and credential-omission assertions remained enforced. All 725 tracked files and three binaries stayed unchanged; the tested daemon SHA-256 was `d4db8b333304643530cd45762347bcd90dbe6bf664e2b3f796d9cfe039147767`.

An earlier combined browser run at `cfc594c00aeae8f24fc889bdd6735b055eabfc9b` failed in the existing private message-send phase; its expanded public and archive cases reported no failure. The wrapper had discarded the exact assertion. Narrow fixed checkpoints and numeric/boolean diagnostics were added without changing application code, assertions or deadlines. A diagnostic private run and the subsequent clean combined run passed; the original failure remains unexplained, rather than claimed fixed. These local results do not establish live providers, installed hosting, physical phones, fleet performance or cutover.

## Editorial presentation proof

The shared Markdown renderer passed an actual packaged anonymous-browser check at desktop and narrow widths. Approved narrative order, headings, emphasis, lists and safe links rendered; image syntax caused no external request, raw HTML stayed literal and unsafe links stayed inactive. Private API reads, credential headers, domain writes and private fixture content were absent. Distinct canvas/card theme colours are checked in both views; private section navigation also has a sticky-header regression in the [editorial fixture](../tools/packagedsmoke/editorial_browser_test.go). Existing publication selection and history contracts remain unchanged. This proves the local synthetic interface, not remote publication or human visual acceptance.
