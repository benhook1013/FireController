GO ?= go
GOFMT ?= gofmt
GO_TEST_PACKAGES ?= ./...
GO_TEST_RUN ?= .
ACTIONLINT ?= $(GO) run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12

.PHONY: check workflow-check ci-selection-check fmt-check test test-race vet build docs-check browser-check browser-build smoke browser-smoke

check: workflow-check ci-selection-check browser-check fmt-check test docs-check

# actionlint v1.7.12 lacks GitHub concurrency.queue (upstream #657).
# CI selection validates the rollout gate, every job slot and queue declaration; every other lint error remains fatal.
workflow-check:
	$(ACTIONLINT) -ignore '^unexpected key "queue" for "concurrency" section[.] expected one of "cancel-in-progress", "group"$$' -shellcheck="" -pyflakes="" .github/workflows/ci.yml

ci-selection-check:
	python3 -m unittest discover -s tools/ci -p 'test_*.py'

fmt-check:
	@files="$$($(GOFMT) -l cmd internal tools)" || exit $$?; if [ -n "$$files" ]; then printf '%s\n' "$$files"; exit 1; fi

test: test-race
	$(MAKE) vet

test-race:
	$(GO) test -race -p=2 -timeout=20m -run '$(GO_TEST_RUN)' $(GO_TEST_PACKAGES)

vet:
	$(GO) vet $(GO_TEST_PACKAGES)

build: browser-build
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/fc ./cmd/fc
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/fcd ./cmd/fcd
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/fcmcp ./cmd/fcmcp

docs-check:
	npm run docs:check

browser-check:
	npm --prefix webapp run check

browser-build:
	npm --prefix webapp run build

# Requires the actual outputs from make build; no implicit rebuild.
smoke:
	$(GO) test -v -vet=all -tags=packaged_smoke -run '^TestPackagedSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke
	$(GO) test -tags=packaged_smoke -run '^TestPackagedUpgradeSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke
	$(GO) test -tags=packaged_smoke -run '^TestPackagedPublicationSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke
	FC_SMOKE_BIN_DIR="$(CURDIR)/bin" $(GO) test -v -tags=packaged_smoke -run '^TestPackagedHostedSubmittedSourceRetention$$' -count=1 -timeout=60s ./internal/backup
	$(GO) test -tags=packaged_smoke -run '^TestPackagedTransferSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke

# Requires built fcd and the browser installed for webapp's pinned Playwright.
browser-smoke:
	@test "$$($(GO) env GOOS)" = linux || { printf '%s\n' 'browser-smoke requires Linux' >&2; exit 1; }
	$(GO) test -tags=packaged_smoke -run '^Test(PackagedBrowserSmoke|BrowserFailureDiagnosticsRetained)$$' -count=1 -timeout=60s ./tools/packagedsmoke
	$(GO) test -tags=packaged_smoke -run '^TestPackagedPublicBrowserSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke

# Requires built fcd and an explicit rclone binary; uses disposable loopback S3.
.PHONY: offsite-smoke
offsite-smoke:
	@test -n "$(RCLONE)" || { printf '%s\n' 'offsite-smoke requires RCLONE=/absolute/path/to/rclone' >&2; exit 1; }
	@test -x "$(CURDIR)/bin/fcd" || { printf '%s\n' 'offsite-smoke requires bin/fcd from make build' >&2; exit 1; }
	FC_TEST_RCLONE="$(RCLONE)" FC_TEST_FCD="$(CURDIR)/bin/fcd" $(GO) test -tags=offsite_integration -run '^TestOffsiteRealRcloneS3AndCryptRecovery$$' -count=1 -timeout=180s ./internal/offsite
