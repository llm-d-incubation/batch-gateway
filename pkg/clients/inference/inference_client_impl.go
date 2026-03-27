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

package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-logr/logr"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	httpclient "github.com/llm-d-incubation/batch-gateway/pkg/clients/http"
)

// Compile-time check: InferenceHTTPClient implements InferenceClient.
var _ InferenceClient = (*InferenceHTTPClient)(nil)

// InferenceHTTPClient wraps the generic HTTP client and implements InferenceClient interface
type InferenceHTTPClient struct {
	client *httpclient.HTTPClient
}

// NewInferenceClient creates a new HTTP-based inference client
func NewInferenceClient(config *HTTPClientConfig, logger logr.Logger) (*InferenceHTTPClient, error) {
	client, err := httpclient.NewHTTPClient(*config, logger)
	if err != nil {
		return nil, err
	}
	return &InferenceHTTPClient{client: client}, nil
}

// Generate makes an inference request with automatic retry logic
func (c *InferenceHTTPClient) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, *ClientError) {
	logger := logr.FromContextOrDiscard(ctx)

	if req == nil {
		return nil, &ClientError{
			Category: httpclient.ErrCategoryInvalidReq,
			Message:  "request cannot be nil",
		}
	}

	// Use endpoint from request (provided by caller from batch.Request.EndPoint)
	endpoint := req.Endpoint
	if endpoint == "" {
		return nil, &ClientError{
			Category: httpclient.ErrCategoryInvalidReq,
			Message:  "endpoint cannot be empty",
			RawError: nil,
		}
	}

	// Extract model from params for logging
	model := ""
	if m, ok := req.Params["model"]; ok {
		if modelStr, ok := m.(string); ok {
			model = modelStr
		}
	}
	logger.V(logging.TRACE).Info("Sending inference request", "endpoint", endpoint, "request_id", req.RequestID, "model", model)

	// Execute HTTP POST request using the underlying http client
	resp, statusCode, err := c.client.Post(ctx, endpoint, req.Params, req.Headers, req.RequestID)

	// Handle request-level errors (network, timeout, etc.)
	if err != nil {
		return c.handleRequestError(ctx, err, req)
	}

	// Check for non-retryable errors after all retries exhausted
	if statusCode != http.StatusOK {
		return nil, c.client.HandleErrorResponse(ctx, statusCode, resp)
	}

	// Parse response body
	var rawData interface{}
	if len(resp) > 0 {
		if jsonErr := json.Unmarshal(resp, &rawData); jsonErr != nil {
			logger.Info("Failed to unmarshal response as JSON", "request_id", req.RequestID, "error", jsonErr)
			rawData = nil
		}
	}

	if logger.V(logging.TRACE).Enabled() {
		promptTokens, completionTokens, totalTokens := extractUsage(rawData)
		logger.V(logging.TRACE).Info("Received successful response",
			"request_id", req.RequestID,
			"status", statusCode,
			"body_size", len(resp),
			"prompt_tokens", promptTokens,
			"completion_tokens", completionTokens,
			"total_tokens", totalTokens,
		)
	}

	return &GenerateResponse{
		RequestID: req.RequestID,
		Response:  resp,
		RawData:   rawData,
	}, nil
}

// handleRequestError processes request-level errors (network, timeout, cancellation)
func (c *InferenceHTTPClient) handleRequestError(ctx context.Context, err error, req *GenerateRequest) (*GenerateResponse, *ClientError) {
	logger := logr.FromContextOrDiscard(ctx)

	if errors.Is(ctx.Err(), context.Canceled) {
		logger.V(logging.DEBUG).Info("Request cancelled", "request_id", req.RequestID)
		return nil, &ClientError{
			Category: httpclient.ErrCategoryUnknown,
			Message:  "request cancelled",
			RawError: err,
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		logger.V(logging.DEBUG).Info("Request timeout", "request_id", req.RequestID)
		return nil, &ClientError{
			Category: httpclient.ErrCategoryServer,
			Message:  "request timeout",
			RawError: err,
		}
	}

	logger.V(logging.DEBUG).Info("Request failed with network error", "request_id", req.RequestID, "error", err)
	return nil, &ClientError{
		Category: httpclient.ErrCategoryServer,
		Message:  fmt.Sprintf("failed to execute request: %v", err),
		RawError: err,
	}
}

// extractUsage pulls prompt/completion/total token counts from a parsed JSON response body.
// Returns nil for any field not present.
func extractUsage(rawData interface{}) (promptTokens, completionTokens, totalTokens interface{}) {
	if m, ok := rawData.(map[string]interface{}); ok {
		if usage, ok := m["usage"].(map[string]interface{}); ok {
			return usage["prompt_tokens"], usage["completion_tokens"], usage["total_tokens"]
		}
	}
	return nil, nil, nil
}
