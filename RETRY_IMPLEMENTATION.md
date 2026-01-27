# Retry Logic Implementation Summary

## Overview

Added built-in retry logic with exponential backoff and jitter to the HTTP inference client. The retry mechanism automatically handles transient failures like rate limits and server errors.

## What Was Added

### 1. Retry Configuration

Added new `RetryConfig` struct and fields to `HTTPInferenceClientConfig`:

```go
type RetryConfig struct {
    MaxRetries     int           // Maximum retry attempts (default: 3)
    InitialBackoff time.Duration // Initial backoff (default: 1 second)
    MaxBackoff     time.Duration // Max backoff (default: 60 seconds)
    BackoffFactor  float64       // Exponential factor (default: 2.0)
    JitterFraction float64       // Jitter percentage (default: 0.1 = 10%)
}
```

### 2. Exponential Backoff Algorithm

Implemented exponential backoff with jitter:

**Formula**: `backoff = min(initial × factor^attempt, maxBackoff) × (1 ± jitter)`

**Example with defaults** (initial=1s, factor=2.0, jitter=10%):
- Attempt 1: immediate
- Attempt 2: ~1s (1s × 2^0 ± 10%)
- Attempt 3: ~2s (1s × 2^1 ± 10%)
- Attempt 4: ~4s (1s × 2^2 ± 10%)

**Why jitter?** Prevents "thundering herd" problem where many clients retry simultaneously.

### 3. Smart Retry Logic

The client automatically determines when to retry:

| Error Type | HTTP Status | Retry? | Reason |
|------------|-------------|--------|---------|
| Rate Limit | 429 | ✅ Yes | Temporary, will resolve |
| Server Error | 500, 502, 503, 504 | ✅ Yes | Temporary, may recover |
| Bad Request | 400 | ❌ No | Client error, won't change |
| Auth Error | 401, 403 | ❌ No | Credentials invalid |
| Other 4xx | 4xx | ❌ No | Client error |

### 4. Context Awareness

- Respects `context.Context` cancellation during retry
- Stops retrying if context is cancelled or times out
- Allows graceful shutdown

## Files Modified

### Core Implementation
1. **`http_inference_client.go`** - Added retry logic
   - New `RetryConfig` struct
   - Updated `HTTPInferenceClientConfig` with retry fields
   - Split `Generate()` into retry wrapper + `generateOnce()`
   - Added `calculateBackoff()` method
   - Added imports: `math`, `math/rand`

### Tests
2. **`http_inference_client_test.go`** - Added 8 new tests
   - ✅ Retry on rate limit error
   - ✅ Retry on server error
   - ✅ No retry on bad request error
   - ✅ No retry on auth error
   - ✅ Respect max retries
   - ✅ Stop retrying when context cancelled
   - ✅ Work without retry when disabled
   - ✅ Apply exponential backoff

### Examples
3. **`examples_test.go`** - Added 2 new examples
   - `ExampleHTTPInferenceClient_withRetry` - Basic retry usage
   - `ExampleHTTPInferenceClient_customBackoff` - Custom configuration

### Documentation
4. **`README.md`** - Updated documentation
   - Added retry configuration table
   - Added retry behavior description
   - Added retry examples (enable, custom, disable)
   - Updated feature list
   - Updated processor integration example
   - Marked retry as implemented in future enhancements

### Configuration
5. **`internal/processor/config/config.go`** - Added retry config fields
   - `InferenceMaxRetries` (default: 3)
   - `InferenceInitialBackoff` (default: 1s)
   - `InferenceMaxBackoff` (default: 60s)
   - `InferenceBackoffFactor` (default: 2.0)
   - `InferenceJitterFraction` (default: 0.1)

## Test Results

```
✅ 23 Passed | 0 Failed | 0 Pending | 0 Skipped
```

All tests pass, including:
- 15 original tests
- 8 new retry logic tests

## Usage Examples

### Basic Usage (Enable Retry)

```go
config := batch.HTTPInferenceClientConfig{
    BaseURL:    "http://localhost:8000",
    MaxRetries: 3, // Enable retry with default settings
}
client := batch.NewHTTPInferenceClient(config)
```

### Custom Retry Configuration

```go
config := batch.HTTPInferenceClientConfig{
    BaseURL:        "http://api.example.com",
    MaxRetries:     5,
    InitialBackoff: 2 * time.Second,
    MaxBackoff:     5 * time.Minute,
    BackoffFactor:  3.0,
    JitterFraction: 0.2,
}
client := batch.NewHTTPInferenceClient(config)
```

### Disable Retry

```go
config := batch.HTTPInferenceClientConfig{
    BaseURL:    "http://localhost:8000",
    MaxRetries: 0, // Disabled (default)
}
client := batch.NewHTTPInferenceClient(config)
```

## Retry Behavior Details

### Retryable Errors

The client automatically retries these errors:

1. **Rate Limit (HTTP 429)**
   - Common when hitting API rate limits
   - Retry with exponential backoff
   - Category: `ErrCategoryRateLimit`

