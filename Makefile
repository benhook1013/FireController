GO ?= go
GOFMT ?= gofmt
GO_TEST_PACKAGES ?= ./...
GO_TEST_RUN ?= .
ACTIONLINT ?= $(GO) run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12

.PHONY: check workflow-check fmt-check test build docs-check browser-check browser-build smoke browser-smoke legacyimport migration-smoke source-converter-check

check: workflow-check browser-check fmt-check test docs-check source-converter-check

workflow-check:
	$(ACTIONLINT) -shellcheck="" -pyflakes="" .github/workflows/ci.yml

fmt-check:
	@files="$$($(GOFMT) -l cmd internal tools)" || exit $$?; if [ -n "$$files" ]; then printf '%s\n' "$$files"; exit 1; fi

test:
	$(GO) test -race -p=2 -timeout=20m -run '$(GO_TEST_RUN)' $(GO_TEST_PACKAGES)
	$(GO) vet $(GO_TEST_PACKAGES)

build: browser-build
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/fc ./cmd/fc
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/fcd ./cmd/fcd
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/fcmcp ./cmd/fcmcp

# Optional source-specific migration utility; not part of the service build.
legacyimport:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/legacyimport ./tools/legacyimport

# Requires make build and make legacyimport; never rebuild during the workload.
migration-smoke:
	$(GO) test -tags=packaged_smoke -run '^TestPackagedCoordinationImportSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke
	$(GO) test -tags=packaged_smoke -run '^TestPackagedOperationalAdoptionSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke

docs-check:
	npm run docs:check

browser-check:
	npm --prefix webapp run check

browser-build:
	npm --prefix webapp run build

# Requires the actual outputs from make build; no implicit rebuild.
smoke:
	$(GO) test -vet=all -tags=packaged_smoke -run '^TestPackagedSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke
	$(GO) test -tags=packaged_smoke -run '^TestPackagedUpgradeSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke
	$(GO) test -tags=packaged_smoke -run '^TestPackagedPublicationSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke
	$(GO) test -tags=packaged_smoke -run '^TestPackagedMapImportSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke
	$(GO) test -tags=packaged_smoke -run '^TestPackagedTransferSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke
	$(GO) test -tags=packaged_smoke -run '^TestPackagedHistoryCollectionSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke
	$(GO) test -tags=packaged_smoke -run '^TestPackagedRoundControlsSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke
	$(GO) test -tags=packaged_smoke -run '^TestPackagedRunSelectionSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke

# Requires built fcd and the browser installed for webapp's pinned Playwright.
browser-smoke:
	@test "$$($(GO) env GOOS)" = linux || { printf '%s\n' 'browser-smoke requires Linux' >&2; exit 1; }
	$(GO) test -tags=packaged_smoke -run '^TestPackagedBrowserSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke
	$(GO) test -tags=packaged_smoke -run '^TestPackagedPublicBrowserSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke
	$(GO) test -tags=packaged_smoke -run '^TestPackagedReviewArchiveSmoke$$' -count=1 -timeout=60s ./tools/packagedsmoke

# Optional source adapters: no installed source root is discovered by tests.
source-converter-check:
	python3 -m unittest discover -s tools/legacyimport -p 'test_*.py'
