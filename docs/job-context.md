# Current Job Reads

The [job-state contract](job-state.md#default-reads) owns the default `jobs read` / `job_read` result: complete current task definition and progress, remaining checklist/counts and compact pending notes attached to that job. This is the ordinary resume read. Worker messages, pause and focus retain separate tools; a job read does not expand their context or any archive.

The former combined job-context packet mixed job state with a worker's inbox and exposed a separate checkpoint choice. It is replaced by the default job read. Earlier source checkpoints and their immutable test evidence describe that historical interface, not additional calls an agent should make now.
