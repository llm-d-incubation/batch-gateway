.PHONY: help build build-apiserver build-processor run-apiserver run-processor test test-verbose test-coverage clean lint fmt vet install-tools

# Variables
APISERVER_BINARY=batch-gateway-apiserver
PROCESSOR_BINARY=batch-gateway-processor
APISERVER_PATH=./bin/$(APISERVER_BINARY)
PROCESSOR_PATH=./bin/$(PROCESSOR_BINARY)
CMD_APISERVER=./cmd/apiserver
CMD_PROCESSOR=./cmd/processor
GO=go
GOFLAGS=
LDFLAGS=-ldflags "-s -w"

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

## test: Run all tests
test:
	@echo "Running tests..."
	$(GO) test -v ./...

## test-short: Run tests with -short flag
test-short:
	@echo "Running short tests..."
	$(GO) test -short -v ./...

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

## test-race: Run tests with race detector
test-race:
	@echo "Running tests with race detector..."
	$(GO) test -race -v ./...

## bench: Run benchmarks
bench:
	@echo "Running benchmarks..."
	$(GO) test -bench=. -benchmem ./...

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

## check: Run fmt, vet, and test-race
check: fmt vet test-race

## ci: Run all CI checks (fmt, vet, lint, test-race)
ci: fmt vet lint test-race
	@echo "All CI checks passed!"

## image-build-apiserver: Build apiserver Docker image
image-build-apiserver:
	@echo "Building apiserver Docker image..."
	docker build -t $(APISERVER_BINARY):latest -f Dockerfile.apiserver .

## image-build-processor: Build processor Docker image
image-build-processor:
	@echo "Building processor Docker image..."
	docker build -t $(PROCESSOR_BINARY):latest -f Dockerfile.processor .

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
