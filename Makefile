.PHONY: help build build-apiserver build-processor build-gc run-apiserver run-processor run-gc run-apiserver-dev run-processor-dev run-gc-dev build-release package-release publish-helm-chart generate-release test test-coverage test-coverage-func clean lint fmt vet tidy install-tools deps-get deps-verify bench check check-container-tool ci image-build image-build-apiserver image-build-processor image-build-gc test-integration test-all test-e2e test-helm dev-deploy dev-clean dev-rm-cluster pre-commit

SHELL := /usr/bin/env bash

TARGETARCH ?= $(shell go env GOARCH)

# Variables
IMAGE_TAG ?= 0.0.1
APISERVER_BINARY=batch-gateway-apiserver
PROCESSOR_BINARY=batch-gateway-processor
GC_BINARY=batch-gateway-gc
APISERVER_PATH=./bin/$(APISERVER_BINARY)
PROCESSOR_PATH=./bin/$(PROCESSOR_BINARY)
GC_PATH=./bin/$(GC_BINARY)
CMD_APISERVER=./cmd/apiserver
CMD_PROCESSOR=./cmd/batch-processor
CMD_GC=./cmd/batch-gc
# Release binaries: name:cmd-path (single source of truth for create-release workflow)
RELEASE_BINARIES := apiserver:$(CMD_APISERVER) processor:$(CMD_PROCESSOR) gc:$(CMD_GC)
BINARIES_DIR ?= dist/binaries
RELEASE_DIR ?= release
APISERVER_IMAGE_TAG_BASE ?= ghcr.io/llm-d-incubation/$(APISERVER_BINARY)
APISERVER_IMG = $(APISERVER_IMAGE_TAG_BASE):$(IMAGE_TAG)
PROCESSOR_IMAGE_TAG_BASE ?= ghcr.io/llm-d-incubation/$(PROCESSOR_BINARY)
PROCESSOR_IMG = $(PROCESSOR_IMAGE_TAG_BASE):$(IMAGE_TAG)
GC_IMAGE_TAG_BASE ?= ghcr.io/llm-d-incubation/$(GC_BINARY)
GC_IMG = $(GC_IMAGE_TAG_BASE):$(IMAGE_TAG)
GO=go
GOFLAGS=
LDFLAGS=-ldflags "-s -w"
BENCHTIME ?= 1s
TEST_FLAGS ?= -race

CONTAINER_TOOL := $(shell (command -v docker >/dev/null 2>&1 && echo docker) || (command -v podman >/dev/null 2>&1 && echo podman) || echo "")
BUILDER := $(shell command -v buildah >/dev/null 2>&1 && echo buildah || echo $(CONTAINER_TOOL))
PLATFORMS ?= linux/amd64 # linux/arm64 # linux/s390x,linux/ppc64le

# Default target
.DEFAULT_GOAL := help

## help: Show this help message
help:
	@echo 'Usage:'
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'

## build-apiserver: Build the apiserver binary
build-apiserver:
	@echo "Building $(APISERVER_BINARY)..."
	@mkdir -p bin
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(APISERVER_PATH) $(CMD_APISERVER)
	@echo "Binary built at $(APISERVER_PATH)"

## build-processor: Build the processor binary
build-processor:
	@echo "Building $(PROCESSOR_BINARY)..."
	@mkdir -p bin
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(PROCESSOR_PATH) $(CMD_PROCESSOR)
	@echo "Binary built at $(PROCESSOR_PATH)"

## build-gc: Build the garbage collector binary
build-gc:
	@echo "Building $(GC_BINARY)..."
	@mkdir -p bin
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(GC_PATH) $(CMD_GC)
	@echo "Binary built at $(GC_PATH)"

## build: Build all binaries
build: build-apiserver build-processor build-gc
	@echo "All binaries built successfully"

## build-release: Build all release binaries for GOOS/GOARCH (e.g. GOOS=linux GOARCH=amd64 make build-release)
build-release:
	@if [ -z "$${GOOS}" ] || [ -z "$${GOARCH}" ]; then \
	  echo "GOOS and GOARCH must be set (e.g. GOOS=linux GOARCH=amd64 make build-release)"; exit 1; \
	fi
	@mkdir -p bin
	@for item in $(RELEASE_BINARIES); do \
	  name=$$(echo $$item | cut -d: -f1); \
	  cmd=$$(echo $$item | cut -d: -f2); \
	  echo "Building batch-gateway-$$name-$${GOOS}-$${GOARCH}..."; \
	  $(GO) build $(GOFLAGS) $(LDFLAGS) -o bin/batch-gateway-$$name-$${GOOS}-$${GOARCH} $$cmd; \
	done
	@echo "Release binaries built successfully"