2. **Server Errors (HTTP 5xx)**
   - 500 Internal Server Error
   - 502 Bad Gateway
   - 503 Service Unavailable
   - 504 Gateway Timeout
   - Category: `ErrCategoryServer`

### Non-Retryable Errors

These errors are returned immediately without retry:

1. **Bad Request (HTTP 400)**
   - Invalid request format
   - Won't succeed even if retried
   - Category: `ErrCategoryInvalidReq`

2. **Authentication Errors (HTTP 401, 403)**
   - Invalid or missing credentials
   - Need to fix API key first
   - Category: `ErrCategoryAuth`

3. **Other Client Errors (HTTP 4xx)**
   - Various client-side errors
   - Category: `ErrCategoryUnknown` or specific

### Logging

Retry attempts are logged at verbosity level 3:

```
I0126 Retrying request_id=req-123 after 1.05s (attempt 1/3, error: RATE_LIMIT)
I0126 Retrying request_id=req-123 after 2.12s (attempt 2/3, error: RATE_LIMIT)
I0126 Request succeeded after 2 retries for request_id=req-123
```

Enable with: `klog.V(3)`

## Configuration File Example

Add to your `config.yaml`:

```yaml
# Inference gateway configuration
inference_gateway_url: "http://llm-d-gateway:8000"
inference_request_timeout: 5m
inference_api_key: "sk-your-api-key"

# Retry configuration
inference_max_retries: 3            # Retry up to 3 times
inference_initial_backoff: 1s       # Start with 1 second
inference_max_backoff: 60s          # Cap at 60 seconds
inference_backoff_factor: 2.0       # Double each retry (optional, default: 2.0)
inference_jitter_fraction: 0.1      # ±10% jitter (optional, default: 0.1)

# Existing processor config...
num_workers: 1
max_job_concurrency: 10
```

## Exponential Backoff Math

### Default Configuration (factor=2.0, initial=1s, max=60s)

| Attempt | Calculation | Backoff (no jitter) | With ±10% Jitter |
|---------|-------------|---------------------|------------------|
| 1 | Immediate | 0s | 0s |
| 2 | 1s × 2^0 | 1s | 0.9s - 1.1s |
| 3 | 1s × 2^1 | 2s | 1.8s - 2.2s |
| 4 | 1s × 2^2 | 4s | 3.6s - 4.4s |
| 5 | 1s × 2^3 | 8s | 7.2s - 8.8s |

### Aggressive Configuration (factor=3.0, initial=5s, max=300s)

| Attempt | Calculation | Backoff (no jitter) | With ±20% Jitter |
|---------|-------------|---------------------|------------------|
| 1 | Immediate | 0s | 0s |
| 2 | 5s × 3^0 | 5s | 4s - 6s |
| 3 | 5s × 3^1 | 15s | 12s - 18s |
| 4 | 5s × 3^2 | 45s | 36s - 54s |
| 5 | 5s × 3^3 | 135s | 108s - 162s |
| 6 | 5s × 3^4 | 405s → 300s (capped) | 240s - 360s |

## Benefits

1. **Automatic Recovery**: Handles transient failures without manual intervention
2. **Configurable**: Tune retry behavior for different workloads
3. **Smart**: Only retries errors that make sense to retry
4. **Respectful**: Uses exponential backoff to avoid overwhelming servers
5. **Jitter**: Prevents synchronized retry storms
6. **Context-Aware**: Respects cancellation and timeouts
7. **Visible**: Logs retry attempts for debugging

## Performance Considerations

### With Retry Enabled

**Pros:**
- Automatic recovery from transient failures
- Better success rate for batch jobs
- No need for manual retry logic

**Cons:**
- Longer total request time on failures
- More load on failing servers (mitigated by backoff)
- Resource usage during backoff wait

**Best For:**
- Batch processing (non-interactive)
- High-value requests (worth waiting)
- APIs with known rate limits

### Without Retry

**Pros:**
- Faster failure detection
- Lower resource usage
- Simpler behavior

**Best For:**
- Interactive/real-time requests
- Quick health checks
- When implementing custom retry logic

## Migration Guide

### For Existing Code

**Before (no retry):**
```go
config := batch.HTTPInferenceClientConfig{
    BaseURL: "http://localhost:8000",
}
client := batch.NewHTTPInferenceClient(config)
```

**After (with retry, backward compatible):**
```go
config := batch.HTTPInferenceClientConfig{
    BaseURL:    "http://localhost:8000",
    MaxRetries: 3, // Add this line to enable retry
}
client := batch.NewHTTPInferenceClient(config)
```

**Note**: Default behavior unchanged - retry is **disabled** unless `MaxRetries > 0`.

## Summary

✅ **Implementation Complete**
- Built-in retry with exponential backoff
- Smart error detection (retry only when appropriate)
- Fully tested (23 tests passing)
- Well documented
- Backward compatible (retry disabled by default)
- Production ready

The HTTP inference client now provides enterprise-grade reliability with automatic retry capabilities, making batch processing more robust against transient failures.
