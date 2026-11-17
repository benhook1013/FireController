# Deployment producer space fixes

`image_layers.py` implements a construction-time fix for the local pinned-image producer. Before appending the newly qualified daemon layer, call `replacement_base(manifest, config, get_blob)` and use **both** returned manifest and config as the base. It strips only a terminal history suffix marked as qualified FireController daemon overlays, after verifying each compressed digest, size, uncompressed diff ID and exact single regular `fcd` file with mode 0555. An unknown suffix is retained; falsely marked or corrupt content is rejected. Runtime configuration is unchanged. Existing containerd images, tags, blobs, current and rollback selections are never deleted. New image import/tag/inspection and deployment guards remain owned by the existing producer.

This prevents each new local deployment from depending on all old daemon overlays. It does not reclaim existing pinned images. Their disposal needs a separate current/rollback and reference inventory.

The installation-specific rollout must also create a versioned helper binary **only when helper units will actually be switched**. For a schema-compatible daemon-only rollout with no helper changes, use the qualified package binary for post-rollout backup-status verification instead of creating an otherwise unused helper release. Preserve the actual selected helper and any running process references.

These fixes address recurrent producer allocations. They do not enable the artifact collector, dispose of mutated rehearsal databases, select historical cleanup candidates, or automatically install a new deployment template. The production preparation entrypoint must use the qualified changed producer rather than copying an older release script. Keep exact before/after source and immutable original deployment receipts; do not replay a completed release operation.

Run `python3 -m unittest discover -s tools/deploy -p 'test_image_layers.py'` for the layer contract tests. Full deployment-helper tests use `test_*.py` in the same directory.

## Bounded schema rehearsal production

`bounded_rehearsal.py` is the producer entrypoint for populated schema rehearsals. A private shared workspace and nonblocking budget lease prevent release-specific copies from accumulating. The existing managed-backup storage-phase lease protects the source for copying and equivalence verification. An existing incomplete, failed or uniquely migrated output prevents another copy; the producer does not silently collect it or create an archive. Existing schema-unchanged qualification still skips the rehearsal entirely.

Only this invocation's byte-identical successful database output is automatically unlinked, after checking the retained original and consumer lease again. Its staged runner is removed only when identical to the retained original runner. Reports and receipts remain, with a maximum of 1024 settled operation receipts. Unique migrated output requires an explicit disposition before the next populated rehearsal. Failed or unknown output is retained. No historical adoption, force-unlock, backup pruning or vendor-review processing occurs.

`prepare_rehearsal.render` produces a supervised entrypoint from installation-owned fixed shared paths and explicit candidate inputs, or preserves an explicit schema-unchanged proof without copying a database. The installation's canonical successor preparer must require one of these modes and emit `rehearse.py`; merely shipping these helpers does not prove production adoption. Focused tests: `python3 -m unittest discover -s tools/deploy -p 'test_bounded_rehearsal.py'`.
