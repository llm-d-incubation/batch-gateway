/*
Copyright 2026 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package batch

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-resty/resty/v2"
	"k8s.io/klog/v2"
)

// HTTPInferenceClient implements InferenceClient interface for HTTP-based inference gateways
// Supports both llm-d (OpenAI-compatible) and GAIE endpoints
type HTTPInferenceClient struct {
	client *resty.Client
}

// HTTPInferenceClientConfig holds configuration for the HTTP client
type HTTPInferenceClientConfig struct {
	BaseURL         string        // Base URL of the inference gateway (e.g., "http://localhost:8000")
	Timeout         time.Duration // Request timeout (default: 5 minutes)
	MaxIdleConns    int           // Maximum idle connections (default: 100)
	IdleConnTimeout time.Duration // Idle connection timeout (default: 90 seconds)
	APIKey          string        // Optional API key for authentication

	// TLS configuration (optional)
	TLSInsecureSkipVerify bool // Skip TLS certificate verification (default: false - INSECURE, only for testing)

	// Retry configuration (optional, set MaxRetries > 0 to enable)
	// Uses resty's built-in exponential backoff with jitter
	MaxRetries     int           // Maximum number of retry attempts (default: 0 = disabled)
	InitialBackoff time.Duration // Initial/minimum retry wait time (default: 1 second)
	MaxBackoff     time.Duration // Maximum retry wait time (default: 60 seconds)
}

// NewHTTPInferenceClient creates a new HTTP-based inference client
func NewHTTPInferenceClient(config HTTPInferenceClientConfig) *HTTPInferenceClient {
	// Set defaults for HTTP client
	if config.Timeout == 0 {
		config.Timeout = 5 * time.Minute
	}
	if config.MaxIdleConns == 0 {
		config.MaxIdleConns = 100
	}
	if config.IdleConnTimeout == 0 {
		config.IdleConnTimeout = 90 * time.Second
	}

	// Set defaults for retry configuration
	if config.MaxRetries > 0 {
		if config.InitialBackoff == 0 {
			config.InitialBackoff = 1 * time.Second
		}
		if config.MaxBackoff == 0 {
			config.MaxBackoff = 60 * time.Second
		}
	}

	// Create resty client
	client := resty.New().
		SetBaseURL(config.BaseURL).
		SetTimeout(config.Timeout).
		SetHeader("Content-Type", "application/json")

	// Set auth token if provided (adds "Authorization: Bearer <token>" to all requests)
	if config.APIKey != "" {
		client.SetAuthToken(config.APIKey)
	}

	// Configure transport - start with Go's secure defaults (http.DefaultTransport)
	// This gives us: TLS 1.2+, system root CAs, certificate verification, proper timeouts
	transport := http.DefaultTransport.(*http.Transport).Clone()

	// Override only the settings we need to customize for batch processing
	transport.MaxIdleConns = config.MaxIdleConns
	transport.MaxIdleConnsPerHost = config.MaxIdleConns // Higher than default (17) for batch workloads
	transport.IdleConnTimeout = config.IdleConnTimeout
	transport.ResponseHeaderTimeout = 30 * time.Second // Prevent hanging on slow backends

	// Only configure TLS if we need to skip verification (testing only)
	if config.TLSInsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}
	// Otherwise, TLSClientConfig stays nil = Go uses system root CAs + TLS 1.2+ defaults

	client.SetTransport(transport)

	// Configure retry only if enabled
	if config.MaxRetries > 0 {
		client.SetRetryCount(config.MaxRetries).
			SetRetryWaitTime(config.InitialBackoff).    // Min wait time between retries
			SetRetryMaxWaitTime(config.MaxBackoff)      // Max wait time between retries
		// Resty automatically applies exponential backoff with jitter

		// Retry condition: retry on server errors, rate limits, and network errors
		client.AddRetryCondition(func(r *resty.Response, err error) bool {
			if err != nil {
				return true // Retry on network errors
			}

			statusCode := r.StatusCode()
			// Retry on 429 (rate limit) and 5xx (server errors)
			return statusCode == http.StatusTooManyRequests || statusCode >= 500
		})

		// Add retry hook for logging
		client.AddRetryHook(func(resp *resty.Response, err error) {
			if reqID := resp.Request.Header.Get("X-Request-ID"); reqID != "" {
				klog.V(3).Infof("Retrying request_id=%s (attempt %d/%d)",
					reqID, resp.Request.Attempt, config.MaxRetries)
			}
		})
	}

	return &HTTPInferenceClient{
		client: client,
	}
}

// Generate makes an inference request to the HTTP gateway with automatic retry logic
func (c *HTTPInferenceClient) Generate(ctx context.Context, req *InferenceRequest) (*InferenceResponse, *InferenceError) {
	if req == nil {
		return nil, &InferenceError{
			Category: ErrCategoryInvalidReq,
			Message:  "request cannot be nil",
		}
	}

	// Use endpoint from request (provided by caller from batch.Request.EndPoint)
	endpoint := req.Endpoint
	if endpoint == "" {
		return nil, &InferenceError{
			Category: ErrCategoryInvalidReq,
			Message:  "endpoint cannot be empty",
			RawError: nil,
		}
	}

	// Create resty request with context
	restyReq := c.client.R().SetContext(ctx)

	// Set request ID header if provided
	if req.RequestID != "" {
		restyReq.SetHeader("X-Request-ID", req.RequestID)
	}

	// Set request body (resty handles JSON marshaling)
	restyReq.SetBody(req.Params)

	klog.V(4).Infof("Sending inference request to %s with request_id=%s, model=%s",
		endpoint, req.RequestID, req.Model)

	// Execute request (resty handles retries automatically)
	resp, err := restyReq.Post(endpoint)

	// Handle request-level errors (network, timeout, etc.)
	if err != nil {
		return c.handleRequestError(ctx, err, req)
	}

	// Check for non-retryable errors after all retries exhausted
	if resp.StatusCode() != http.StatusOK {
		return nil, c.handleErrorResponse(resp.StatusCode(), resp.Body())
	}

	// Log success with retry info
	if resp.Request.Attempt > 1 {
		klog.V(3).Infof("Request succeeded after %d retries for request_id=%s",
			resp.Request.Attempt-1, req.RequestID)
	}

	// Parse response body
	var rawData interface{}
	if len(resp.Body()) > 0 {
		if jsonErr := json.Unmarshal(resp.Body(), &rawData); jsonErr != nil {
			klog.Warningf("Failed to unmarshal response as JSON for request_id=%s: %v",
				req.RequestID, jsonErr)
			rawData = nil
		}
	}

	klog.V(4).Infof("Received successful response for request_id=%s, status=%d, body_size=%d",
		req.RequestID, resp.StatusCode(), len(resp.Body()))

	return &InferenceResponse{
		RequestID: req.RequestID,
		Response:  resp.Body(),
		RawData:   rawData,
	}, nil
}

// handleRequestError processes request-level errors (network, timeout, cancellation)
func (c *HTTPInferenceClient) handleRequestError(ctx context.Context, err error, req *InferenceRequest) (*InferenceResponse, *InferenceError) {
	if errors.Is(ctx.Err(), context.Canceled) {
		klog.V(3).Infof("Request cancelled for request_id=%s", req.RequestID)
		return nil, &InferenceError{
			Category: ErrCategoryUnknown,
			Message:  "request cancelled",
			RawError: err,
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		klog.V(3).Infof("Request timeout for request_id=%s", req.RequestID)
		return nil, &InferenceError{
			Category: ErrCategoryServer,
			Message:  "request timeout",
			RawError: err,
		}
	}

	klog.V(3).Infof("Request failed with network error for request_id=%s: %v", req.RequestID, err)
	return nil, &InferenceError{
		Category: ErrCategoryServer,
		Message:  fmt.Sprintf("failed to execute request: %v", err),
		RawError: err,
	}
}

// handleErrorResponse parses error response and maps to InferenceError
func (c *HTTPInferenceClient) handleErrorResponse(statusCode int, body []byte) *InferenceError {
	// Try to parse OpenAI-style error response
	var errorResp struct {
		Error struct {
			Code    int    `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
			Param   string `json:"param"`
		} `json:"error"`
	}

	message := string(body)
	if err := json.Unmarshal(body, &errorResp); err == nil && errorResp.Error.Message != "" {
		message = errorResp.Error.Message
	}

	// Map HTTP status codes to error categories
	category := c.mapStatusCodeToCategory(statusCode)

	klog.V(3).Infof("Inference request failed with status=%d, category=%s, message=%s", statusCode, category, message)

	return &InferenceError{
		Category: category,
		Message:  fmt.Sprintf("HTTP %d: %s", statusCode, message),
		RawError: fmt.Errorf("status code: %d, body: %s", statusCode, string(body)),
	}
}

// mapStatusCodeToCategory maps HTTP status codes to error categories
func (c *HTTPInferenceClient) mapStatusCodeToCategory(statusCode int) ErrorCategory {
	switch statusCode {
	case http.StatusBadRequest: // 400
		return ErrCategoryInvalidReq
	case http.StatusUnauthorized, http.StatusForbidden: // 401, 403
		return ErrCategoryAuth
	case http.StatusTooManyRequests: // 429
		return ErrCategoryRateLimit
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout: // 500, 502, 503, 504
		return ErrCategoryServer
	default:
		if statusCode >= 500 {
			return ErrCategoryServer
		}
		return ErrCategoryUnknown
	}
}
