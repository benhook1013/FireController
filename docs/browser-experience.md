# Delivery and Project Website

## Implementation status

The existing browser separates jobs, workers, reviews and map records into list/detail screens. The Delivery homepage, merge-head hero and editorial Project page described below replace that navigation-first presentation. Existing service isolation, live updates, private profiles, authenticated coordination and publication allowlists remain the foundation. Implementation and executed proof are recorded in [delivery](delivery.md).

## Default Delivery homepage

The project root opens Delivery: one useful view of who is doing what, followed by the review and merge queue. Worker lane cards contain their assigned open jobs, including primary focus and other concurrent jobs. An Unassigned lane contains open jobs without an owner. Cards show concise current activity, job state, blockers and the next useful step with links into full context. Worker pause is an explicit coordination fact, never inferred online presence or a job status. Active work comes first, followed by blocked and parked work; completed jobs remain available in Jobs rather than crowding the default board.

The default view needs no worker selection or filter configuration. Inbox actor selection never filters Delivery. Jobs remains a separate management view with status/worker filters, completed history and a future home for deliberate creation/editing; browser job mutations are not implied by this redesign. Worker details and histories remain secondary links from lanes.

The review/merge queue sits below the lanes, with distinct Hosted and CLI groups, actual findings, taper/limits and CI facts where available. Merged entries remain visible completed history. Remove entries only for explicit defunct/superseded work. Detailed review pages supplement the overview without making routine monitoring require several screens.

The hero right side presents the merge-train head PR with its title, meaningful stored review/CI state and a link to inspect it. Select the head from actual stored queue ordering and eligibility; an incomplete bounded slice cannot establish that no head exists. Keep Hosted and CLI progress distinct and retain freshness/unknown states. This card must not fetch GitHub or start a review. Remove decorative workflow filler. Show one connection indicator and the Refresh action in a compact sticky header so they remain usable while scrolling; keep Disconnect a compact utility. Refresh rereads stored service data and never starts provider work. Remove duplicate hero utilities and visible internal project IDs. With no head, the hero contracts rather than leaving an empty panel.

Reads stay bounded and project-scoped. Reuse stored summaries and live invalidation; never expand every job brief, checkpoint, note archive or provider capture to paint the board. Expose continuation or partial coverage honestly at larger sizes, without presenting a partial page as the full project. Desktop and narrow layouts preserve readable lanes, a clear queue and stable loading geometry.

## Delivery read contract

`GET /api/v1/projects/{project_id}/delivery` requires project read access and returns `workers` and `jobs`, each an independently bounded `items`/`cursor`/`has_more` collection from one local read transaction. `worker_limit` defaults to 20 and `job_limit` to 50, each capped at 100; `worker_cursor` and `job_cursor` bind the project, principal and ordering. Unknown, repeated or malformed selectors fail validation. Worker order is stable ID order; open jobs use active, blocked, parked then ID. Completed jobs remain outside this overview.

Workers include their identity/name, selected primary job, explicit pause state and a pause-reason preview. Jobs include compact title/state/revision/assignment, the assigned worker name and their optional latest checkpoint sequence, captured job revision, done/next/blocker previews. Each preview is at most 240 UTF-8 bytes with an explicit truncation flag; full details remain deliberate reads. No briefs, evidence arrays, histories, provider captures or implicit provider refresh are included. Checkpoint revisions remain visible so older progress cannot masquerade as an update to a newer job brief.

The browser groups this bounded job slice into lanes and offers explicit continuation. An assigned job whose worker lies outside the worker slice still carries its worker name and never becomes Unassigned. A partial job page does not establish that any lane is complete. Continuation and filters remain usable without automatically walking an entire fleet. Job creation/update/checkpoint, worker creation/pause/focus and broad resynchronization invalidate this stored summary; unrelated message arrivals do not require rereading every job.

## Editorial Project page

Project explains what the project is, its direction and progress as a coherent website. It replaces the record catalogue as the primary map presentation. Content has flexible editorial depth: a substantial Markdown section, a short update, an initiative with subsections, a milestone, or contextual deferred direction are all legitimate. Progress, dates, status labels, phases and links are optional aids, not a mandatory checklist shape.

Use readable section hierarchy, generous but purposeful typography/spacing and appropriate full-width narrative, cards or columns. Render the actual content in the page rather than requiring a click through each summary. Do not present every entry as the same boxed database row or surround prose with empty metadata placeholders. Preserve project-configurable appearance, the selected default branding and platform attribution.

Markdown is an appropriate authoring format; storage and APIs must preserve its expressive content and deliberate order. Render supported prose, headings, emphasis, lists, quotes, code and safe links as content. Raw HTML, scripts, arbitrary CSS and automatic external embeds do not acquire authority from Markdown. Existing plain editorial records and historical revisions remain readable; optional structured progress remains available. The domain contract at [project map](project-map.md) owns exact persisted shape, bounds, history and import behavior.

A return point can remain editorial context when it explains project direction. Actionable reminders belong on jobs or contextual notes, with links where helpful. Do not automatically reclassify, import, deliver or resolve those records.

## Public and private use

The Project page is suitable for a shareable showcase when an administrator selects publication. The [public-site allowlist](public-site.md) still controls exposure. Private worker activity, job briefs, inboxes, reminders, credentials and captures do not become public because the layout changes. Published editorial content uses the same polished presentation with approved fields; private Delivery remains the operational view.

## Observable acceptance

Opening a populated project shows several workers with their assigned jobs, an Unassigned lane and the review queue on one page without filter setup. The hero reflects the actual queue head and distinct review channels without provider work. Deep-linking and returning preserves useful page context. Empty, paused, blocked, partial and failed states stay honest; mobile has no overflow or distracting loading shifts.

A Project page containing a long narrative section, a short update, optional initiative progress, links and contextual return information reads like a composed website. It preserves Markdown meaning and editorial order, avoids empty template labels, and works without every entry supplying phases, dates or a progress state. Public/private tests prove that layout changes cannot expose operational data. Functional browser and actual packaged/installed checks complement source/component tests; human visual acceptance remains explicit.
