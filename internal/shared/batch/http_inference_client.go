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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"time"

	"k8s.io/klog/v2"
)

// HTTPInferenceClient implements InferenceClient interface for HTTP-based inference gateways
// Supports both llm-d (OpenAI-compatible) and GAIE endpoints
type HTTPInferenceClient struct {
	client       *http.Client
	baseURL      string
	apiKey       string        // optional API key for authentication
	retryConfig  RetryConfig   // retry configuration
}

// RetryConfig holds retry configuration with exponential backoff
type RetryConfig struct {
	MaxRetries     int           // Maximum number of retry attempts (default: 3)
	InitialBackoff time.Duration // Initial backoff duration (default: 1 second)
	MaxBackoff     time.Duration // Maximum backoff duration (default: 60 seconds)
	BackoffFactor  float64       // Backoff multiplier (default: 2.0)
	JitterFraction float64       // Jitter as fraction of backoff (default: 0.1 = 10%)
}

// HTTPInferenceClientConfig holds configuration for the HTTP client
type HTTPInferenceClientConfig struct {
	BaseURL         string        // Base URL of the inference gateway (e.g., "http://localhost:8000")
	Timeout         time.Duration // Request timeout (default: 5 minutes)
	MaxIdleConns    int           // Maximum idle connections (default: 100)
	IdleConnTimeout time.Duration // Idle connection timeout (default: 90 seconds)
	APIKey          string        // Optional API key for authentication

	// Retry configuration (optional, set MaxRetries > 0 to enable)
	MaxRetries     int           // Maximum number of retry attempts (default: 3)
	InitialBackoff time.Duration // Initial backoff duration (default: 1 second)
	MaxBackoff     time.Duration // Maximum backoff duration (default: 60 seconds)
	BackoffFactor  float64       // Backoff multiplier (default: 2.0)
	JitterFraction float64       // Jitter as fraction of backoff (default: 0.1 = 10%)
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
	retryConfig := RetryConfig{
		MaxRetries:     config.MaxRetries,
		InitialBackoff: config.InitialBackoff,
		MaxBackoff:     config.MaxBackoff,
		BackoffFactor:  config.BackoffFactor,
		JitterFraction: config.JitterFraction,
	}

	// Apply retry defaults if MaxRetries is set but other fields are zero
	if retryConfig.MaxRetries > 0 {
		if retryConfig.InitialBackoff == 0 {
			retryConfig.InitialBackoff = 1 * time.Second
		}
		if retryConfig.MaxBackoff == 0 {
			retryConfig.MaxBackoff = 60 * time.Second
		}
		if retryConfig.BackoffFactor == 0 {
			retryConfig.BackoffFactor = 2.0
		}
		if retryConfig.JitterFraction == 0 {
			retryConfig.JitterFraction = 0.1
		}
	}

	// Create HTTP client with custom transport for connection pooling
	transport := &http.Transport{
		MaxIdleConns:        config.MaxIdleConns,
		MaxIdleConnsPerHost: config.MaxIdleConns,
		IdleConnTimeout:     config.IdleConnTimeout,
	}

	return &HTTPInferenceClient{
		client: &http.Client{
			Timeout:   config.Timeout,
			Transport: transport,
		},
		baseURL:     config.BaseURL,
		apiKey:      config.APIKey,
		retryConfig: retryConfig,
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

	// If retry is disabled, make a single request
	if c.retryConfig.MaxRetries == 0 {
		return c.generateOnce(ctx, req)
	}

	// Retry loop with exponential backoff
	var lastErr *InferenceError
	for attempt := 0; attempt <= c.retryConfig.MaxRetries; attempt++ {
		// Make the request
		resp, err := c.generateOnce(ctx, req)
		if err == nil {
			// Success
			if attempt > 0 {
				klog.V(3).Infof("Request succeeded after %d retries for request_id=%s", attempt, req.RequestID)
			}
			return resp, nil
		}

		lastErr = err

		// Check if we should retry
		if !err.IsRetryable() {
			klog.V(3).Infof("Non-retryable error for request_id=%s: %s", req.RequestID, err.Message)
			return nil, err
		}

		// Check if we've exhausted retries
		if attempt >= c.retryConfig.MaxRetries {
			klog.V(3).Infof("Max retries (%d) exhausted for request_id=%s", c.retryConfig.MaxRetries, req.RequestID)
			break
		}

		// Check if context is cancelled
		if ctx.Err() != nil {
			klog.V(3).Infof("Context cancelled, stopping retries for request_id=%s", req.RequestID)
			return nil, err
		}

		// Calculate backoff duration with exponential backoff and jitter
		backoff := c.calculateBackoff(attempt)
		klog.V(3).Infof("Retrying request_id=%s after %v (attempt %d/%d, error: %s)",
			req.RequestID, backoff, attempt+1, c.retryConfig.MaxRetries, err.Category)

		// Wait for backoff duration or until context is cancelled
		select {
		case <-time.After(backoff):
			// Continue to next retry
		case <-ctx.Done():
			klog.V(3).Infof("Context cancelled during backoff for request_id=%s", req.RequestID)
			return nil, &InferenceError{
				Category: ErrCategoryUnknown,
				Message:  "request cancelled during retry",
				RawError: ctx.Err(),
			}
		}
	}

	// All retries exhausted
	return nil, lastErr
}

// generateOnce makes a single inference request without retry logic
func (c *HTTPInferenceClient) generateOnce(ctx context.Context, req *InferenceRequest) (*InferenceResponse, *InferenceError) {

	// Determine endpoint based on request parameters
	endpoint := c.determineEndpoint(req.Params)

	// Marshal request parameters to JSON
	requestBody, err := json.Marshal(req.Params)
	if err != nil {
		return nil, &InferenceError{
			Category: ErrCategoryInvalidReq,
			Message:  fmt.Sprintf("failed to marshal request: %v", err),
			RawError: err,
		}
	}

	// Create HTTP request
	url := c.baseURL + endpoint
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(requestBody))
	if err != nil {
		return nil, &InferenceError{
			Category: ErrCategoryUnknown,
			Message:  fmt.Sprintf("failed to create HTTP request: %v", err),
			RawError: err,
		}
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))
	}
	if req.RequestID != "" {
		httpReq.Header.Set("X-Request-ID", req.RequestID)
	}

	// Log the request
	klog.V(4).Infof("Sending inference request to %s with request_id=%s, model=%s", url, req.RequestID, req.Model)

	// Execute request
	httpResp, err := c.client.Do(httpReq)
	if err != nil {
		// Check if context was cancelled or timed out
		if ctx.Err() == context.Canceled {
			return nil, &InferenceError{
				Category: ErrCategoryUnknown,
				Message:  "request cancelled",
				RawError: err,
			}
		}
		if ctx.Err() == context.DeadlineExceeded {
			return nil, &InferenceError{
				Category: ErrCategoryServer,
				Message:  "request timeout",
				RawError: err,
			}
		}
		return nil, &InferenceError{
			Category: ErrCategoryServer,
			Message:  fmt.Sprintf("failed to execute request: %v", err),
			RawError: err,
		}
	}
	defer httpResp.Body.Close()

	// Read response body
	responseBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, &InferenceError{
			Category: ErrCategoryServer,
			Message:  fmt.Sprintf("failed to read response body: %v", err),
			RawError: err,
		}
	}

	// Check status code
	if httpResp.StatusCode != http.StatusOK {
		return nil, c.handleErrorResponse(httpResp.StatusCode, responseBody)
	}

	// Parse response to extract RawData
	var rawData interface{}
	if err := json.Unmarshal(responseBody, &rawData); err != nil {
		klog.Warningf("Failed to unmarshal response as JSON: %v", err)
		// Continue anyway, just set rawData to nil
		rawData = nil
	}

	klog.V(4).Infof("Received successful response for request_id=%s, status=%d, body_size=%d", req.RequestID, httpResp.StatusCode, len(responseBody))

	return &InferenceResponse{
		RequestID: req.RequestID,
		Response:  responseBody,
		RawData:   rawData,
	}, nil
}

