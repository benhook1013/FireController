# Operator Variables

Operator variables are short opaque strings that an administrator sets for agents to read, such as a provider mode. FireController stores, resolves and returns them. It never interprets a value or acts on one; the meaning belongs to whoever reads it.

## Model

- **Scope.** A variable is instance-wide by default. A project may set an override for the same key, which replaces the instance value for that project only.
- **Key.** 1-64 bytes of `a-z`, `0-9`, `_`, `.` and `-`, starting with a letter or digit.
- **Value.** 1-240 bytes of UTF-8 without control characters or surrounding whitespace. Unsetting a key records a revision with no value.
- **Bound.** At most 32 set keys per scope: the instance, and each project's overrides.
- **History.** Each key in each scope has its own revision sequence starting at 1. Every set or unset appends an immutable revision with its actor and time. Writing the current value again is `unchanged` and adds no revision.

## Access

- Any credential that can read the project reads the effective values for that project.
- Writes require project administration. Instance-wide writes additionally require an instance administrator credential.
- An optional `expected_revision` guards a write; without one, writes are last-write-wins with full history.

## API

| Route | Result |
| --- | --- |
| `GET /api/v1/projects/{project_id}/vars` | `{"variables":[{key,value,scope,revision,updated_at}]}`: every effective value, sorted by key. `scope` names the source, `instance` or `project`. |
| `GET /api/v1/projects/{project_id}/vars/{key}` | `{key,set,value,scope,instance,project}`: `set` reports whether an effective value exists; `value` and `scope` are omitted when it does not. `instance` and `project` are each scope's current entry `{set,value,revision,actor_id,updated_at}`, omitted when that scope has never written the key. |
| `PUT /api/v1/projects/{project_id}/vars/{key}` | Body `{scope, value, expected_revision?}`, `scope` is `instance` or `project`. `value` is required: a string sets, an explicit null unsets. Requires an `Idempotency-Key`. Returns `{key,scope,revision,outcome}` with outcome `updated` or `unchanged`. |
| `GET /api/v1/projects/{project_id}/vars/{key}/history` | Query `scope` (default `instance`), `limit`, `cursor`. One newest-first page of `{revision,set,value,actor_id,created_at}`; an unset revision has `set` false and no `value`. |

A revision conflict returns `409 operator_variable_revision_conflict` with `current_revision`. Setting a new key in a full scope returns `409 operator_variable_limit`.

## Clients

- CLI: `vars list`, `vars get KEY`, `vars set KEY VALUE`, `vars unset KEY` and `vars history KEY`. `--project` selects the bound project's override for set, unset and history. Set and unset accept `--expect-revision N` and generate an idempotency key unless `--key` is given. `vars help` is local.
- MCP: `var_read` with an optional `key`. Without a key it returns the effective list; with one it returns the single-key detail. There is no MCP write.
- `doctor` (CLI and MCP) includes `operator_variables`, a map of effective key to value, so a session start sees them without another call. The field is absent when the service cannot provide it.

## Backups

Variables live in one append-only table, `operator_variable_history`, in schema domain `operator_variables`. Backup inventory validates contiguous revisions, key and value bounds, the per-scope bound and that project rows name an existing project.