## package-release: Package binaries as .tar.gz with SHA256SUMS (BINARIES_DIR=dist/binaries RELEASE_DIR=release)
package-release:
	@mkdir -p $(RELEASE_DIR)
	@cp $(BINARIES_DIR)/* $(RELEASE_DIR)/
	@cd $(RELEASE_DIR) && \
	  for f in batch-gateway-*; do \
	    if [ -f "$$f" ]; then \
	      chmod +x "$$f"; \
	      tar czf "$$f.tar.gz" "$$f"; \
	      rm -f "$$f"; \
	    fi; \
	  done && \
	  sha256sum *.tar.gz > SHA256SUMS && \
	  cat SHA256SUMS && \
	  ls -la

## publish-helm-chart: Patch chart for VERSION, package, append chart to SHA256SUMS, push to oci://ghcr.io/llm-d-incubation/charts (requires VERSION, yq, helm; GITHUB_TOKEN, GITHUB_ACTOR for push).
publish-helm-chart:
	@if [ -z "$(VERSION)" ]; then \
	  echo "VERSION is required (e.g. VERSION=v1.0.0 make publish-helm-chart)"; exit 1; \
	fi
	@export VERSION="$(VERSION)"; \
	export GITHUB_TOKEN="$(GITHUB_TOKEN)"; \
	export GITHUB_ACTOR="$(GITHUB_ACTOR)"; \
	./scripts/publish-helm-chart.sh

## generate-release: Create and push a release tag (requires REL_VERSION; optional REL_BRANCH=main|release-vX.Y.Z , default main)
generate-release:
	@if [ -z "$(REL_VERSION)" ]; then \
	  echo "Error: REL_VERSION is required. Example: make generate-release REL_VERSION=0.0.1"; exit 1; \
	fi
	@if [ -n "$(REL_BRANCH)" ]; then \
	  ./scripts/generate-release.sh $(REL_VERSION) $(REL_BRANCH); \
	else \
	  ./scripts/generate-release.sh $(REL_VERSION); \
	fi

## run-apiserver: Run the apiserver
run-apiserver: build-apiserver
	@echo "Starting $(APISERVER_BINARY)..."
	$(APISERVER_PATH)

## run-processor: Run the processor
run-processor: build-processor
	@echo "Starting $(PROCESSOR_BINARY)..."
	$(PROCESSOR_PATH)

## run-apiserver-dev: Run the apiserver with verbose logging
run-apiserver-dev: build-apiserver
	@echo "Starting $(APISERVER_BINARY) in development mode..."
	$(APISERVER_PATH) --v=5

## run-processor-dev: Run the processor with verbose logging
run-processor-dev: build-processor
	@echo "Starting $(PROCESSOR_BINARY) in development mode..."
	$(PROCESSOR_PATH) --v=5

## run-gc: Run the garbage collector
run-gc: build-gc
	@echo "Starting $(GC_BINARY)..."
	$(GC_PATH)

## run-gc-dev: Run the garbage collector with verbose logging
run-gc-dev: build-gc
	@echo "Starting $(GC_BINARY) in development mode..."
	$(GC_PATH) --v=5

## test: Run tests with summary
test:
	@echo "Running tests..."
	@OUT=$$(mktemp); \
	$(GO) test $(TEST_FLAGS) -v ./... 2>&1 | tee $$OUT; \
	TEST_EXIT=$${PIPESTATUS[0]}; \
	PASS_COUNT=$$(grep -- '--- PASS:' $$OUT 2>/dev/null | wc -l | tr -d ' '); \
	FAIL_COUNT=$$(grep -- '--- FAIL:' $$OUT 2>/dev/null | wc -l | tr -d ' '); \
	SKIP_COUNT=$$(grep -- '--- SKIP:' $$OUT 2>/dev/null | wc -l | tr -d ' '); \
	echo ""; \
	echo "========== Test Summary =========="; \
	grep -E "^\s*--- (PASS|FAIL|SKIP):" $$OUT || true; \
	echo ""; \
	echo "Passed: $$PASS_COUNT | Failed: $$FAIL_COUNT | Skipped: $$SKIP_COUNT"; \
	echo ""; \
	if [ $$TEST_EXIT -eq 0 ]; then \
		echo "✅ All tests passed!"; \
	else \
		echo "❌ Tests failed with exit code $$TEST_EXIT"; \
	fi; \
	rm -f $$OUT; \
	exit $$TEST_EXIT

## test-coverage: Run tests with coverage
test-coverage:
	@echo "Running tests with coverage..."
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

## test-coverage-func: Show test coverage by function
test-coverage-func:
	@echo "Running tests with coverage..."
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

## bench: Run all benchmarks
# make bench BENCHTIME=5s (use BENCHTIME=5s to override duration)
bench:
	@echo "Running benchmarks (benchtime=$(BENCHTIME))..."
	$(GO) test -bench=. -benchmem -benchtime=$(BENCHTIME) ./...

## lint: Run golangci-lint
lint:
	@echo "Running linter..."
	@which golangci-lint > /dev/null || (echo "golangci-lint not found. Run 'make install-tools' to install it." && exit 1)
	golangci-lint run ./...

## pre-commit: Run pre-commit on all files
pre-commit:
	@echo "Running pre-commit on all files..."
	@which pre-commit > /dev/null || (echo "pre-commit not found. Install it with: pip install pre-commit" && exit 1)
	pre-commit run --all-files

## fmt: Run go fmt on all files
fmt:
	@echo "Formatting code..."
	$(GO) fmt ./...

## vet: Run go vet
vet:
	@echo "Running go vet..."
	$(GO) vet ./...

## tidy: Run go mod tidy
tidy:
	@echo "Tidying go modules..."
	$(GO) mod tidy

## clean: Remove build artifacts and coverage files
clean:
	@echo "Cleaning..."
	@rm -rf bin/
	@rm -f coverage.out coverage.html
	@echo "Clean complete"

## install-pre-commit-tools: Install tools for pre-commit hooks (goimports, gosec, ruleguard, helm-unittest)
install-pre-commit-tools:
	@echo "Installing pre-commit tools..."
	$(GO) install golang.org/x/tools/cmd/goimports@v0.43.0
	$(GO) install github.com/securego/gosec/v2/cmd/gosec@v2.25.0
	$(GO) install github.com/quasilyte/go-ruleguard/cmd/ruleguard@v0.4.5
	@if command -v helm >/dev/null 2>&1; then \
		helm plugin list | grep -q unittest || \
			helm plugin install https://github.com/helm-unittest/helm-unittest.git || \
			helm plugin install --verify=false https://github.com/helm-unittest/helm-unittest.git; \
	else \
		echo "helm not found, skipping helm-unittest plugin install"; \
	fi
	@echo "Pre-commit tools installed"

## install-tools: Install all development tools (includes pre-commit tools + golangci-lint)
install-tools: install-pre-commit-tools
	@echo "Installing additional development tools..."
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4
	@echo "All tools installed"

## test-helm: Run Helm chart template tests (requires helm-unittest plugin)
test-helm:
	@helm unittest --help >/dev/null 2>&1 || { echo "Error: helm-unittest plugin not installed. Run 'make install-tools' first."; exit 1; }
	@echo "Running Helm chart tests..."
	helm unittest charts/batch-gateway

## check: Run fmt, vet, and test
check: fmt vet test

## check-dco: Check that all commits since main have a DCO Signed-off-by trailer
check-dco:
	@scripts/check-dco.sh

## ci: Run all CI checks
ci: pre-commit check-dco
	@echo "All CI checks passed!"

check-container-tool:
	@command -v $(CONTAINER_TOOL) >/dev/null 2>&1 || { \
	  echo "❌ $(CONTAINER_TOOL) is not installed."; \
	  echo "🔧 Try: sudo apt install $(CONTAINER_TOOL) OR brew install $(CONTAINER_TOOL)"; exit 1; }

## image-build-apiserver: Build apiserver Docker image
image-build-apiserver: check-container-tool
	@printf "\033[33;1m==== Building Docker image $(APISERVER_IMG) ====\033[0m\n"
	$(CONTAINER_TOOL) build \
		--platform linux/$(TARGETARCH) \
		--build-arg TARGETOS=linux \
		--build-arg TARGETARCH=$(TARGETARCH) \
		-f docker/Dockerfile.apiserver \
		-t $(APISERVER_IMG) .

## image-build-processor: Build processor Docker image
image-build-processor: check-container-tool
	@printf "\033[33;1m==== Building Docker image $(PROCESSOR_IMG) ====\033[0m\n"
	$(CONTAINER_TOOL) build \
		--platform linux/$(TARGETARCH) \
		--build-arg TARGETOS=linux \
		--build-arg TARGETARCH=$(TARGETARCH) \
		-f docker/Dockerfile.processor \
		-t $(PROCESSOR_IMG) .

## image-build-gc: Build garbage collector Docker image
image-build-gc: check-container-tool
	@printf "\033[33;1m==== Building Docker image $(GC_IMG) ====\033[0m\n"
	$(CONTAINER_TOOL) build \
		--platform linux/$(TARGETARCH) \
		--build-arg TARGETOS=linux \
		--build-arg TARGETARCH=$(TARGETARCH) \
		-f docker/Dockerfile.gc \
		-t $(GC_IMG) .

## image-build: Build all Docker images
image-build: image-build-apiserver image-build-processor image-build-gc

## deps-get: Download dependencies
deps-get:
	@echo "Downloading dependencies..."
	$(GO) mod download

## deps-verify: Verify dependencies
deps-verify:
	@echo "Verifying dependencies..."
	$(GO) mod verify

## test-integration: Run integration tests (each test spawns its own mock server)
test-integration:
	@echo "Running integration tests..."
	@$(GO) test -v -tags=integration ./... || \
		(echo "\n❌ Integration tests failed" && exit 1)
	@echo "\n✅ Integration tests passed!"

## test-all: Run all tests (unit + integration)
test-all: test test-integration

KIND_CLUSTER_NAME ?= batch-gateway-dev

## dev-deploy: Deploy batch-gateway to a local kind cluster with all dependencies
dev-deploy:
	@bash scripts/dev-deploy.sh

## dev-clean: Clean up dev deployment (removes all resources but keeps the kind cluster)
dev-clean:
	@bash scripts/dev-clean.sh

## dev-rm-cluster: Delete the kind cluster
dev-rm-cluster:
	@echo "Deleting kind cluster 'batch-gateway-dev'..."
	@kind delete cluster --name batch-gateway-dev || echo "Cluster not found or already deleted"
	@echo "✅ Kind cluster deleted"

## test-e2e: Run E2E tests against a live API server (requires TEST_BASE_URL or dev-deploy NodePort services)
##           Use TEST_RUN to filter tests, e.g.: make test-e2e TEST_RUN=TestE2E/Batches/Cancel/InProgress
test-e2e:
	@echo "Running E2E tests..."
	@OUT=$$(mktemp); \
	cd test/e2e && $(GO) test -v -count=1 $(if $(TEST_RUN),-run $(TEST_RUN)) ./... 2>&1 | tee $$OUT; \
	TEST_EXIT=$${PIPESTATUS[0]}; \
	PASS_COUNT=$$(grep -- '--- PASS:' $$OUT 2>/dev/null | wc -l | tr -d ' '); \
	FAIL_COUNT=$$(grep -- '--- FAIL:' $$OUT 2>/dev/null | wc -l | tr -d ' '); \
	SKIP_COUNT=$$(grep -- '--- SKIP:' $$OUT 2>/dev/null | wc -l | tr -d ' '); \
	echo ""; \
	echo "========== E2E Test Summary =========="; \
	grep -E "^\s*--- (PASS|FAIL|SKIP):" $$OUT || true; \
	echo ""; \
	echo "Passed: $$PASS_COUNT | Failed: $$FAIL_COUNT | Skipped: $$SKIP_COUNT"; \
	echo ""; \
	if [ $$TEST_EXIT -eq 0 ]; then \
		echo "✅ All E2E tests passed!"; \
	else \
		echo "❌ E2E tests failed with exit code $$TEST_EXIT"; \
	fi; \
	rm -f $$OUT; \
	exit $$TEST_EXIT
