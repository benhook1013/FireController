# CI selection

`select-go-tests.py` selects complete, disjoint package and test slices from successful `go list ./...` and `go test -race -list .` inventories. It refuses malformed, duplicate, unknown or empty selections. Each top-level test, example and fuzz seed in a partitioned package belongs to exactly one slice; its subtests remain with their parent. All other packages, including new packages, join the two remaining-package slices. Benchmarks are not CI tests.

The explicit workflow matrix must contain exactly the selector's shards. `make ci-selection-check` verifies that inventory, every package/test owner and the required validation dependencies. Source/browser checks, all race slices, packaged qualification and both native smoke runners must succeed for `validate` to pass; failed, cancelled or skipped families remain failures.

Local `make check`, `make test`, build and smoke entry points retain their existing coverage. `GO_TEST_PACKAGES` and `GO_TEST_RUN` select an explicit race/vet slice; their defaults include all packages and tests. All available tests retain race coverage and the existing twenty-minute per-package deadline. [Development](../../docs/development.md#build-and-checks) owns commands and qualification boundaries. Selection performs no provider work and grants no publication or deployment authority.
