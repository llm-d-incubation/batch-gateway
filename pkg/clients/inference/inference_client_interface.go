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

	httpclient "github.com/llm-d-incubation/batch-gateway/pkg/clients/http"
)

// InferenceClientI defines the interface for making inference requests
type InferenceClient interface {
	Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, *ClientError)
}

// GenerateRequest represents an inference generation request
type GenerateRequest struct {
	RequestID string                 // unique request id set by user
	Endpoint  string                 // API endpoint (e.g., "/v1/chat/completions")
	Params    map[string]interface{} // parameters (must include "model")
	Headers   map[string]string      // extra headers to forward to the endpoint
}

// GenerateResponse represents an inference generation response
type GenerateResponse struct {
	RequestID string
	Response  []byte
	RawData   interface{}
}

// ClientError represents an inference client error
type ClientError = httpclient.ClientError

// HTTPClientConfig is an alias for httpclient.Config
type HTTPClientConfig = httpclient.Config
