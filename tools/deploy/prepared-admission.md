# Prepared image admission

The optional split coordinator separates one-time image preparation from later rollout admission. Preparation holds the existing release and artifact leases, but never acquires backup phase, batch or provider leases. Admission retains the existing natural-window and live-preimage guards. This does not extend the backup service budget, restart transfers, change timer settings or provide another rollout authority.

## Fresh operation contract

An operator renders a new operation from an exact reviewed coordinator template. `render_split_coordinator.py` requires its expected source digest and original root, preserves the existing command/reaping, preimage and window functions, and changes only the fresh root and entrypoint. The caller's expected digest is provenance supplied by the release review, not independent approval. The resulting reviewed namespace must provide the existing release lease path as `RELEASE_LEASE`.

The complete execution manifest must include `coordinator_split_adapter.py`, `prepared_release.py`, `prepared_image_content.py` and `release/tools/deploy/artifact_retention.py`. The rendered entrypoint verifies the review and every manifest member before importing helper code. Use the existing isolated Python invocation and a trusted private operation tree. This is not protection against a hostile interpreter, preloaded module, or noncooperating concurrent namespace replacement.

`--prepare-reviewed-image` invokes the existing image producer once under release and artifact custody. A durable start marker fences interrupted or failed preparation. Only a successful producer, unchanged preimage and verified prepared content permit a completion receipt. Unknown preparation is never automatically retried or adopted from a historical operation.

`--admit-reviewed-operation` reacquires release then artifact custody, verifies the prepared identity, archive and OCI manifest/configuration/layer content, and rechecks the original live preimage and natural window. A closed window can defer admission without rebuilding. A durable admission-start marker is written before the original rollout; once present, it prohibits replay even if execution fails. The original rollout retains its own fresh custody, preimage, window and reconciliation checks.

## Cost and recovery

OCI verification reads complete content and is an admission-time check, not a polling or UI operation. Content reads use bounded subprocess timeouts and inherited lease descriptors with process-group cleanup. The size check occurs after capture; it is not a hard bound on stdout allocation. Preparation and admission still require appropriately bounded operator supervision and a qualified release slot.

Process-exit tests cover preparation-start, uncertain producer return, completed preparation and admission-start. They establish that a new process can use only completed preparation and cannot replay started work. They do not simulate host power loss or establish new automatic repair authority. Preserve interrupted operation evidence and reconcile explicitly rather than deleting markers.

## Integration boundary

These helpers are not installed or invoked by daemon startup, backup timers, ordinary package generation or the default deployment helper. Each adoption requires a freshly rendered complete package, exact current source and preimage bindings, independent review, and qualification of the actual producer and rollout path. Unit tests use controlled OCI and coordinator callbacks; a read-only content check does not itself prove a new image build or live rollout.
