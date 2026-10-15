# Live Browser and Project Presentation

## Implementation status

The service embeds a read-only browser for project connection, workers, basic jobs/full briefs and incoming conversations with live message updates. The [browser guide](browser-guide.md) records its implemented boundary, build and proof limits. The [native review workspace](review-workspace.md) provides stored review queue/history/findings. Project settings, maps and browser mutations remain later slices. This contract replaces the legacy private refresh/render/publish cycle after a separately reviewed migration.

## Live application

The service serves embedded application assets. The browser reads the same project-scoped API as agents and subscribes to authorized changes. Job, conversation, map, and PR detail routes render from service records rather than generated per-PR HTML files. Opening or refreshing a page does not silently call GitHub or sync provider captures.

Initial views include project selection, worker lanes and history, job detail with current brief and checkpoint, inbox conversations and individual handling actions, project maps, review queue and PR evidence details, and project settings. Loading, disconnected, stale-observation, provider-error, permission, and empty states are explicit. Server-confirmed mutations update the view; an optimistic display cannot claim successful persistence before the receipt.

## Project presentation

Retain the refined existing dashboard's look and feel as the default wherever the new browser has equivalent structure: red fire theme, typography, hero, compact review pills, worker lanes, and information density. Rebuilding the service is not a reason to redesign those working views. Adapt the presentation where new structure requires it, and verify the result visually at desktop and narrow widths. The defaults remain customizable project settings rather than fixed project identity.

Review summaries and PR history keep Hosted and CLI progression in separate channel groups, with all retained rounds ordered chronologically. Compact pills show found/accepted/routed counts and age, with explicit detail links. Manual and subagent evidence has separate groups. This preserves the ability to see a declining finding trend without mixing independent review cycles. A historical trend does not establish taper or readiness: those labels require the actual configured policy and applicable evidence. Presentation changes must preserve these useful interactions or explain the concrete behavior that requires a different view.

Each project owns its display name, page and section headings, theme colors, and branding configuration. Headings and colors are data-driven settings, not constants tied to an originating project. A profile's project selection does not modify another project's appearance.

Store a versioned, revision-guarded presentation record with semantic theme tokens such as background, surface, text, accent, and status colors. Validate supported values and length bounds; do not accept arbitrary CSS, JavaScript, or HTML. Render names and headings as text. Preserve visible focus, readable contrast, and non-color status labels when customizing colors.

Appearance changes emit project-scoped events. Existing views update without a static rebuild; a fallback theme keeps the app usable if optional presentation fields are absent. Asset upload and logos need bounded sizes, content-type validation, and a separate authorization contract before being implemented.

The browser includes a small “Powered by FireController” attribution linked to the public repository so users can discover and adopt the tool. Project branding remains the primary presentation. Platform attribution does not appear in agent JSON responses or create repetitive tool-output notices.

## Legacy disk inputs

The inspected current private website documents `status.json` as runtime configuration and `progress.json` as editorial structure, while mutable jobs and map state are already stored in controller SQLite. However, the retained map importer also reads legacy lanes, tasks, next steps, blockers, and brief provenance from a status input. Do not classify all historical status files as configuration-only. The site generates HTML, reads external tracker Markdown, and can synchronize completed hosted-review captures during refresh. This establishes a classification starting point, not a complete inventory of every private file.

Move application-owned map names, phases, descriptions, headings, links, and appearance into revisioned service records. Keep deployment selectors and credentials as protected operator configuration. Treat generated pages as disposable outputs. External tracker integration remains an adapter; a private filesystem path is not portable public project metadata.

A migration inventory must enumerate actual JSON readers and writers, retained data, selected runtime files, and authority before cutover. Preview import is read-only; applying an import is explicit and idempotent, records source provenance, and never overwrites a newer application revision silently. Do not copy private JSON or site source into the public repository without an explicit content and rights decision.

## Privacy and public status

Authenticated project access is the default. Briefs, detailed updates, checkpoints, notes, messages, credentials, session identifiers, and provider captures are private. Public projection is a separate field allowlist with project opt-in, not a JSON dump or an authenticated response with links removed.

Optional public live views and static exports consume only the public projection and separate public events. Private routes, identifiers that reveal private connections, note bodies, and detailed review captures are excluded. Safe Markdown rendering escapes raw HTML and rejects unsafe URL schemes. External images and artifact links require explicit handling rather than unrestricted embedding.
