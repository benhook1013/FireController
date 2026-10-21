# Automatic GitHub observation

## Implementation status

Source implements service-owned PR lifecycle synchronization and automatic external Hosted review discovery. Focused parser, scheduler, archive, migration and backup checks pass; the combined repository checks are in progress. Matching-client installation, retained-data upgrade and installed browser qualification remain pending. These source changes are not the installed service.

## Lifecycle and CI

The service observes existing enrolled, visible PRs without an agent maintaining the dashboard or a visitor pressing Refresh. Ordinary agent and browser reads remain provider-free. Committed observations reach connected browsers through the existing project event feed.

One scheduler serves configured projects with project fairness, one in-flight observation per project, account serialization and at most four workers. Default lifecycle intervals are one minute for open PRs, five minutes for closed PRs and six hours for merged PRs. Required CI is collected every five minutes and after a changed head. Provider throttling and failures delay account work with bounded backoff. An incomplete or ambiguous external inventory retains a per-PR diagnostic and normal retry cadence; it does not delay other PRs or projects on that account. Collection is cancellable; shutdown joins the workers. These intervals describe eligible polling, not a promise that a provider outage or a busy account finishes within that interval.

Repository identity, current enrollment, capability binding and visible/nonretired state are checked before provider work and again before publication. Publication preserves concurrent evidence, queue order, old snapshots and explicit public enrollment. A poll never enrolls or reactivates a PR. Identical observations produce no snapshot or event. A moved comparison cannot retain an inherited passing CI status. Unproven CI remains unknown.

Authenticated conditional GETs revalidate cached response bodies with GitHub. The cache is isolated to the adapter's credential and bounded to 256 entries and 16 MiB; it never supplies an unvalidated stale response or caches a write. Its cadence, cache and backoff are process-local. Restored provider holds prevent observation until the operator releases the restored installation.

## Hosted reviews started outside FireController

Completed CodeRabbit reviews started directly on GitHub are discovered automatically for those same enrolled PRs. Discovery only reads GitHub. It never posts a review command, queries review allowance, launches a CLI process or creates a controller execution attempt. Deferring review work or enabling the operator's review-request pause does not disable this observation.

Stable numeric bot identity and actor kind establish the provider; the login is display text. Collection must be complete within its finite page, item, byte and time budgets. Discovery groups actual provider publications and run IDs rather than inventing a FireController request or using a matching head as proof of ownership. Inline roots, outside-diff findings and duplicate sections retain their source identities. Replies and empty review acknowledgements do not become rounds. Each observed run keeps its actual reviewed head, including a head that is no longer current. A mutable standing summary exposes the run still present when collected; polling cannot reconstruct a summary that GitHub has already overwritten. Unsupported provider formats remain unproven rather than being guessed.

External runs are appended under a separate GitHub producer to the normal retained Hosted history and finding views. Repeated observation and restart do not append another copy. Native-owned reviews are reconciled against actual retained operation, attempt and trigger identities; a pasted marker alone cannot hide an external review. Incomplete or unsupported inventory is deferred and retried rather than appearing as a clean zero-finding review. Once a supported complete inventory is retained, later collection does not overwrite its captured text, decisions or history. The two run timestamps identify provider publication; the observer does not claim to know execution start time or runtime.

On first observation of a PR, the service freezes a durable discovery frontier from its latest already retained non-observer Hosted completion, before making provider calls. Automatic discovery considers later results. The frontier never advances when a new native review completes or the service restarts, so late-arriving external results remain discoverable. Existing older history remains retained; historical gaps before that frontier use explicit collection/import rather than duplicating imported runs whose original provider IDs were not retained.

An external history record does not assert a controller-issued review, consumed controller round, current coverage, completed adjudication or merge readiness. Native decision/fix/routing controls currently require admitted execution evidence; automatically archived external history does not fabricate that evidence. These controls remain distinct from capturing and displaying an external result.

Independent CLI runs are local to the machine that runs them. They are not discoverable through GitHub and need an explicit submission/import path; CLI reviews requested through FireController already retain their results normally.

## Proof boundary

Focused proof covers multiple provider groups, stable identities and forged actors, zero and positive inventories, replies, partial collections, repeated observations, restart, old heads, native/external deduplication and immutable frontiers. Transaction tests must establish rollback, retained history and counts, concurrent native publication and no reactivation. Controlled provider tests must count zero review POSTs, allowance queries and CLI launches, including paused operation and restored-provider holds.

Installed qualification is separate: migrate a verified retained backup, inspect and restore it, install matching clients, verify history and browser updates without agent metadata edits, and record observed update latency. Real existing GitHub results may be read for qualification; no new review is required or authorized by this feature.
