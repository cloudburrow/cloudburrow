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
TEST_TIMEOUT        ?= 5m
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

## bigquery-binaries: Build the patched Linux BigQuery emulator the CLI embeds, from pinned, checksummed sources (#1061)
# goccy/bigquery-emulator v0.8.1 with CloudBurrow's patches to its SQL
# engine (third_party/bigquery-emulator). About two minutes from an empty Go
# build cache on a 16-core machine; nothing when the inputs are unchanged.
# BQENGINE_FLAGS=-prebuilt builds nothing and fails unless the binaries in
# place were built from these sources: CI builds them once per run (#1087).
BQENGINE_FLAGS ?=
.PHONY: bigquery-binaries
bigquery-binaries:
	go run ./tools/bqengine -out internal/bigqueryimage/bin $(BQENGINE_FLAGS)

## bigquery-patch-tests: Run the BigQuery emulator patches' regression tests and the patched modules' suites (#1132)
# In the patched copies tools/bqengine writes under
# third_party/bigquery-emulator/build/, which `go test ./...` here does not
# reach. CI runs it on pushes to main only.
BQ_PATCH_TEST_PKGS = \
	github.com/goccy/bigquery-emulator/server/... \
	github.com/goccy/bigquery-emulator/internal/... \
	github.com/goccy/bigquery-emulator/types/... \
	github.com/goccy/googlesqlite/... \
	github.com/goccy/go-googlesql
.PHONY: bigquery-patch-tests
bigquery-patch-tests:
	go run ./tools/bqengine -prepare-only
	# googlesqlite's own CLI (cmd/googlesqlite, internal/cli) is not built into
	# the emulator, and its dependencies are not in this module's go.sum.
	# TZ=UTC: googlesqlite's TestBindDateFromUnixDate and
	# TestParseTimeFormatRoundtrip/unixsec (internal/functions/date, helper)
	# expect UTC and fail in any other local zone, as on a developer's machine.
	cd third_party/bigquery-emulator && TZ=UTC GOFLAGS=-mod=readonly GOWORK=off go test -tags http2legacy \
		$$(GOFLAGS=-mod=readonly GOWORK=off go list -e -tags http2legacy $(BQ_PATCH_TEST_PKGS) | grep -v -e '/googlesqlite/cmd/googlesqlite$$' -e '/googlesqlite/internal/cli$$')

## build: Build the binary into bin/, with the Linux storage server and BigQuery emulator embedded
.PHONY: build
build: storage-binaries bigquery-binaries
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/$(BINARY)
	@echo "built $(BIN_DIR)/$(BINARY) ($(VERSION))"

