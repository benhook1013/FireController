# Job State

## Implementation status

The service, CLI, MCP adapter and browser implement the selected model below, replacing separate everyday updates and checkpoints. Source checks cover conversion, guarded updates, current-context reads, backup/restore and actual packaged clients and browser behavior. Installed migration, matching-client rollout and active-job content normalization are qualified at the delivery owner. Automatic GitHub observation is service-owned; job writes do not maintain review metadata.

## One working record

A job has a title, private brief, optional assigned worker, status and one current progress record. Status is `active`, `blocked`, `parked` or `completed`; [worker pause](worker-pause.md) and [primary-job focus](worker-focus.md) are independent coordination records. The brief defines scope, constraints and completion criteria. Progress contains `summary`, `next`, `blocker` and `evidence`; it records current position rather than another task definition. Empty next/blocker/evidence values deliberately clear them. A checklist is optional. Attached notes retain distinct pending reminders or selected source material; substantial separate work belongs in another job. An optional shareable summary retains its separate audience and explicit-publication boundary.

There is one everyday update operation. `PATCH /api/v1/projects/{project_id}/jobs/{job_id}` accepts required `expected_revision` and selected `title`, `brief`, `worker_id`, `status` and `progress` fields. Progress is a partial object: omitted members stay unchanged. Explicit empty strings/arrays clear values. Unknown, duplicate, null and malformed supplied fields are refused. At least one field must be supplied. This combined PATCH has a 384 KiB encoded-body limit so maximum valid brief/progress text also fits ordinary JSON escaping; other routes retain their existing limits. One transaction applies supplied changes, rechecks same-project assignment, updates the job revision, appends automatic history and refreshes approved status/assignment projections where required. A true no-op returns `unchanged` without another revision/history/event. Exact-key replay retains the existing authorization and uncertain-outcome rules.

Progress summary and next permit up to 8 KiB each, blocker up to 4 KiB, and evidence at most ten nonblank references of 1,024 bytes each; total progress content is at most 16 KiB. Ordinary multiline UTF-8 and existing text-control rules apply. Progress may be entirely empty. Successful writes return only the job ID, current revision and outcome.

The normal CLI is `fc jobs update JOB_ID --expect-revision N --key KEY` with optional `--title`, `--brief`, `--worker`, `--status`, `--progress`, `--next`, `--blocker` and repeated `--evidence`. The progress flag sets `progress.summary`; `--evidence` replaces its reference list, and a sole `--evidence ''` clears it (an empty reference mixed with nonempty references is invalid). MCP exposes one `job_update` tool with the same partial fields and `progress` object. There is no separate everyday edit, narrative-update or checkpoint mutation to choose between.

## Default reads

`GET .../jobs/{job_id}`, CLI `jobs read` and MCP `job_read` return the complete current job identity, revision, title, brief, assignment, status and timestamps, plus:

- `progress`: the full current `{summary,next,blocker,evidence}` object, including explicit empty values.
- `checklist`: `{revision,remaining,completed,total}`. Remaining contains ordered unfinished `{id,text,done:false}` items; completed is a count. Full checklist/history remain explicit reads.
- `notes`: one ordinary compact pending-note page attached to this exact job, default five entries. It uses existing preview/cursor/has-more semantics; source bodies and further pages require explicit reads. It does not include every note for the assigned worker.

The complete brief and progress are never silently shortened. Reads use one authorized snapshot and perform no mutation or provider work. Do not attach unrelated worker inbox, focus, pause or copied PR state. The old combined job-context tool is removed from the normal agent surface; the default job read supplies the job's working context, while worker coordination retains its own deliberate tools.

Job lists remain compact: existing identity/title/status/worker/revision fields plus `progress_summary` and `blocker` previews of at most 240 UTF-8 bytes each. A shortened preview has its corresponding `progress_truncated` or `blocker_truncated` flag. No full brief, evidence, checklist, notes or history is loaded with the list.

## Compact status read

`GET /api/v1/projects/{project_id}/jobs/{job_id}/status`, CLI `jobs status JOB_ID` and MCP `job_status_read` with `job_id` provide a small current-state check. The response contains `id`, `title`, `worker_id`, `status`, `revision`, `updated_at`, `progress_summary`, `next`, `blocker`, `progress_truncated`, `next_truncated`, `blocker_truncated` and `evidence_count`. All fields are explicit, including empty previews and false flags. Progress summary is at most 1,024 UTF-8 bytes; next and blocker are at most 512 bytes each. Shortening takes a valid UTF-8 prefix without adding prose, and sets the corresponding flag. Evidence is represented only by its count, never its reference strings.

