GO ?= go
GOFMT ?= gofmt
GO_TEST_PACKAGES ?= ./...
GO_TEST_RUN ?= .
ACTIONLINT ?= $(GO) run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12

.PHONY: check workflow-check fmt-check test build docs-check browser-check browser-build smoke browser-smoke

check: workflow-check browser-check fmt-check test docs-check

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

docs-check:
	npm run docs:check

browser-check:
	npm --prefix webapp run check

browser-build:
	npm --prefix webapp run build

# Requires the actual outputs from make build; no implicit rebuild.
smoke:
	$(GO) test -tags=packaged_smoke -run '^TestPackaged(Smoke|UpgradeSmoke|PublicationSmoke|MapImportSmoke|TransferSmoke|HistoryCollectionSmoke)$$' -count=1 -timeout=60s ./tools/packagedsmoke

# Requires built fcd and the browser installed for webapp's pinned Playwright.
browser-smoke:
	@test "$$($(GO) env GOOS)" = linux || { printf '%s\n' 'browser-smoke requires Linux' >&2; exit 1; }
	$(GO) test -tags=packaged_smoke -run '^TestPackaged(BrowserSmoke|PublicBrowserSmoke|ReviewArchiveSmoke)$$' -count=1 -timeout=60s ./tools/packagedsmoke