// determineEndpoint determines which endpoint to use based on request parameters
func (c *HTTPInferenceClient) determineEndpoint(params map[string]interface{}) string {
	// Check if messages field exists (indicates chat completion)
	if _, hasMessages := params["messages"]; hasMessages {
		return "/v1/chat/completions"
	}

	// Check if prompt field exists (indicates text completion)
	if _, hasPrompt := params["prompt"]; hasPrompt {
		return "/v1/completions"
	}

	// Default to chat completions
	return "/v1/chat/completions"
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

// calculateBackoff calculates the backoff duration with exponential backoff and jitter
// Formula: backoff = min(initial * factor^attempt, maxBackoff) * (1 ± jitter)
func (c *HTTPInferenceClient) calculateBackoff(attempt int) time.Duration {
	// Calculate exponential backoff: initial * factor^attempt
	backoff := float64(c.retryConfig.InitialBackoff) * math.Pow(c.retryConfig.BackoffFactor, float64(attempt))

	// Cap at max backoff
	if backoff > float64(c.retryConfig.MaxBackoff) {
		backoff = float64(c.retryConfig.MaxBackoff)
	}

	// Add jitter: randomize by ±jitterFraction
	// For example, with jitterFraction=0.1, the backoff will be randomized by ±10%
	jitter := backoff * c.retryConfig.JitterFraction * (rand.Float64()*2 - 1)
	backoff += jitter

	// Ensure backoff is positive
	if backoff < 0 {
		backoff = float64(c.retryConfig.InitialBackoff)
	}

	return time.Duration(backoff)
}
