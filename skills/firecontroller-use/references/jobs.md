# Writing and maintaining jobs

Read when creating, substantially rewriting or cleaning up a job. Routine updates use the current fields and local help without reopening this guide.

## Give each fact one home

- **Title:** the outcome in a short, understandable phrase.
- **Brief:** scope, constraints and completion criteria. Use useful Markdown headings and lists; ordinary prose needs no fixed template. Refer to existing authoritative workflow for reusable operating rules, retaining task-specific exceptions and human authority explicitly.
- **Progress summary:** current position, not a cumulative log of commands, reviews or test runs.
- **Next:** the immediate useful action, or the action after a named dependency/resumption. Refer to an existing checklist step instead of copying the plan.
- **Blocker:** what actually prevents progress. Leave empty when nothing blocks it. Do not repeat the whole progress report here.
- **Evidence:** pointers to native findings, changes, tests or documents supporting the record. Keep exact technical recovery information with the work that needs it.
- **Checklist, optional:** distinct unfinished steps worth checking off. It is not another copy of the brief.
- **Attached note, optional:** a distinct unresolved reminder or source record. Resolve a reminder when handled; substantial separate work becomes another job. Do not move every technical paragraph into notes merely to make the card shorter.

A shareable job summary is optional audience-facing prose with an independent publication boundary. Ordinary job work does not require another human-facing summary or a publication-maintenance step. Delivery reads current progress for its overview; detailed instructions remain on the full job.

## Make the current position easy to understand

Prefer a complete plain-language thought over internal abbreviations and inventories. Technical names and IDs belong where necessary to act; do not remove exact recovery information or weaken constraints just to shorten text. Compact progress should explain what the detail means.

For a paused job:

- **Progress:** Work paused after local checks completed. Unpublished changes are saved.
- **Next:** When resumed, publish the prepared changes and continue outstanding review fixes.
- **Blocker:** Waiting for your instruction to resume.

The full job can retain a short, clearly labelled resume section identifying saved changes and remaining validation. Review findings and their exact decisions/fix proof remain in native review records rather than a duplicated review inventory in the brief.

## Keep current instructions current

A current job must be usable without retrieving an old job revision for missing standing instructions. Bring still-relevant scope, constraints and unresolved obligations into the current brief, checklist or attached record; use an existing authoritative document for genuinely separate maintained detail. Do not hide the current task behind “read revision X.”

Use `jobs revision JOB_ID REVISION` / `job_revision_read` only for explicit historical comparison or provenance; `jobs history` / `job_history` discovers unknown revisions. Historical evidence does not revive superseded instructions or approval gates. Replace outdated current progress: automatic history preserves prior versions. Do not turn job history into a document store or a substitute for the current working record.

## Edit only what changed

`job_update` takes a partial `progress` object with optional `summary`, `next`, `blocker` and `evidence`. CLI `--progress` writes summary; omitted fields stay unchanged. Explicit empty values clear old next/blocker/evidence. Use current revisions and retain the exact request/key if its result is unknown.

For one checklist completion/reopen, use `jobs checklist-item JOB_ID ITEM_ID --done true|false --expect-checklist-revision N --key KEY` / `job_checklist_item_update`. The checklist revision and remaining item IDs are in the job read; full replacement is for restructuring. Checklist edits do not advance the job revision.

Update at a material milestone, changed plan, blocker, handoff or completion. There is no required update after each command, test or merge, and no extra end-of-turn ritual. Complete a job when its outcome is achieved and record useful evidence and any actual unproven boundary. A worker's optional primary focus and pause remain separate from job status.
