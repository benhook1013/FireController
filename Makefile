GO ?= go
GOFMT ?= gofmt

.PHONY: check fmt-check test build docs-check

check: fmt-check test docs-check

fmt-check:
	@files="$$($(GOFMT) -l cmd internal)" || exit $$?; if [ -n "$$files" ]; then printf '%s\n' "$$files"; exit 1; fi

test:
	$(GO) test -race ./...
	$(GO) vet ./...

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/fc ./cmd/fc
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/fcd ./cmd/fcd

docs-check:
	npm run docs:check
