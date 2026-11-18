# Project Tracking

Status: phase 1 implemented (milestones, items, typed links, API, CLI, MCP and backups). Migration of existing map workstreams and return points, routed-finding and note targets, and browser views are later phases.

## Purpose

Project tracking records what a project intends to deliver and how far it has got, as structured data that agents can count, filter and update cheaply. It complements the [project map](project-map.md): map sections stay the editorial narrative, while milestones and items carry status. Everything is generic. Callers choose keys, labels and link targets; the service never reads repository files to populate it.

## Records

A **milestone** is a named target such as `v1`. It has a caller-chosen `key`, `title`, Markdown `description` (up to 8 KiB), `state` (`open` or `closed`, default `open`), optional `target_date` (`YYYY-MM-DD`) and `position`.

An **item** is one trackable unit. It has a caller-chosen `key`, `title`, short Markdown `description` (up to 2 KiB, so it cannot become a proof log), `status`, optional `milestone` and `parent` keys, up to 16 `labels`, optional `revisit_when` text (up to 1 KiB) and `position`.

Item `status` is exactly one of `planned`, `active`, `blocked`, `deferred`, `done` or `dropped`, default `planned`. Anything finer belongs in labels. A deferred item with `revisit_when` replaces the map's return point; a parent item replaces a workstream. There is no separate verification field: proven means `done` with a `proof` link, so status is the only state to keep current. Status never changes automatically.

A **link** is a typed edge from an item: `{type, ref, role, label}`. Types are `job`, `pr`, `route`, `note`, `item` and `url`; roles are `delivers`, `proof`, `blocks` and `related`. An item holds at most 64 links, unique by `(type, ref, role)`.

| Type | Ref | Checked on write |
| --- | --- | --- |
| `job` | Job ID | Exists in the project |
| `note` | Note ID | Exists in the project |
| `route` | Routed-finding ID | Exists in the project |
| `item` | Item key | Exists in the project and is not the item itself |
| `pr` | `NUMBER`, `#NUMBER` or `owner/name#NUMBER` | Syntax only; stored as `owner/name#NUMBER`, with bare numbers using the project repository |
| `url` | Absolute `http`/`https` URL with a host and no credentials | Syntax only |

Keys match `[A-Za-z0-9][A-Za-z0-9._:-]{0,63}` and are unique per project and record kind. Labels match `[A-Za-z0-9][A-Za-z0-9._:/+-]{0,63}`, are de-duplicated and stored sorted. A milestone or parent must exist before an item names it. Parent chains cannot form a cycle and are limited to 32 levels. Each stored record, links included, is at most 32 KiB of canonical JSON. Records are never deleted: close a milestone, or set an item to `dropped`.

## Writes

Every write is keyed by the record key, so a declaration is an upsert and repeating it converges on the same state. A write that changes nothing returns outcome `unchanged` without adding history; this includes a create-only (`expected_revision: 0`) declaration identical to the existing record, so a lost create can be retried safely. A change adds one immutable history revision, the matching link-index rows and a `tracking.*` event in the same transaction.

The revision guard is optional. `expected_revision` omitted means last-write-wins with full history; `0` means create only; `N` requires the current revision `N`. A stale guard returns `revision_conflict` (HTTP 409) with `details.current_revision`. Title and description changes through a partial update need the guard, because they are prose that a concurrent writer may be editing. Status, milestone, parent, label, position and link changes do not.

The HTTP API requires an `Idempotency-Key` header like every other mutation. The CLI and MCP generate a fresh key when none is supplied, because the writes are idempotent by record key.

## API

All paths are under `/api/v1/projects/{project_id}/tracking` and use the existing scoped read and write authority. No operation calls a provider.

| Method and path | Contract |
| --- | --- |
| `PUT /items/{key}` | `{expected_revision?, item}`. Declares the item; omitted optional fields take their defaults and omitted `links` keeps the current links. |
| `PATCH /items/{key}` | `{expected_revision?, changes}`. Any of `title`, `description`, `status`, `milestone`, `parent`, `labels`, `add_labels`, `remove_labels`, `revisit_when`, `position`; empty `milestone` or `parent` clears it. |
| `POST /items/{key}/links` | `{expected_revision?, add?, remove?}`. One set change: `remove` entries `{type, ref, role?}` drop matching links (no role drops every role), then `add` entries are inserted or have their label replaced. Missing targets are no-ops. |
| `GET /items` | Compact rows ordered by position then key. Filters combine: `milestone`, `parent`, `status` (comma list), `label`, `link=TYPE:REF`. Keyset pages, limit 25, maximum 100. |
| `GET /items/{key}` | `{item, children}`: the full item with links, plus its direct children's counts by status. |
| `GET /items/{key}/history[/{revision}]` | Newest-first revision summaries, or one captured revision. |
| `PUT /milestones/{key}`, `PATCH /milestones/{key}` | Declare, or change selected fields, under the same guard rules. |
| `GET /milestones` | Every milestone (at most 200), optional `state`, each with item counts by status. |
| `GET /milestones/{key}` | `{milestone, counts, open_items, open_items_more}`: counts by status and up to 100 non-terminal items. |
| `GET /milestones/{key}/history[/{revision}]` | As for items. |
| `POST /batch` | `{milestones?, items?}`, up to 20 milestones then 100 items, each `{key, expected_revision?, milestone}` or `{key, expected_revision?, item}`. Applied in order in one transaction; any failure applies nothing and names the failing record. |

Write receipts are `{kind, key, revision, outcome}` with outcome `created`, `updated` or `unchanged`; a batch returns `{results}` in request order. Counts are `{planned, active, blocked, deferred, done, dropped, total}` and are computed on read, never stored. Explicit JSON null is refused anywhere; omit a field instead.

## Agent commands

```sh
fc milestones create v1 --title "First release" --target-date 2027-01-31
fc milestones read v1                        # counts + open items
fc items create GR-2.1 --title "Room grid" --milestone v1 --parent GR --labels area:world
fc items set GR-2.1 --status done            # no guard needed
fc items set GR-2.1 --expect-revision 4 --title "Renamed"
fc items link GR-2.1 pr:3427 --role delivers # default role is delivers
fc items unlink GR-2.1 pr:3427
fc items list --milestone v1 --status planned,active,blocked
fc items list --link pr:3427                 # what is this PR for
fc items upsert --file /private/batch.json   # same body as POST /batch
```

`items set` also takes `--add-labels`, `--remove-labels`, `--labels`, `--clear-milestone` and `--clear-parent`. Every write accepts `--key` to reuse a specific idempotency key. MCP offers the same operations as `milestone_list`, `milestone_read`, `milestone_upsert`, `milestone_update`, `item_list`, `item_read`, `item_upsert`, `item_update`, `item_links_change`, `tracking_batch`, `tracking_history` and `tracking_revision_read`; see the [MCP inventory](mcp.md#tools-and-output).

## Storage and backups

The `project_tracking` schema domain (version 1) owns `tracking_milestones`, `tracking_milestone_history`, `tracking_items`, `tracking_item_history` and `tracking_item_links`. Current rows hold the canonical record JSON plus the indexed columns used for filtering; history rows hold every captured revision. Backup inspection checks the exact physical schema, that every record decodes canonically and agrees with its indexed columns, that history is contiguous and ends at the current record, and that the link index matches each item's links.

## Later phases

- Converting existing map workstreams and return points into items, and importing tracker rows through a prepared batch generated outside the service.
- An unassigned routed finding targeting an item, and notes attached to an item.
- Read-side PR lifecycle hints from the review queue on `pr` links, and browser views for milestone progress, the PR stack, the routed-findings queue and jobs by worker.
