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
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"time"

	"github.com/go-resty/resty/v2"
	"k8s.io/klog/v2"
)

// HTTPInferenceClient implements InferenceClient interface for HTTP-based inference gateways
// Supports both llm-d (OpenAI-compatible) and GAIE endpoints
type HTTPInferenceClient struct {
	client       *resty.Client
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

	// Create resty client
	client := resty.New().
		SetBaseURL(config.BaseURL).
		SetTimeout(config.Timeout).
		SetHeader("Content-Type", "application/json")

	// Configure transport for connection pooling
	transport := &http.Transport{
		MaxIdleConns:        config.MaxIdleConns,
		MaxIdleConnsPerHost: config.MaxIdleConns,
		IdleConnTimeout:     config.IdleConnTimeout,
	}
	client.SetTransport(transport)

	// Configure retry only if enabled
	if retryConfig.MaxRetries > 0 {
		client.SetRetryCount(retryConfig.MaxRetries)

		// Custom retry condition: retry on server errors and rate limits
		client.AddRetryCondition(func(r *resty.Response, err error) bool {
			if err != nil {
				return true // Retry on network errors
			}

			statusCode := r.StatusCode()
			// Retry on 429 (rate limit) and 5xx (server errors)
			return statusCode == http.StatusTooManyRequests || statusCode >= 500
		})
	}

	httpClient := &HTTPInferenceClient{
		client:      client,
		baseURL:     config.BaseURL,
		apiKey:      config.APIKey,
		retryConfig: retryConfig,
	}

	// Set custom backoff strategy if retry is enabled
	if retryConfig.MaxRetries > 0 {
		client.SetRetryAfter(httpClient.customRetryAfter)
	}

	return httpClient
}

// customRetryAfter implements custom exponential backoff with jitter for resty
func (c *HTTPInferenceClient) customRetryAfter(client *resty.Client, resp *resty.Response) (time.Duration, error) {
	// Extract attempt number from resty's internal state
	// Resty counts attempts starting from 1, adjust to 0-based for calculateBackoff
	attempt := resp.Request.Attempt - 1

	backoff := c.calculateBackoff(attempt)

	// Log retry with backoff duration
	if reqID := resp.Request.Header.Get("X-Request-ID"); reqID != "" {
		klog.V(3).Infof("Retrying request_id=%s after %v (attempt %d/%d)",
			reqID, backoff, resp.Request.Attempt, c.retryConfig.MaxRetries)
	}

	return backoff, nil
}

// Generate makes an inference request to the HTTP gateway with automatic retry logic
func (c *HTTPInferenceClient) Generate(ctx context.Context, req *InferenceRequest) (*InferenceResponse, *InferenceError) {
	if req == nil {
		return nil, &InferenceError{
			Category: ErrCategoryInvalidReq,
			Message:  "request cannot be nil",
		}
	}

	// Determine endpoint based on request parameters
	endpoint := c.determineEndpoint(req.Params)

	// Create resty request with context
	restyReq := c.client.R().SetContext(ctx)

	// Set headers
	if c.apiKey != "" {
		restyReq.SetHeader("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))
	}
	if req.RequestID != "" {
		restyReq.SetHeader("X-Request-ID", req.RequestID)
	}

	// Set request body (resty handles JSON marshaling)
	restyReq.SetBody(req.Params)

	klog.V(4).Infof("Sending inference request to %s with request_id=%s, model=%s",
		c.baseURL+endpoint, req.RequestID, req.Model)

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
	// Check if context was cancelled or timed out
	if ctx.Err() == context.Canceled {
		klog.V(3).Infof("Request cancelled for request_id=%s", req.RequestID)
		return nil, &InferenceError{
			Category: ErrCategoryUnknown,
			Message:  "request cancelled",
			RawError: err,
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
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