## cross: Build and vet every package for all four release targets (#713)
# `make build` compiles only the host target, so a GOOS- or GOARCH-specific
# break would otherwise surface only when release.yml builds a tag.
CROSS_TARGETS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64
.PHONY: cross
cross:
	@for t in $(CROSS_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "cross: $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build ./... || exit 1; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go vet ./... || exit 1; \
	done
	@echo "cross ok: $(CROSS_TARGETS)"

## build-onnx: Build bin/cloudburrow-onnx with -tags onnx (local embeddings, #41): cgo, the pinned libtokenizers.a, and the embedded binaries
# The onnx build serves the community ONNX EmbeddingGemma conversion
# (docs/embeddings.md). It links Hugging Face's tokenizers through
# github.com/daulet/tokenizers, whose static library is fetched from that
# module's release at the version go.mod pins and checked against the
# SHA-256 below; ONNX Runtime itself is loaded when the binary runs
# (ONNXRUNTIME_LIB), so nothing else is needed to build. No model is
# fetched: `up -local-ai-embeddings` does that at run time.
TOKENIZERS_VERSION := v1.27.0
TOKENIZERS_PLATFORM := $(shell go env GOOS)-$(shell go env GOARCH)
# The release's asset names, and their SHA-256, by GOOS-GOARCH.
TOKENIZERS_ASSET_darwin-arm64 := darwin-arm64
TOKENIZERS_ASSET_darwin-amd64 := darwin-x86_64
TOKENIZERS_ASSET_linux-amd64 := linux-amd64
TOKENIZERS_ASSET_linux-arm64 := linux-arm64
TOKENIZERS_SHA256_darwin-arm64 := fb84b8b2e349a5952767ffe80ccd862fc44084de47f3b0cc3f0b7c9d4e649cf7
TOKENIZERS_SHA256_darwin-amd64 := 6239efe5a81fde8089ef2df8ae710366542b4e5deab6d8ecb74d7d1862db2ddb
TOKENIZERS_SHA256_linux-amd64 := 72556cdca798dd4ea7cdaba308e5f0d68a8cb93b67c96edf485b7a0edd7b07f4
TOKENIZERS_SHA256_linux-arm64 := e96545ad05930c26f51f63d932ee6d3bbd32bbed149e102c5290d587a2293067
TOKENIZERS_LIB_DIR ?= $(BIN_DIR)/tokenizers/$(TOKENIZERS_VERSION)/$(TOKENIZERS_PLATFORM)
.PHONY: build-onnx
build-onnx: storage-binaries bigquery-binaries onnx-compile
	@echo "built $(BIN_DIR)/$(BINARY)-onnx ($(VERSION)); run it with ONNXRUNTIME_LIB=/path/to/libonnxruntime"

## onnx-compile: Build and vet the onnx-tagged CLI alone, without making the embedded binaries (CI's onnx job)
.PHONY: onnx-compile
onnx-compile: $(TOKENIZERS_LIB_DIR)/libtokenizers.a
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=1 CGO_LDFLAGS="-L$(abspath $(TOKENIZERS_LIB_DIR)) $(CGO_LDFLAGS)" \
		go build -tags onnx -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY)-onnx ./cmd/$(BINARY)
	CGO_ENABLED=1 CGO_LDFLAGS="-L$(abspath $(TOKENIZERS_LIB_DIR)) $(CGO_LDFLAGS)" \
		go vet -tags onnx ./internal/embedding/ ./internal/localai/ ./cmd/$(BINARY)/

$(BIN_DIR)/tokenizers/$(TOKENIZERS_VERSION)/%/libtokenizers.a:
	@test -n "$(TOKENIZERS_ASSET_$*)" -a -n "$(TOKENIZERS_SHA256_$*)" || { echo "build-onnx: no pinned libtokenizers for $$(go env GOOS)/$$(go env GOARCH); set TOKENIZERS_LIB_DIR to a directory holding libtokenizers.a"; exit 1; }
	@mkdir -p $(@D)
	curl -fsSL -o $(@D)/libtokenizers.tar.gz \
		https://github.com/daulet/tokenizers/releases/download/$(TOKENIZERS_VERSION)/libtokenizers.$(TOKENIZERS_ASSET_$*).tar.gz
	echo "$(TOKENIZERS_SHA256_$*)  $(@D)/libtokenizers.tar.gz" | shasum -a 256 -c -
	tar -xzf $(@D)/libtokenizers.tar.gz -C $(@D) libtokenizers.a
	@rm -f $(@D)/libtokenizers.tar.gz

## install: Install the binary into GOBIN
.PHONY: install
install: storage-binaries bigquery-binaries
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

## compat-node: Run the official Node.js client suite against a running instance (CLOUDBURROW_ARGS names it)
.PHONY: compat-node
NPM ?= npm
NODE ?= node
COMPAT_NODE_DIR ?= test/compat-node
COMPAT_NODE_TESTS ?= *.test.mjs
# Extra node --test flags, such as --test-name-pattern or --test-skip-pattern.
COMPAT_NODE_FLAGS ?=
compat-node: build
	cd $(COMPAT_NODE_DIR) && $(NPM) ci --ignore-scripts --no-audit --no-fund
	cd $(COMPAT_NODE_DIR) && CLOUDBURROW_BIN=$(abspath $(BIN_DIR)/$(BINARY)) CLOUDBURROW_ARGS="$(CLOUDBURROW_ARGS)" \
		$(NODE) --import ./guard.mjs --test --test-concurrency=1 --test-timeout=300000 --test-reporter=spec $(COMPAT_NODE_FLAGS) $(COMPAT_NODE_TESTS)

