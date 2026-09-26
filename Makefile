# CloudBurrow build and test commands.
#
# `make check` runs what CI runs. Run it before opening a PR.

BINARY      := cloudburrow
PKG         := github.com/cloudburrow/cloudburrow
BIN_DIR     := bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

VERSION_PKG := $(PKG)/internal/version
LDFLAGS     := -s -w \
	-X '$(VERSION_PKG).version=$(VERSION)' \
	-X '$(VERSION_PKG).commit=$(COMMIT)' \
	-X '$(VERSION_PKG).date=$(BUILD_DATE)'

# Timeouts are explicit so a hung test fails rather than stalling CI.
TEST_TIMEOUT        ?= 2m
INTEGRATION_TIMEOUT ?= 10m
COMPAT_TIMEOUT      ?= 10m
UPSTREAM_TIMEOUT    ?= 15m
E2E_TIMEOUT         ?= 25m

.DEFAULT_GOAL := help

## help: List available targets
.PHONY: help
help:
	@echo "CloudBurrow targets:"
	@grep -E '^## [a-z-]+:' $(MAKEFILE_LIST) | sed 's/## /  /' | sort

## storage-binaries: Cross-build the Linux storage server the CLI embeds (#514)
.PHONY: storage-binaries
storage-binaries:
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o internal/storageimage/bin/cloudburrow-storage-linux-$$arch ./cmd/cloudburrow-storage || exit 1; \
	done

## build: Build the binary into bin/, with the Linux storage server embedded
.PHONY: build
build: storage-binaries
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/$(BINARY)
	@echo "built $(BIN_DIR)/$(BINARY) ($(VERSION))"

## install: Install the binary into GOBIN
.PHONY: install
install: storage-binaries
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/$(BINARY)

## compat-python: Run the official Python SDK suite against a running instance (CLOUDBURROW_ARGS names it)
.PHONY: compat-python
PYTHON ?= python3
COMPAT_PY_VENV ?= .venv-compat-python
COMPAT_PY_TESTS ?= test/compat-python
compat-python: build
	$(PYTHON) -m venv $(COMPAT_PY_VENV)
	$(COMPAT_PY_VENV)/bin/pip install --quiet --require-hashes -r test/compat-python/requirements.lock
	CLOUDBURROW_BIN=$(abspath $(BIN_DIR)/$(BINARY)) CLOUDBURROW_ARGS="$(CLOUDBURROW_ARGS)" \
		$(COMPAT_PY_VENV)/bin/python -m pytest -p no:cacheprovider -v $(COMPAT_PY_TESTS)

## fmt: Format all Go source
.PHONY: fmt
fmt:
	gofmt -s -w .

## fmt-check: Fail if any file is not formatted
.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -s -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-ed:"; echo "$$unformatted"; exit 1; \
	fi
	@echo "formatting ok"

## vet: Run go vet
.PHONY: vet
vet:
	go vet ./...

## test: Run unit tests
.PHONY: test
test:
	go test -timeout $(TEST_TIMEOUT) ./...

## test-race: Run unit tests with the race detector
.PHONY: test-race
test-race:
	go test -race -timeout $(TEST_TIMEOUT) ./...

## test-cover: Run unit tests and report coverage
.PHONY: test-cover
test-cover:
	go test -timeout $(TEST_TIMEOUT) -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

## test-integration: Run tests requiring Docker (build tag: integration)
.PHONY: test-integration
test-integration:
	go test -tags=integration -timeout $(INTEGRATION_TIMEOUT) ./...

## test-upstream: Run upstream-component probes for the reuse audit (build tag: upstream)
.PHONY: test-upstream
test-upstream:
	go test -tags=upstream -timeout $(UPSTREAM_TIMEOUT) ./test/upstream/...

## test-e2e: Run the acceptance workflow against a running instance (build tag: e2e)
.PHONY: test-e2e
test-e2e:
	go test -tags=e2e -timeout $(E2E_TIMEOUT) -v ./test/e2e/...

## test-compat: Run official-SDK compatibility tests (build tag: compat)
.PHONY: test-compat
test-compat:
	go test -tags=compat -timeout $(COMPAT_TIMEOUT) ./test/...

## oracle-kms: Compare Cloud KMS against a SHA-pinned fakekms, in its own module (#419; not part of check)
.PHONY: oracle-kms
oracle-kms:
	cd test/oracle/fakekms && go test -count=1 -v ./...

## footprint: Measure cold/warm start and memory for the default and all-services profiles (#312)
.PHONY: footprint
footprint: build
	scripts/footprint.sh footprint.json

## deps-check: Report newer upstream versions (discovery only; changes nothing)
.PHONY: deps-check
deps-check:
	go run ./tools/depcheck --inventory dependencies.json

## tidy: Tidy and verify module dependencies
.PHONY: tidy
tidy:
	go mod tidy
	go mod verify

## check: Everything CI runs (fmt-check, vet, test-race)
.PHONY: check
check: fmt-check vet test-race
	@echo "all checks passed"

## clean: Remove build and coverage output
.PHONY: clean
clean:
	rm -rf $(BIN_DIR) coverage.out

## test-localai: Run the local generation suite against the real runtime and a
## real model, driven by the official Google Gen AI SDK (build tag: localai).
## Needs `make litert-lm` and a model artifact; skips when CLOUDBURROW_TEST_MODEL
## is unset.
.PHONY: test-localai
test-localai:
	go test -tags=localai -count=1 -timeout 30m ./test/localai/...

## litert-lm: build the local AI runtime image from Google's source.
##
## No LiteRT-LM release publishes a Linux artifact, so CloudBurrow builds one.
## It is a Bazel C++ build. Measured at 6m17s wall clock from a cold Docker
## cache on an Apple M4 Max with 16 CPUs allocated to the daemon, of which the
## Bazel step is 4m41s; a machine with fewer cores will take proportionally
## longer, so treat this as a floor rather than an estimate. Deliberately not
## part of `make build` — nobody should pay that cost unless they are going to
## use local AI.
litert-lm:
	docker build -t cloudburrow/litert-lm:local deploy/litert-lm
