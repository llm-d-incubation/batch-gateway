# Inference Client

This package provides a generic interface and HTTP implementation for communicating with inference gateways such as llm-d and GAIE.

## Overview

The `InferenceClient` interface provides a simple abstraction for making inference requests to various backends:

```go
type InferenceClient interface {
    Generate(ctx context.Context, req *InferenceRequest) (*InferenceResponse, *InferenceError)
}
```

## HTTP Inference Client

The `HTTPInferenceClient` is an implementation that works with OpenAI-compatible HTTP endpoints, including:
- llm-d inference gateway
- GAIE (via HTTP proxy)
- Any OpenAI-compatible API

### Features

- ✅ OpenAI-compatible API support (`/v1/chat/completions`, `/v1/completions`)
- ✅ Automatic endpoint detection based on request parameters
- ✅ Connection pooling and reuse
- ✅ Configurable timeouts
- ✅ Context-aware cancellation
- ✅ Comprehensive error categorization (rate limit, server errors, auth errors, etc.)
- ✅ **Built-in retry logic with exponential backoff and jitter**
- ✅ Automatic retry on retryable errors (rate limit, server errors)
- ✅ Configurable retry parameters (max retries, backoff settings)
- ✅ Optional API key authentication
- ✅ Request ID tracking

### Quick Start

```go
package main

import (
    "context"
    "fmt"
    "time"

    "github.com/llm-d-incubation/batch-gateway/internal/shared/batch"
)

func main() {
    // Create client
    config := batch.HTTPInferenceClientConfig{
        BaseURL:        "http://localhost:8000",
        Timeout:        5 * time.Minute,
        MaxIdleConns:   100,
        IdleConnTimeout: 90 * time.Second,
        APIKey:         "", // Optional
    }
    client := batch.NewHTTPInferenceClient(config)

    // Prepare request
    req := &batch.InferenceRequest{
        RequestID: "my-request-001",
        Model:     "gpt-4",
        Params: map[string]interface{}{
            "model": "gpt-4",
            "messages": []map[string]interface{}{
                {
                    "role":    "user",
                    "content": "Hello, how are you?",
                },
            },
            "temperature": 0.7,
            "max_tokens":  100,
        },
    }

    // Make inference call
    ctx := context.Background()
    resp, err := client.Generate(ctx, req)
    if err != nil {
        if err.IsRetryable() {
            fmt.Printf("Retryable error: %s\n", err.Message)
            // Implement retry logic
        } else {
            fmt.Printf("Fatal error: %s\n", err.Message)
        }
        return
    }

    // Use response
    fmt.Printf("Response: %s\n", string(resp.Response))
}
```

### Configuration

The `HTTPInferenceClientConfig` struct supports the following fields:

#### HTTP Client Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `BaseURL` | `string` | Required | Base URL of the inference gateway (e.g., `http://localhost:8000`) |
| `Timeout` | `time.Duration` | `5 minutes` | Maximum time for a request to complete |
| `MaxIdleConns` | `int` | `100` | Maximum number of idle connections in the pool |
| `IdleConnTimeout` | `time.Duration` | `90 seconds` | How long an idle connection remains in the pool |
| `APIKey` | `string` | `""` | Optional API key for authentication (sent as `Bearer` token) |

#### Retry Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `MaxRetries` | `int` | `0` (disabled) | Maximum number of retry attempts (set > 0 to enable) |
| `InitialBackoff` | `time.Duration` | `1 second` | Initial backoff duration for first retry |
| `MaxBackoff` | `time.Duration` | `60 seconds` | Maximum backoff duration between retries |
| `BackoffFactor` | `float64` | `2.0` | Exponential backoff multiplier |
| `JitterFraction` | `float64` | `0.1` (10%) | Random jitter as fraction of backoff (prevents thundering herd) |

**Retry Behavior:**
- Automatically retries on **rate limit errors (429)** and **server errors (5xx)**
- Does NOT retry on **client errors (4xx)** except rate limits
- Uses exponential backoff formula: `backoff = min(initial * factor^attempt, maxBackoff) * (1 ± jitter)`
- Respects context cancellation during retry backoff
- Logs retry attempts at verbosity level 3

