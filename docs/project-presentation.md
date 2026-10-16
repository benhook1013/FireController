# Project Presentation and Live Metadata

## Implementation status

Project appearance records and the separate project-metadata feed are implemented with focused model, authenticated HTTP, client/CLI/MCP, stream, browser-unit and backup/restore checks. Actual packaged CLI/MCP and Chromium checks passed at the checkpoint recorded below. Full integration validation and deployment remain pending. The metadata feed refreshes map/reference views in the new browser; this has not been installed on the retained operational website. The existing incoming-message feed, agent message wait, project identity and provider workers retain their owning contracts. The new feed does not turn metadata events into messages or provide host chat wakeups.

## Stored presentation

One project owns one independently revisioned presentation record. The configuration contains `name`, `brand_name`, `tagline`, `headings` and `theme`. `name` is the canonical project display name: a successful change updates `projects.name` in the same transaction, without changing project ID, repository identity, workers or profiles. The other fields affect the browser only; endpoint names, statuses, tool schemas and JSON fields are never renamed by presentation settings.

Name is required, trimmed nonempty text up to 200 bytes, with no Unicode control characters. New project creation enforces the same rule before repository verification. A retained project with an older invalid name is not renamed by a read: its implicit revision-0 snapshot retains that name, and strict browser/agent clients may refuse it until an administrator explicitly replaces presentation at expected revision 0 with a valid name. That replacement can repair the name without first requiring the old implicit name to validate. Brand name and tagline default to the existing `FireController` and `Project workspace`, are trimmed nonempty text up to 120 bytes, and reject controls. Headings is an optional map with exactly these supported keys: `jobs`, `workers`, `inbox`, `reviews`, `map`, `hero_eyebrow`, `hero_title`, `hero_description`. Values are trimmed nonempty single-line text up to 240 bytes. Missing keys retain the existing interface wording. Other operational labels remain service vocabulary, not a translation framework. Unknown fields/keys, malformed Unicode and scalar null fail validation; omitted maps normalize to empty objects.

Theme is an optional map of supported semantic tokens to six-digit `#RRGGBB` colors, normalized lowercase. No CSS expressions, selectors, script, HTML, URLs, fonts or asset uploads are accepted. The supported tokens and reference colors are:

| Tokens | Reference colors |
| --- | --- |
| `canvas_background`, `canvas_text`, `canvas_muted`, `canvas_border` | `#e8e7e4`, `#252833`, `#666874`, `#c8c7c4` |
| `surface_background`, `surface_text`, `surface_muted`, `surface_border` | `#f7f6f3`, `#252833`, `#666874`, `#cfccc7` |
| `accent_background`, `accent_text`, `accent_soft_background` | `#a51e28`, `#ffffff`, `#f0dfd8` |
| `header_background`, `header_text`, `header_muted` | `#a21f29`, `#ffffff`, `#f3cbc8` |
| `hero_background`, `hero_text`, `hero_muted` | `#a11c26`, `#ffffff`, `#f6c5bc` |
| `focus` | `#e68151` |
| `active_background`, `active_text` | `#f3dcd4`, `#a3382c` |
| `blocked_background`, `blocked_text` | `#ecd9dc`, `#a32234` |
| `parked_background`, `parked_text` | `#e5e0d4`, `#82704c` |
| `completed_background`, `completed_text` | `#e1e7db`, `#597048` |

The shared pure `internal/presentationmodel` package owns configuration normalization and validation without SQL or HTTP dependencies. Its embedded `defaults.json` contains the `theme` reference palette and `headings` defaults; browser validation imports that same data. Go clients do not import SQL/HTTP implementation packages. CSS uses bounded token variables with its exact existing selector colors as fallbacks. With no overrides, preserve the existing refined red dashboard byte-for-byte in color/typography/layout; any nonempty theme applies the resolved semantic palette (reference colors plus explicit overrides), so foregrounds match the reference values used by validation. Clearing the theme restores each exact stylesheet fallback. Shadows and noninformational transparency need not become settings. All informative foreground, background, border and focus colors must be assigned to an appropriate supported role rather than leaving project-specific red or dark text behind in a custom theme. Default flame/favicon and platform attribution remain; custom assets are outside this contract.

