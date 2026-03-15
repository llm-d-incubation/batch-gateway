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

	httpclient "github.com/llm-d-incubation/batch-gateway/pkg/clients/http"
	"k8s.io/klog/v2"
)

// Compile-time check: InferenceHTTPClient implements InferenceClient.
var _ InferenceClient = (*InferenceHTTPClient)(nil)

// InferenceHTTPClient wraps the generic HTTP client and implements InferenceClient interface
type InferenceHTTPClient struct {
	client *httpclient.HTTPClient
}

// NewInferenceClient creates a new HTTP-based inference client
func NewInferenceClient(config *HTTPClientConfig) (*InferenceHTTPClient, error) {
	client, err := httpclient.NewHTTPClient(*config)
	if err != nil {
		return nil, err
	}
	return &InferenceHTTPClient{client: client}, nil
}

// Generate makes an inference request with automatic retry logic
func (c *InferenceHTTPClient) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, *ClientError) {
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
	klog.V(4).Infof("Sending inference request to %s with request_id=%s, model=%s",
		endpoint, req.RequestID, model)

	// Execute HTTP POST request using the underlying http client
	resp, statusCode, err := c.client.Post(ctx, endpoint, req.Params, req.Headers, req.RequestID)

	// Handle request-level errors (network, timeout, etc.)
	if err != nil {
		return c.handleRequestError(ctx, err, req)
	}

	// Check for non-retryable errors after all retries exhausted
	if statusCode != http.StatusOK {
		return nil, c.client.HandleErrorResponse(statusCode, resp)
	}

	// Parse response body
	var rawData interface{}
	if len(resp) > 0 {
		if jsonErr := json.Unmarshal(resp, &rawData); jsonErr != nil {
			klog.Warningf("Failed to unmarshal response as JSON for request_id=%s: %v",
				req.RequestID, jsonErr)
			rawData = nil
		}
	}

	if klog.V(4).Enabled() {
		promptTokens, completionTokens, totalTokens := extractUsage(rawData)
		klog.V(4).Infof("Received successful response for request_id=%s, status=%d, body_size=%d, prompt_tokens=%v, completion_tokens=%v, total_tokens=%v",
			req.RequestID, statusCode, len(resp), promptTokens, completionTokens, totalTokens)
	}

	return &GenerateResponse{
		RequestID: req.RequestID,
		Response:  resp,
		RawData:   rawData,
	}, nil
}

// handleRequestError processes request-level errors (network, timeout, cancellation)
func (c *InferenceHTTPClient) handleRequestError(ctx context.Context, err error, req *GenerateRequest) (*GenerateResponse, *ClientError) {
	if errors.Is(ctx.Err(), context.Canceled) {
		klog.V(3).Infof("Request cancelled for request_id=%s", req.RequestID)
		return nil, &ClientError{
			Category: httpclient.ErrCategoryUnknown,
			Message:  "request cancelled",
			RawError: err,
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		klog.V(3).Infof("Request timeout for request_id=%s", req.RequestID)
		return nil, &ClientError{
			Category: httpclient.ErrCategoryServer,
			Message:  "request timeout",
			RawError: err,
		}
	}

	klog.V(3).Infof("Request failed with network error for request_id=%s: %v", req.RequestID, err)
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