### Retry with Exponential Backoff

The client includes built-in retry logic with exponential backoff:

```go
// Enable retry with default settings
config := batch.HTTPInferenceClientConfig{
    BaseURL:    "http://localhost:8000",
    MaxRetries: 3, // Enable retry (default backoff settings apply)
}
client := batch.NewHTTPInferenceClient(config)

// The client will automatically retry on rate limits and server errors
resp, err := client.Generate(ctx, req)
// Backoff timeline (with defaults):
// Attempt 1: immediate
// Attempt 2: ~1s backoff  (1s * 2^0 ± 10%)
// Attempt 3: ~2s backoff  (1s * 2^1 ± 10%)
// Attempt 4: ~4s backoff  (1s * 2^2 ± 10%)
```

#### Custom Retry Configuration

```go
// For rate-limited APIs, use aggressive backoff
config := batch.HTTPInferenceClientConfig{
    BaseURL:        "http://api.rate-limited.com",
    MaxRetries:     5,               // More retries
    InitialBackoff: 5 * time.Second, // Longer initial wait
    MaxBackoff:     5 * time.Minute, // Allow longer waits
    BackoffFactor:  3.0,             // More aggressive exponential growth
    JitterFraction: 0.2,             // More jitter (±20%)
}
client := batch.NewHTTPInferenceClient(config)
```

#### Disable Retry

```go
// Set MaxRetries to 0 to disable retry
config := batch.HTTPInferenceClientConfig{
    BaseURL:    "http://localhost:8000",
    MaxRetries: 0, // No retry
}
```

### Endpoint Detection

The client automatically detects which endpoint to use based on request parameters:

- **Chat Completions** (`/v1/chat/completions`): Used when `messages` field is present
- **Text Completions** (`/v1/completions`): Used when `prompt` field is present
- **Default**: Chat completions

### Error Handling

All errors are returned as `*InferenceError` with categorization:

```go
type ErrorCategory string

const (
    ErrCategoryRateLimit  ErrorCategory = "RATE_LIMIT"   // HTTP 429 - retryable
    ErrCategoryServer     ErrorCategory = "SERVER_ERROR" // HTTP 5xx - retryable
    ErrCategoryInvalidReq ErrorCategory = "INVALID_REQ"  // HTTP 400 - not retryable
    ErrCategoryAuth       ErrorCategory = "AUTH_ERROR"   // HTTP 401/403 - not retryable
    ErrCategoryUnknown    ErrorCategory = "UNKNOWN"      // Other - not retryable
)
```

#### Error Handling Example

```go
resp, err := client.Generate(ctx, req)
if err != nil {
    switch err.Category {
    case batch.ErrCategoryRateLimit:
        // Implement exponential backoff
        time.Sleep(time.Second)
        // Retry...
    case batch.ErrCategoryServer:
        // Server error - safe to retry
        // Retry...
    case batch.ErrCategoryAuth:
        // Authentication failed - check credentials
        log.Fatal("Invalid API key")
    case batch.ErrCategoryInvalidReq:
        // Bad request - fix the request
        log.Printf("Invalid request: %s", err.Message)
    default:
        // Unknown error
        log.Printf("Error: %s", err.Message)
    }
}
```

#### Retry Detection

Use the `IsRetryable()` method to determine if an error can be retried:

```go
if err != nil && err.IsRetryable() {
    // Safe to retry
    time.Sleep(backoff)
    // Retry the request
}
```

### Context Support

The client respects context cancellation and deadlines:

```go
// With timeout
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
resp, err := client.Generate(ctx, req)

// With cancellation
ctx, cancel := context.WithCancel(context.Background())
go func() {
    // Cancel after some condition
    time.Sleep(10 * time.Second)
    cancel()
}()
resp, err := client.Generate(ctx, req)
```

### Request Examples

#### Chat Completion

```go
req := &batch.InferenceRequest{
    RequestID: "chat-001",
    Model:     "gpt-4",
    Params: map[string]interface{}{
        "model": "gpt-4",
        "messages": []map[string]interface{}{
            {
                "role":    "system",
                "content": "You are a helpful assistant.",
            },
            {
                "role":    "user",
                "content": "What is the capital of France?",
            },
        },
        "temperature": 0.7,
        "max_tokens":  100,
    },
}
```

