# CloudBurrow build and test commands.
#
# `make check` runs what CI runs. Run it before opening a PR.

BINARY      := cloudburrow
PKG         := github.com/identity-wael/cloudburrow
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

.DEFAULT_GOAL := help

## help: List available targets
.PHONY: help
help:
	@echo "CloudBurrow targets:"
	@grep -E '^## [a-z-]+:' $(MAKEFILE_LIST) | sed 's/## /  /' | sort

## build: Build the binary into bin/
.PHONY: build
build:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/$(BINARY)
	@echo "built $(BIN_DIR)/$(BINARY) ($(VERSION))"

## install: Install the binary into GOBIN
.PHONY: install
install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/$(BINARY)

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

## test-compat: Run official-SDK compatibility tests (build tag: compat)
.PHONY: test-compat
test-compat:
	go test -tags=compat -timeout $(COMPAT_TIMEOUT) ./test/...

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