The read requires ordinary project read authorization and observes job metadata and validated current progress in one transaction. It selects no private brief, checklist, attached notes or history, performs no provider calls or mutations, and rejects unsupported query parameters. The encoded response is limited to 16 KiB, including JSON escaping. These previews are status aids, not sufficient resume context; read the complete current job before continuing its work. The full job read and stored content remain unchanged, and no schema migration is needed.

Focused source tests cover authorization and project isolation, coherent reads during atomic updates, large-job omission, UTF-8 boundaries, worst-case HTML escaping, malformed current progress and strict client response validation. CLI/MCP tests exercise the same native route without context fanout. Installed client rollout and hands-on deployment qualification remain separate.

## Explicit history reads

`GET .../jobs/{job_id}/progress/history`, CLI `jobs progress-history` and MCP `job_progress_history` provide one explicit newest-first progress archive. Each item contains `id`, `project_id`, `job_id`, `job_revision`, `sequence`, `actor_id`, `worker_id`, `progress` and `created_at`. Archive selection is bounded and cursor-based; normal reads never walk it. `GET .../jobs/{job_id}/history/{revision}`, CLI `jobs revision JOB_ID REVISION` and MCP `job_revision_read` read exactly one retained native state, including its original full brief, historical progress when present, revision, actor and recording time. The scoped indexed lookup does not page through history, load current notes/checklists or perform writes/provider work. A missing retained revision returns `not_found`; it never substitutes today’s state, another revision or unimported source history. Ordinary job revision history remains a separate ascending audit for discovering edited job states. Its pages contain whole retained entries within a 4 MiB encoded response budget, including the continuation envelope. The requested item limit is an upper bound: large snapshots can produce a shorter page with `has_more` and a cursor from its last included revision. Follow that cursor rather than assuming a short page is final. Valid legacy snapshots without progress keep their original shape; malformed or oversized stored snapshots fail safely without changing retained data.

Embedded pending-note continuation preserves exact-job selection: CLI `notes list --job JOB_ID --cursor CURSOR`, MCP `note_list` with `job_id` and that cursor. An explicit job selects all of its notes independently of creator, worker defaults or reassignment. The normal worker-note default uses a different selection. Discover worker reminders separately; they are not implicitly included in a job read.

## Existing records

Jobs schema eight adds canonical current progress and its history without changing provider/review domains. Convert retained checkpoints and narrative updates once: checkpoint done/next/blocker/evidence becomes one progress state, and a newer narrative update replaces its summary/evidence while retaining the latest known next/blocker. If a newer narrative combined with earlier next/blocker fields exceeds the progress budget, retain that complete newest narrative and evidence as current progress with empty next/blocker; the preceding complete state stays in history. Never shorten source text to fit. Preserve original identities, attribution and text in retained source archives; those archives are not a second current-state authority or normal write surface. Earlier supported backups retain their own schema and must remain inspectable without mutation.

Active, blocked and parked jobs receive a deliberate content pass after migration: keep task definition in the brief, put current position/next/blocker/evidence in progress, and leave actionable reminders in notes without duplicating them. The user prioritizes useful current records over reconstructing exact old history. Historical completed jobs may retain a mechanically converted state. Existing source-specific import helpers must seed canonical progress rather than require agents to repair imported jobs manually.

## Agent use

Record a material change with one update, including only changed fields. History is automatic. Routine commands, tests and merges do not each need a progress entry. A job's next action can refer to an existing checklist item instead of repeating the plan. Keep current progress readable in plain language and retain necessary exact recovery details in the brief or owning evidence. The current record must contain current obligations without requiring an old revision as an instruction lookup. See the [job-writing reference](../skills/firecontroller-use/references/jobs.md) for examples and field ownership. These defaults belong in the usage skill and do not impose an end-of-turn ritual. The service enforces coherent state, bounded output and guarded writes, not how often an agent must narrate its work.

## Required proof

Verify partial updates and explicit clearing, no-op and exact replay, stale-write refusal, atomic assignment/status/progress/history changes, rollback, project isolation, strict input and bounded output. Default reads must expose only selected current job context and pending attached reminders without archives/providers. Exercise old-record conversion and repeated startup, malformed snapshots, canonical backup/inactive restore, and actual CLI/MCP/browser use. Installed content normalization and matching-client rollout are separate from passing source tests.
