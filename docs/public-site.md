# Public Project Website

## Implementation status

The current browser requires authentication for every project-data view. Anonymous application assets and health endpoints do not provide a project showcase. Public website implementation remains required product work; it is separate from posting review checkpoints to GitHub. This document owns the selected public/private boundary. The [browser contract](browser.md) owns the shared presentation and private application.

## Sharing a project

One hosted service serves a public project website and the authenticated workspace. An instance administrator opts into publication and selects a stable public slug; projects remain private until then. The shareable route needs no account, API credential or browser session. A visible sign-in link leads to the private workspace without exposing credentials or bypassing authorization. Multi-project hosting does not make every project discoverable. Public slugs are unique across the instance, including unpublished projects. Claims and replacements commit atomically; an existing or concurrent claim is rejected without changing either project’s publication. Disabling publication preserves the configured slug reservation. Only an explicit instance-administrator publication update can replace a project’s slug; replacement releases the previous slug, its URL returns not found until explicitly selected by another publication, and no redirect is created. Changing the project display name does not change its configured slug. Retain the slug when shared links must stay tied to the same project.

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

An instance administrator previews and applies the revision-guarded publication manifest, including changes to its selected sections, public item references and enabled state. Editorial content publishes an approved revision or explicitly authored public summary. Editing a private source later does not silently publish that edit. Preview shows the resulting anonymous fields and links before apply; changed approved editorial/title/link inputs require another preview. Private review-workspace revisions alone do not invalidate that preview. Exact apply retries are idempotent. Explicit unpublishing also requires instance-administrator authority.

An instance administrator explicitly enrolls a PR through the publication manifest. An enrolled PR may follow new stored lifecycle and finalized summary/count observations without manual approval of every review round. Its approved public title and repository link stay fixed until deliberately changed by an instance administrator. Enrollment never exposes raw findings, private execution diagnostics or draft editorial content. Any future automatic enrollment is a separately selected project policy, not an implicit effect of enabling the project website.

Public references resolve only inside the published projection. Do not expose private snapshot IDs, internal record IDs or links to briefs, notes, conversations and captures. Public URLs allow safe HTTP(S) project destinations and render as links rather than automatic embeds; preview must make each destination visible. Publication cannot claim that a user-approved external URL is publicly accessible or safe merely because its scheme is allowed.

Public revision, freshness timestamps, live delivery and cursors belong to the published projection. Never copy a private workspace revision, snapshot ID or recorded time: internal attempts can advance those without changing any public fact. Public live delivery has its own authorized-by-publication feed and cursor namespace. Only a change to the published projection emits public activity. A private message, draft edit or internal review attempt must not generate public events, change their metadata or delivery cadence, or introduce sequence gaps. This isolates the public feed's logical activity; it does not guarantee anonymous HTTP or stream latency isolation from shared resource contention. Replay and bounded reconnect read only retained public projections. A public refresh is a stored-data read, not a provider refresh.

Unpublishing immediately rejects subsequent anonymous data, history and event requests and terminates active public delivery. Initially use no-store responses so browser/proxy caches do not continue serving unpublished content. Content already viewed or copied cannot be recalled. Restored installations retain the inactive recovery boundary; publication is not permission to start serving restored data before activation.

## Functional proof

Use distinct private sentinel values in messages, briefs, checkpoints, note bodies, draft maps, private history, actor/session IDs, provider captures and another project. Inspect anonymous HTML, JSON, events, errors and links to prove that only the approved projection appears. Test both unpublished projects and published projects containing unpublished records.

Prove instance-administrator authority, instance-wide slug collision and concurrent-claim rejection, reservation after unpublish, explicit replacement/release, old-link not-found behavior without redirects, preview/apply retries, changed source revisions, a later private edit, selected PR summary updates, merged history retention and honest unknown/stale labels. Public reads must leave private data and provider-call counts unchanged. An attempt that does not change the published projection must leave anonymous bytes, freshness metadata and events unchanged. Exercise unpublish during an open stream and reject replay afterward. Reuse the default layout at desktop and phone widths, with separate public/private navigation and no credential requirement for the shared route. These are required checks, not claims that the unimplemented website has passed.
