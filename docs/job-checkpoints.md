# Job Progress History

[Unified job state](job-state.md) owns current progress and the one normal update operation. Default reads contain the current progress record; older progress requires an explicit `jobs progress-history` / `job_progress_history` read. Agents do not choose between narrative updates and checkpoints or maintain both.

Schema-eight migration converts earlier checkpoint and narrative records into the canonical progress archive, retaining original source records for historical inspection. Those source archives are not another current-state authority or everyday write interface.

## Proof and limits

Earlier checkpoint-only proof belongs to its pinned historical source, including `1b824d31` and the packaged checks described in repository history. Those results do not prove the unified model. Current migration, default-read, atomic-update, client/browser and installed proof are tracked at the [job-state owner](job-state.md#required-proof).
