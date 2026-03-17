.PHONY: help build build-apiserver build-processor run-apiserver run-processor run-apiserver-dev run-processor-dev test test-coverage test-coverage-func clean lint fmt vet tidy install-tools deps-get deps-verify bench check check-container-tool ci image-build image-build-apiserver image-build-processor test-integration test-all test-e2e dev-deploy dev-clean dev-rm-cluster

SHELL := /usr/bin/env bash

TARGETARCH ?= $(shell go env GOARCH)

# Variables
DEV_VERSION ?= 0.0.1
APISERVER_BINARY=batch-gateway-apiserver
PROCESSOR_BINARY=batch-gateway-processor
APISERVER_PATH=./bin/$(APISERVER_BINARY)
PROCESSOR_PATH=./bin/$(PROCESSOR_BINARY)
CMD_APISERVER=./cmd/apiserver
CMD_PROCESSOR=./cmd/batch-processor
APISERVER_IMAGE_TAG_BASE ?= ghcr.io/llm-d-incubation/$(APISERVER_BINARY)
APISERVER_IMG = $(APISERVER_IMAGE_TAG_BASE):$(DEV_VERSION)
PROCESSOR_IMAGE_TAG_BASE ?= ghcr.io/llm-d-incubation/$(PROCESSOR_BINARY)
PROCESSOR_IMG = $(PROCESSOR_IMAGE_TAG_BASE):$(DEV_VERSION)
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

## build: Build all binaries
build: build-apiserver build-processor
	@echo "All binaries built successfully"

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

## install-tools: Install development tools
install-tools:
	@echo "Installing development tools..."
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	@echo "Tools installed"

## check: Run fmt, vet, and test
check: fmt vet test

## ci: Run all CI checks (fmt, vet, lint, test)
ci: fmt vet lint test
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

## image-build: Build all Docker images
image-build: image-build-apiserver image-build-processor

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
	@$(GO) test -v -tags=integration ./internal/inference/... || \
		(echo "\n❌ Integration tests failed" && exit 1)
	@echo "\n✅ Integration tests passed!"

## test-all: Run all tests (unit + integration)
test-all: test test-integration

## deploy: Deploy batch-gateway to a local kind cluster and start port-forward
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

## test-e2e: Run E2E tests against a live API server (requires TEST_BASE_URL or port-forward)
test-e2e:
	@echo "Ensuring port forwards are active..."
	@bash scripts/ensure-port-forwards.sh
	@echo "Running E2E tests..."
	@OUT=$$(mktemp); \
	cd test/e2e && $(GO) test -v -count=1 ./... 2>&1 | tee $$OUT; \
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
