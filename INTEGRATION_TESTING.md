# Integration Testing Guide

This document explains how to run integration tests for the HTTP inference client using the llm-d-inference-sim mock server.

## Quick Start

```bash
# Run all integration tests (automatically starts/stops mock server)
make test-integration

# Or run all tests (unit + integration)
make test-all
```

That's it! The Makefile handles everything:
- Pulls the llm-d-inference-sim Docker image
- Starts the mock server on port 8100
- Waits for it to be ready
- Runs the integration tests
- Stops and cleans up the mock server

## Requirements

- **Docker** or **Podman**: Required to run the mock server container
- **curl**: Used to check if server is ready
- **Go 1.24+**: To run the tests

## Available Commands

### Run Integration Tests

```bash
# Run integration tests (full workflow)
make test-integration
```

This will:
1. Start llm-d-inference-sim in Docker
2. Wait for it to be healthy
3. Run integration tests
4. Stop and remove the container

### Manual Control

Start the mock server manually:
```bash
make test-integration-up
```

Run tests against running server:
```bash
go test -v -tags=integration ./internal/shared/batch/... -run TestHTTPInferenceClientIntegration
```

Stop the mock server:
```bash
make test-integration-down
```

View mock server logs:
```bash
make test-integration-logs
```

## What Gets Tested

The integration tests validate:

### ✅ Basic Inference
- Text completion requests (`/v1/completions`)
- Chat completion requests (`/v1/chat/completions`)
- Response structure validation
- Multiple sequential requests

### ✅ Retry Logic
- Retry configuration application
- Disabled retry mode (MaxRetries=0)

### ✅ Context Handling
- Context timeout respect
- Context cancellation

### ✅ Configuration
- Custom retry parameters (BackoffFactor, JitterFraction, etc.)
- Full configuration validation

## Test Configuration

The integration tests use:

| Setting | Value |
|---------|-------|
| Mock Server Image | `ghcr.io/llm-d/llm-d-inference-sim:latest` |
| Mock Server Port | 8100 (mapped from container's 8000) |
| Mock Server Model | `fake-model` |
| Mock Server Mode | `random` (returns random responses) |

## Docker Compose Configuration

See `docker-compose.test.yml`:

```yaml
services:
  llm-d-mock-server:
    image: ghcr.io/llm-d/llm-d-inference-sim:latest
    ports:
      - "8100:8000"
    command:
      - --port=8000
      - --model=fake-model
      - --mode=random
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8000/health"]
      interval: 2s
      timeout: 1s
      retries: 10
```

## CI/CD Integration

### GitHub Actions Example

```yaml
name: Integration Tests

on: [push, pull_request]

jobs:
  integration:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3

      - name: Set up Go
        uses: actions/setup-go@v4
        with:
          go-version: '1.24'

      - name: Run integration tests
        run: make test-integration
```

The Docker image will be automatically pulled from GitHub Container Registry.

## Skipping Integration Tests

Set the environment variable to skip integration tests:

```bash
SKIP_INTEGRATION_TESTS=true go test -v -tags=integration ./internal/shared/batch/...
```

This is useful in environments where Docker is not available.

## Troubleshooting

### "Mock server not running" Error

**Problem**: Tests skip because mock server isn't detected

**Solution**: Make sure Docker is running and start the server:
```bash
make test-integration-up
```

### "Mock server failed to start" Error

**Problem**: Port 8100 already in use

**Solution**: Stop any process using port 8100:
```bash
# Find process
lsof -i :8100

# Stop the container if it's running
make test-integration-down

# Or change the port in docker-compose.test.yml
```

### Docker Image Pull Issues

**Problem**: Can't pull `ghcr.io/llm-d/llm-d-inference-sim:latest`

**Solution**: Check if image exists or use a specific version:
```yaml
# In docker-compose.test.yml, change to a specific version
image: ghcr.io/llm-d/llm-d-inference-sim:v0.1.0
```

### Tests Timing Out

**Problem**: Tests take too long or timeout

**Solution**: Increase timeout in test configuration or check mock server logs:
```bash
make test-integration-logs
```

## Advanced: Custom Mock Server Configuration

You can modify `docker-compose.test.yml` to test different scenarios:

### With Latency Simulation
```yaml
command:
  - --port=8000
  - --model=fake-model
  - --mode=random
  - --time-to-first-token=200ms
  - --inter-token-latency=50ms
```

### With Failure Injection
```yaml
command:
  - --port=8000
  - --model=fake-model
  - --mode=random
  - --failure-injection-rate=50
  - --failure-types=server_error
```

### With LoRA Adapters
```yaml
command:
  - --port=8000
  - --model=fake-model
  - --lora-modules={"name":"adapter-1"}
```

See the [llm-d-inference-sim documentation](https://github.com/llm-d/llm-d-inference-sim) for all available options.

## Comparison: Unit vs Integration Tests

| Aspect | Unit Tests | Integration Tests |
|--------|------------|-------------------|
| **What** | Client logic, retry algorithm | End-to-end HTTP requests |
| **Speed** | Fast (~2-3 seconds) | Slower (~10-15 seconds with Docker) |
| **Dependencies** | None (uses httptest) | Requires Docker |
| **Run by** | `make test` | `make test-integration` |
| **When** | Always (every commit) | Pre-merge, CI/CD |

Both are important for comprehensive testing!

## Examples

### Run just basic inference tests
```bash
go test -v -tags=integration ./internal/shared/batch/... -run "TestHTTPInferenceClientIntegration/Basic"
```

### Run with verbose Ginkgo output
```bash
go test -v -tags=integration ./internal/shared/batch/... -ginkgo.v -run TestHTTPInferenceClientIntegration
```

### Run against a custom mock server
```bash
# Start your own mock server on a different port
docker run -d -p 9000:8000 ghcr.io/llm-d/llm-d-inference-sim:latest \
  --port 8000 --model custom-model

# Update test to use it (modify mockServerURL in test file)
# Or set environment variable if supported
```

## Summary

✅ **Easy to run**: `make test-integration`
✅ **Self-contained**: Docker handles everything
✅ **Portable**: Works anywhere Docker works
✅ **No external dependencies**: Uses official Docker image
✅ **CI-ready**: Perfect for GitHub Actions, GitLab CI, etc.
