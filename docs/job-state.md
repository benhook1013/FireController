# Job State

## Implementation status

The selected model below replaces separate everyday updates and checkpoints. Implementation, existing-data conversion and installed qualification are in progress. GitHub lifecycle synchronization is a separate unfinished capability; job writes do not compensate for it.

## One working record

A job has a title, private brief, optional assigned worker, status and one current progress record. The brief defines scope, constraints and completion criteria. Progress contains `summary`, `next`, `blocker` and `evidence`; it records current position rather than another task definition. Empty next/blocker/evidence values deliberately clear them. A checklist is optional. Attached notes retain distinct pending reminders or selected source material; substantial separate work belongs in another job. An optional shareable summary retains its separate audience and explicit-publication boundary.

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

`GET .../jobs/{job_id}/progress/history`, CLI `jobs progress-history` and MCP `job_progress_history` provide one explicit newest-first progress archive. Each item contains `id`, `project_id`, `job_id`, `job_revision`, `sequence`, `actor_id`, `worker_id`, `progress` and `created_at`. Archive selection is bounded and cursor-based; normal reads never walk it. Ordinary job revision history remains a separate audit of edited job state.

## Existing records

Jobs schema eight adds canonical current progress and its history without changing provider/review domains. Convert retained checkpoints and narrative updates once: checkpoint done/next/blocker/evidence becomes one progress state, and a newer narrative update replaces its summary/evidence while retaining the latest known next/blocker. If a newer narrative combined with earlier next/blocker fields exceeds the progress budget, retain that complete newest narrative and evidence as current progress with empty next/blocker; the preceding complete state stays in history. Never shorten source text to fit. Preserve original identities, attribution and text in retained source archives; those archives are not a second current-state authority or normal write surface. Earlier supported backups retain their own schema and must remain inspectable without mutation.

Active, blocked and parked jobs receive a deliberate content pass after migration: keep task definition in the brief, put current position/next/blocker/evidence in progress, and leave actionable reminders in notes without duplicating them. The user prioritizes useful current records over reconstructing exact old history. Historical completed jobs may retain a mechanically converted state. Existing source-specific import helpers must seed canonical progress rather than require agents to repair imported jobs manually.

## Agent use

Record a material change with one update, including only changed fields. History is automatic. Routine commands, tests and merges do not each need a progress entry. A job's next action can refer to an existing checklist item instead of repeating the plan. These defaults belong in the usage skill and do not impose an end-of-turn ritual. The service enforces coherent state, bounded output and guarded writes, not how often an agent must narrate its work.

## Required proof

Verify partial updates and explicit clearing, no-op and exact replay, stale-write refusal, atomic assignment/status/progress/history changes, rollback, project isolation, strict input and bounded output. Default reads must expose only selected current job context and pending attached reminders without archives/providers. Exercise old-record conversion and repeated startup, malformed snapshots, canonical backup/inactive restore, and actual CLI/MCP/browser use. Installed content normalization and matching-client rollout are separate from passing source tests.