#### Text Completion

```go
req := &batch.InferenceRequest{
    RequestID: "completion-001",
    Model:     "gpt-3.5-turbo",
    Params: map[string]interface{}{
        "model":       "gpt-3.5-turbo",
        "prompt":      "Once upon a time",
        "max_tokens":  50,
        "temperature": 0.8,
    },
}
```

#### Tool/Function Calls

```go
req := &batch.InferenceRequest{
    RequestID: "tool-001",
    Model:     "gpt-4",
    Params: map[string]interface{}{
        "model": "gpt-4",
        "messages": []map[string]interface{}{
            {
                "role":    "user",
                "content": "What is the weather like in Boston?",
            },
        },
        "tools": []map[string]interface{}{
            {
                "type": "function",
                "function": map[string]interface{}{
                    "name":        "get_current_weather",
                    "description": "Get the current weather",
                    "parameters": map[string]interface{}{
                        "type": "object",
                        "properties": map[string]interface{}{
                            "location": map[string]interface{}{
                                "type": "string",
                                "description": "City and state",
                            },
                        },
                        "required": []string{"location"},
                    },
                },
            },
        },
        "tool_choice": "auto",
    },
}
```

### Processor Integration

The client is designed to be used with the batch processor. Add the following to your processor configuration:

```yaml
# config.yaml
inference_gateway_url: "http://localhost:8000"
inference_request_timeout: 5m
inference_api_key: ""  # Optional

# Retry configuration
inference_max_retries: 3           # Maximum retry attempts
inference_initial_backoff: 1s      # Initial backoff duration
inference_max_backoff: 60s         # Maximum backoff cap
inference_backoff_factor: 2.0      # Exponential multiplier (optional, default: 2.0)
inference_jitter_fraction: 0.1     # Jitter percentage (optional, default: 0.1)
```

Then in your processor code:

```go
import (
    "github.com/llm-d-incubation/batch-gateway/internal/processor/config"
    "github.com/llm-d-incubation/batch-gateway/internal/shared/batch"
)

func setupInferenceClient(cfg *config.ProcessorConfig) batch.InferenceClient {
    clientConfig := batch.HTTPInferenceClientConfig{
        BaseURL:         cfg.InferenceGatewayURL,
        Timeout:         cfg.InferenceRequestTimeout,
        MaxIdleConns:    100,
        IdleConnTimeout: 90 * time.Second,
        APIKey:          cfg.InferenceAPIKey,

        // Retry configuration (all configurable via YAML)
        MaxRetries:      cfg.InferenceMaxRetries,
        InitialBackoff:  cfg.InferenceInitialBackoff,
        MaxBackoff:      cfg.InferenceMaxBackoff,
        BackoffFactor:   cfg.InferenceBackoffFactor,
        JitterFraction:  cfg.InferenceJitterFraction,
    }
    return batch.NewHTTPInferenceClient(clientConfig)
}
```

### Testing

Run the tests:

```bash
cd internal/shared/batch
go test -v -run TestHTTPInferenceClient \
    http_inference_client_test.go \
    http_inference_client.go \
    client_errors.go \
    inference_client.go
```

See `examples_test.go` for more usage examples.

## Architecture

```
┌─────────────────┐
│  Batch Gateway  │
└────────┬────────┘
         │
         │ InferenceClient Interface
         │
         ▼
┌──────────────────────┐
│ HTTPInferenceClient  │
└──────────┬───────────┘
           │
           │ HTTP POST
           │
    ┌──────┴───────────────────┐
    │                          │
    ▼                          ▼
┌─────────┐            ┌──────────────┐
│  llm-d  │            │     GAIE     │
│ Gateway │            │   Gateway    │
└─────────┘            └──────────────┘
```

## Future Enhancements

- [ ] gRPC client for native GAIE support
- [ ] Streaming support for real-time responses
- [x] ~~Built-in retry logic with exponential backoff~~ ✅ Implemented!
- [ ] Metrics and observability (Prometheus)
- [ ] Circuit breaker pattern
- [ ] Response caching
- [ ] Request/response middleware hooks
- [ ] Per-request retry override