## cloud-sql-proxy: Fetch Google's pinned Cloud SQL Auth Proxy binary, checked against its SHA-256, for the compat test (#1421)
# The release the Cloud Run adapter's sidecar image is (internal/adapter/run
# /cloudsql.go), from Google's release bucket; the SHA-256 of each asset is
# the one Google's release page lists
# (github.com/GoogleCloudPlatform/cloud-sql-proxy/releases/tag/v2.19.0).
# Downloaded once, checked before it is made executable, and kept under bin/.
# TestCloudSQLAuthProxyBinary runs it when CLOUDBURROW_TEST_CLOUD_SQL_PROXY
# names it: export CLOUDBURROW_TEST_CLOUD_SQL_PROXY=$$(make -s cloud-sql-proxy).
CLOUD_SQL_PROXY_VERSION := v2.19.0
CLOUD_SQL_PROXY_PLATFORM := $(shell go env GOOS).$(shell go env GOARCH)
CLOUD_SQL_PROXY_SHA256_linux.amd64 := 218ae2a18ef8295c48809b73962f326caca1270aad4623f16c4ff5fa1e9f7d3e
CLOUD_SQL_PROXY_SHA256_linux.arm64 := a58e9e0d4f10f368dbe2e24079597eb005dd9352b43fb18726f7983f913e6c1f
CLOUD_SQL_PROXY_SHA256_darwin.arm64 := 0b8d095a3808a50253c31825b24e30c63d34a9122977de2244749b8da49ad7f7
CLOUD_SQL_PROXY_BIN := $(BIN_DIR)/cloud-sql-proxy/$(CLOUD_SQL_PROXY_VERSION)/$(CLOUD_SQL_PROXY_PLATFORM)/cloud-sql-proxy
.PHONY: cloud-sql-proxy
cloud-sql-proxy: $(CLOUD_SQL_PROXY_BIN)
	@echo $(abspath $(CLOUD_SQL_PROXY_BIN))

$(BIN_DIR)/cloud-sql-proxy/$(CLOUD_SQL_PROXY_VERSION)/%/cloud-sql-proxy:
	@test -n "$(CLOUD_SQL_PROXY_SHA256_$*)" || { echo "cloud-sql-proxy: no pinned binary for $*" >&2; exit 1; }
	@mkdir -p $(@D)
	@curl --retry 5 --retry-all-errors -fsSL -o $@.download \
		https://storage.googleapis.com/cloud-sql-connectors/cloud-sql-proxy/$(CLOUD_SQL_PROXY_VERSION)/cloud-sql-proxy.$*
	@echo "$(CLOUD_SQL_PROXY_SHA256_$*)  $@.download" | shasum -a 256 -c - >&2 || { rm -f $@.download; exit 1; }
	@chmod +x $@.download && mv $@.download $@

## fmt: Format all Go source
.PHONY: fmt
fmt:
	gofmt -s -w .

## fmt-check: Fail if any file is not formatted
# The files git tracks or would add, so an ignored directory, such as the
# BigQuery engine's build tree (third_party/bigquery-emulator/build), is not
# scanned on a developer's machine when CI's clean checkout has none.
.PHONY: fmt-check
fmt-check:
	@unformatted=$$(git ls-files -z --cached --others --exclude-standard -- '*.go' | xargs -0 gofmt -s -l); \
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

## verify-local: Run CI's compat shards on this machine at --port-base 41000 and write a dated JSON result (VERIFY_ARGS: script flags; #705)
.PHONY: verify-local
VERIFY_ARGS ?=
verify-local:
	scripts/verify-local.sh $(VERIFY_ARGS)

## deps-check: Report newer upstream versions (discovery only; changes nothing)
.PHONY: deps-check
deps-check:
	go run ./tools/depcheck --inventory dependencies.json

## tidy: Tidy and verify module dependencies
.PHONY: tidy
tidy:
	go mod tidy
	go mod verify
	cd test/oracle/fakekms && go mod tidy && go mod verify
	cd testdata/worker && go mod tidy && go mod verify
	cd testdata/function && go mod tidy && go mod verify
	cd testdata/sourcebuild && go mod tidy && go mod verify

## docs-check: Fail if a generated doc is stale, a relative Markdown link is broken, or the module map or Console table drifts
.PHONY: docs-check
docs-check:
	go run ./tools/coverage -check
	go run ./tools/doclinks
	go run ./tools/docsmap

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
