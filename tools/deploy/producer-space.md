# Deployment producer space fixes

`image_layers.py` implements a construction-time fix for the local pinned-image producer. Before appending the newly qualified daemon layer, call `replacement_base(manifest, config, get_blob)` and use **both** returned manifest and config as the base. It strips only a terminal history suffix marked as qualified FireController daemon overlays, after verifying each compressed digest, size, uncompressed diff ID and exact single regular `fcd` file with mode 0555. An unknown suffix is retained; falsely marked or corrupt content is rejected. Runtime configuration is unchanged. Existing containerd images, tags, blobs, current and rollback selections are never deleted. New image import/tag/inspection and deployment guards remain owned by the existing producer.

This prevents each new local deployment from depending on all old daemon overlays. It does not reclaim existing pinned images. Their disposal needs a separate current/rollback and reference inventory.

The installation-specific rollout must also create a versioned helper binary **only when helper units will actually be switched**. For a schema-compatible daemon-only rollout with no helper changes, use the qualified package binary for post-rollout backup-status verification instead of creating an otherwise unused helper release. Preserve the actual selected helper and any running process references.

These fixes address recurrent producer allocations. They do not enable the artifact collector, dispose of mutated rehearsal databases, select historical cleanup candidates, or automatically install a new deployment template. The production preparation entrypoint must use the qualified changed producer rather than copying an older release script. Keep exact before/after source and immutable original deployment receipts; do not replay a completed release operation.

Run `python3 -m unittest discover -s tools/deploy -p 'test_image_layers.py'` for the layer contract tests. Full deployment-helper tests use `test_*.py` in the same directory.
