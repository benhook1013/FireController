# CI selection

`development-checks.py` compares a development candidate against the exact source of the latest successful development CI run. Only website source/assets and the browser-experience document can reuse backend qualification; missing, malformed or unavailable source comparisons select full checks. PRs and `main` always select full checks. See [build publication](../../docs/releases.md) for the qualification and trust boundaries.

`select-go-tests.py` selects the requested package/test slice from successful `go list ./...` and `go test -race -list .` inventories. It refuses malformed, duplicate-package, unknown or empty selections. Repeated valid discovery names are deduplicated before partitioning: internal and external test packages may register the same name, and one anchored selection executes both implementations in their assigned shard. Partitioned families retain every top-level test, example and fuzz seed exactly once; their subtests stay with the parent. All other packages, including new packages, join the two remaining-package slices. Benchmarks are not CI tests.

The workflow matrix and this selector must contain the same shards; `make ci-selection-check` verifies that boundary and the required validation dependencies. Full test, runtime-budget and build ownership is described in [development](../../docs/development.md#build-and-checks). No selection changes runtime deadlines or authorizes provider work.

The [CI scheduling contract](scheduling.md) owns this workflow's job inventory, per-PR and per-branch supersession, twenty shared job lanes and rollout checks.