Validate each pair when either of its tokens appears in the override map, even if the supplied color equals its reference value. Text and muted tokens pair with their own canvas, surface, header or hero background. Accent text pairs with accent background; accent background also pairs with canvas and surface backgrounds because it serves links. Soft accent backgrounds pair with surface text, surface muted text and accent links. Each status text pairs with its corresponding status background. These pairs require contrast at least 4.5. Focus pairs with canvas, surface, header, hero, accent, soft accent and each status background, requiring 3 whenever focus or that background is overridden. Missing members use reference colors; calculate contrast through sRGB relative luminance. Entirely unmodified default pairs retain the established appearance; this is not an assertion that every existing small label already meets accessibility criteria. Reject an invalid override rather than quietly changing its color. Actual desktop/narrow browser proof of configured themes complements these semantic checks; this is not a claim of complete accessibility compliance.

The canonical JSON configuration is at most 16 KiB, within the existing API request ceiling. A replacement supplies `expected_revision` and `presentation`; it replaces the complete configuration, so omitted optional overrides clear to defaults. Fast local mutations return compact receipts, with explicit keys and no operation-polling cycle. Current authorization is checked on exact replay. A no-op preserves revision/history/events while recording the receipt for its new key.

## API and history

Paths are under `/api/v1/projects/{project_id}`. Reads require project read access. Replacement requires existing administrator authority and an explicit worker binding; receiving a message never grants that authority. Browser sessions remain read-only.

| Method and path | Contract |
| --- | --- |
| `GET /presentation` | One authorized read transaction returns `{presentation,event_cursor}`: current configuration/revision plus atomic project-metadata watermark. |
| `PUT /presentation` | Explicit key, nonnegative expected revision and complete replacement configuration; receipt `{project_id,revision,outcome}`. |
| `GET /presentation/history` | Compact revision/actor/time summaries, newest first, default 25/max 100, explicit cursor. |
| `GET /presentation/history/{revision}` | One complete captured configuration with its actor/time. |
| `GET /project-events/stream` | Persistent authorized metadata SSE from the explicit cursor, limit 1–100/default 25. |

The current presentation object contains `project_id`, `revision`, the five configuration fields and `updated_at`. An installation with no record returns revision 0 and empty updated_at with its current project name and default configuration; reading does not allocate rows. There is no retained revision 0. Current/history data is private. History freezes configuration and name, not the rendered pixels of future browser releases.

Configuration, immutable history, canonical project-name update, compact event and receipt commit together. Events contain only identifiers/revisions. Histories are contiguous, and current settings agree with their last snapshot and canonical project name. Inspection/restore preserve those bindings; startup installs the optional domain before serving. No provider or filesystem tracker is consulted by a presentation command.

## Separate project-metadata stream

The feed contains only `project.presentation_changed`, `map.entry_created`, `map.entry_updated` and `job.workstream_changed`. It exposes the existing durable sequence through an opaque cursor, not bodies, names, settings, messages or provider captures. A page is `{items,event_cursor,has_more}`; each item contains `event_type`, `resource_id`, `revision` and its own `event_cursor`. Ordering is ascending durable sequence; delivery is at least once. There is no acknowledgment mutation.

Cursors bind instance, principal, project and the project-metadata feed namespace. They are intentionally independent of the browser's selected worker because this is project-wide read authority. Message cursors remain independently bound to their incoming-message scope and cannot be exchanged. Registration precedes the first durable read; a snapshot watermark and later replay cannot miss a committed change. Advance across irrelevant sequence gaps without inventing an item. Reject ahead/malformed/cross-scope cursors and use the existing seven-day replay-eligibility boundary; expired replay requires a fresh presentation snapshot before resubscribing.

Use the same bounded, coalescing in-process wakeup mechanism and shared subscription limits (1024/process, 16/principal) as the existing feed, with an explicit feed namespace. Project metadata wakes project metadata subscribers only; it does not wake every agent inbox. Credential revocation wakes affected subscriptions promptly. No idle JSON or database polling, provider work, writer lock, stored cursor or destructive retention task is introduced.

SSE frames use event name `project-events`; heartbeats, safe errors, two-second write/flush bounds, frame limits, current authorization checks and daemon/request cancellation follow the established [message stream](message-events.md) transport guarantees. Reuse lifecycle primitives rather than maintaining a second divergent reconnect/timeout implementation. The incoming-message API, wire types and call-owned agent waits do not change.

## Browser application and isolation

Obtain the authorized presentation/watermark before starting project content queries. Subscribe from that watermark, invalidate canceled pre-event reads, refetch active affected queries and advance the processed cursor only after successful handling. Map events invalidate relevant map/reference queries; presentation events reread current settings/name. Mark inactive pages stale without walking them or fetching their bodies. Multiple events coalesce; no periodic JSON refresh is needed.

