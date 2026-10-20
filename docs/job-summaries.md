# Job Summaries

## Delivery status

The API, typed client, CLI, MCP and private browser implement an optional summary attached to a job. Focused normal/race checks cover guarded writes, history, scope, retry and output bounds. Combined installation and public/private browser qualification remain separate at the [delivery tracker](delivery.md). Older installed clients may omit this feature; use matching-release documentation.

## Purpose and authority

A summary is deliberately authored audience-facing prose alongside the private working brief. It remains private until an administrator selects its exact retained revision in a [publication manifest](public-site.md). It is optional; a missing summary never becomes an anonymous excerpt of the private brief. Ordinary job edits do not update it. Clear it explicitly with both title and body empty. Clearing the current summary does not withdraw a separately published older revision; publication withdrawal belongs to the administrator controls.

## API

`GET /api/v1/projects/{project_id}/jobs/{job_id}/summary` returns `job_id`, the current `job_revision`, independent summary `revision`, `title` and `body`. A missing summary has revision zero and empty prose. The title is at most 240 UTF-8 bytes without control characters; the body is at most 2,000 UTF-8 bytes with ordinary multiline Markdown. Both are nonblank, or both empty for an explicit clear.

`PUT` at the same route requires project write access, `expected_job_revision`, `expected_revision`, `title`, `body` and an explicit `Idempotency-Key`. Current job and summary revisions are independent guards. Replacement appends immutable summary history and a compact project event, without rewriting the job brief or job history. Exact-key recovery retains the original result after later writes; another request under that key conflicts. Successful writes return the compact job ID, summary revision, current job revision and outcome.

`GET .../summary/history` provides an explicit cursor page; `GET .../summary/history/{revision}` reads one exact retained revision with its original job binding and attribution. History does not silently expand routine job context. Private delivery cards include a current nonempty summary when present and retain the detailed brief behind the job link.

## Agent commands

Use the worker's bound launcher in place of `fc`:

```sh
fc jobs summary JOB_ID
fc jobs summary-set JOB_ID --expect-revision JOB_REVISION --expect-summary-revision SUMMARY_REVISION --title 'Shareable title' --body-file /private/summary.md --key RETAINED_KEY
```

`--body` is the alternative to `--body-file`; supply exactly one. MCP exposes `job_summary_read` and `job_summary_set` with the same guards. These calls neither publish content nor perform provider work. Keep the exact request and key for uncertain recovery, as with other guarded mutations.

## Publication and derived facts

An authenticated manifest's `job_refs` select an exact summary revision and an explicitly chosen public key. Public output contains that selected prose and the enrolled job's current status, without source IDs, private brief, checkpoint or note content. A later private or summary edit leaves the selected prose intact. A selected worker lane can derive its headline and milestones from enrolled jobs currently assigned to it; its public label is explicitly selected. Assignment/status changes refresh that optional projection transactionally; unchanged public content emits no public activity. A projection failure retains the last good publication and a private diagnostic without inventing a successful update.
