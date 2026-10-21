# Job Checklists

A checklist is an optional ordered plan attached to a job. The [job-state contract](job-state.md) owns the brief and single current progress record; a checklist does not require duplicate plan text or another progress log.

## Guarded checklist state

`GET /api/v1/projects/{project_id}/jobs/{job_id}/checklist` returns the complete current ordered checklist, its independent revision and current job revision. `PUT` requires `expected_job_revision`, `expected_revision` and `items`. Each item is `{id,text,done}` with a stable selected ID. At most 100 items and 16 KiB combined ID/text are allowed; each nonblank text is at most 1,024 bytes. An empty list clears the plan. Unknown, duplicate, malformed or foreign fields are refused.

Checklist changes do not rewrite the brief or progress. A stale guard writes nothing, and exact-key replay returns the original compact receipt after current authorization. Revision history is explicit and newest first. Default job reads include remaining items and completion counts; full completed items and history are loaded only on request.

For routine completion or reopening, `PATCH /api/v1/projects/{project_id}/jobs/{job_id}/checklist/items/{item_id}` takes `{expected_revision,done}` and an explicit mutation key. The guard selects the checklist revision, not the job revision: unrelated brief/progress edits do not invalidate it. The item must already exist. Only its boolean changes; all other item text, completion, identity and ordering stay intact. A current-revision no-op adds no history/event. A stale guard rejects even when the desired boolean already matches. The compact receipt contains job ID, checklist revision, current job revision and changed/unchanged outcome. An exact-key replay returns its original receipt, even after later edits. Checklist edits do not advance the job revision.

CLI uses `jobs checklist`, `jobs checklist-set`, `jobs checklist-item JOB_ID ITEM_ID --done true|false --expect-checklist-revision N --key KEY` and `jobs checklist-history`. MCP uses `job_checklist_read`, `job_checklist_replace`, `job_checklist_item_update` and `job_checklist_history`. Use the item operation with the checklist revision and remaining item IDs already present in an ordinary job read; use complete replacement when restructuring the plan. These bounded local operations perform no provider work. A job's next action can refer to an existing checklist item rather than repeat it.

## Progress history

Separate narrative updates and checkpoints are replaced by the [unified progress archive](job-state.md#default-reads). One job update changes only selected metadata/progress fields, with automatic history. There is no second everyday progress mechanism to keep synchronized.