The metadata watch has a separate lifecycle from the existing inbox watch, so selecting a worker neither closes it nor changes its identity. Reuse the existing SSE parser, scoped request budgets, abort handling and reconnect/backoff primitives where they are independent of message vocabulary. A reconnect may replay events; expired replay obtains a fresh snapshot and invalidates affected views before resuming. Transient failures show a stale/disconnected status rather than implying current settings. Terminal authorization/identity failures clear the connection.

Apply theme/headings/branding only from the active verified project session. Masthead, document title and root CSS sit outside the workspace remount and must explicitly reset on disconnect, project switch, route mismatch, canceled connection and terminal authentication failure. Ignore delayed callbacks from the prior session. Store no appearance or credential payload in browser storage. Tabs remain independent. The document title uses the active project name and FireController; disconnected pages use FireController. Render all text literally. The small Powered by FireController link stays linked to the public repository.

## Agent use and proof

Bound CLI commands are `fc presentation read`, `history [--limit N] [--cursor CURSOR]`, `revision N`, and `replace --file /private/presentation.json --expect-revision N --key KEY`. The input file contains the complete configuration; the CLI supplies the revision wrapper. File shape, Unicode, keys, colors and bounds are checked before credentials or network calls. The default command budget remains five seconds; replacement returns a completed compact receipt, not a pending operation.

Settings reads are explicit; ordinary context, inbox, job and map reads do not append theme data. Provide bound CLI presentation read/history/revision and administrator replacement using an owner-private strict regular JSON file (Unix mode excludes group/other access; Windows permissions remain an operator responsibility), explicit revision/key and local argument validation. The bounded `presentation_read` MCP tool exposes the same explicit snapshot; privileged administrator mutations do not gain an MCP shortcut. Keep source configuration, key and binding for uncertain-write recovery.

Prove implicit defaults allocate no state, scoped read/admin write authority, strict keys/colors/text, changed-pair contrast refusal, CAS/no-op/exact replay after later edits/restart, project-name atomicity and backup integrity. Preserve existing message-stream tests while proving separate namespaces/capacity/wakeups, atomic snapshot/replay races, filtered gaps, cancellation, revocation, slow consumers and shutdown without idle scans. Actual browser tests must exercise default appearance, two configured projects, live updates without JSON polling, delayed old responses, reconnect/reload and reset of root-level styles/branding; inspect desktop and narrow screenshots and text bounds. Full integration, shipped binaries and installed cutover need separate executed evidence.

## Executed packaged proof

Clean checkpoint `e6e0683caaa51c5cfca1685ae41a83245345bedc` passed five packaged CLI/operator/MCP groups in 0.467 seconds, excluding helper compilation. The run used 28 actual CLI calls, three operator commands, two daemon starts and one actual MCP session through the official SDK, with all discovery pages followed. It checked implicit defaults without durable writes, strict local files, compact replacement receipts, history/CAS/project authority, exact-key recovery after failed executable stdout, metadata replay, restart and backup/inspection/inactive restore of both presentation tables and the canonical project name. A separate in-process CLI short-writer adapter against the packaged daemon checked partial-output uncertainty; this is not an OS-level short-write claim for the executable. The run recorded 56 HTTP requests and zero external/provider/Git calls.

An independent actual packaged-service Chromium matrix passed its first run in 26.841 seconds. Nine desktop/390-pixel screenshots were inspected, including unchanged default appearance, two configured projects, long live map titles and disconnected/authentication-reset views. Held initial-stream replay, live settings/map/job-reference updates, worker selection alongside incoming messages, delayed old responses, same-SPA project switching, reload, real daemon restart, revocation and connection cancellation passed. Root colors, masthead and title reset correctly. A 16.001-second idle interval made no additional API requests or database changes. Browser writes were limited to session creation/disconnection; seven controlled fixture mutations and one fixture restart were counted separately.

Source, supplied binaries and earlier proof artifacts stayed unchanged; owned processes/listeners and synthetic runtime state were cleaned. The tested service SHA-256 was `0d11acbb9ca1cff6ae4c047eeaed8b61b4c872776a5d14b0edc88cc3753ece03`. These results cover local packaged behavior, not physical phones, installed deployment, human GUI acceptance, complete accessibility, live provider accounts, fleet throughput or model-host wakeups. Historical full map validation failures remain recorded at the [map owner](project-map.md); focused and packaged success does not turn those failed runs into passing evidence.
