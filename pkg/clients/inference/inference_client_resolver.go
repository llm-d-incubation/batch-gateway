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
	"fmt"
	"time"

	"github.com/go-logr/logr"
)

// GatewayClientConfig holds a fully-resolved, self-contained gateway configuration.
// APIKey is the actual secret value (already read from disk).
// Every entry in the map passed to NewPerModelResolver must be fully specified —
// there is no inheritance between entries.
type GatewayClientConfig struct {
	URL    string
	APIKey string

	Timeout        time.Duration
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration

	TLSInsecureSkipVerify bool
	TLSCACertFile         string
	TLSClientCertFile     string
	TLSClientKeyFile      string
}

// toHTTPClientConfig converts a GatewayClientConfig to an HTTPClientConfig.
func (gw GatewayClientConfig) toHTTPClientConfig() *HTTPClientConfig {
	return &HTTPClientConfig{
		BaseURL:               gw.URL,
		APIKey:                gw.APIKey,
		Timeout:               gw.Timeout,
		MaxRetries:            gw.MaxRetries,
		InitialBackoff:        gw.InitialBackoff,
		MaxBackoff:            gw.MaxBackoff,
		TLSInsecureSkipVerify: gw.TLSInsecureSkipVerify,
		TLSCACertFile:         gw.TLSCACertFile,
		TLSClientCertFile:     gw.TLSClientCertFile,
		TLSClientKeyFile:      gw.TLSClientKeyFile,
	}
}

// ErrCodeModelNotFound is the request-level error code written to the batch
// error file when a model has no configured gateway in per-model mode.
const ErrCodeModelNotFound = "model_not_found"

// GatewayResolver routes inference requests to the correct gateway client
// based on the model name.
//
// Resolution order:
//  1. If a global client is set, return it for every model.
//  2. Exact match in per-model clients.
//  3. Return nil — the caller should treat this as a request-level error.
//
// GatewayResolver is immutable after construction — safe for concurrent reads.
// TODO: When dynamic config reload is added, wrap with atomic.Pointer[GatewayResolver]
// and swap the entire resolver on reload.
type GatewayResolver struct {
	globalClient InferenceClient
	modelClients map[string]InferenceClient
}

// NewGlobalResolver creates a GatewayResolver where all models resolve to a
// single global inference gateway. Use this when the downstream gateway handles
// model routing internally.
func NewGlobalResolver(config GatewayClientConfig, logger logr.Logger) (*GatewayResolver, error) {
	client, err := NewInferenceClient(config.toHTTPClientConfig(), logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create global inference client: %w", err)
	}
	return &GatewayResolver{globalClient: client}, nil
}

// NewPerModelResolver creates a GatewayResolver that routes each model to its
// own inference gateway. Clients with identical settings share a single
// HTTPClient instance to reuse connection pools.
func NewPerModelResolver(configs map[string]GatewayClientConfig, logger logr.Logger) (*GatewayResolver, error) {
	pool := make(map[GatewayClientConfig]InferenceClient)
	modelClients := make(map[string]InferenceClient, len(configs))

	for model, gw := range configs {
		if client, ok := pool[gw]; ok {
			modelClients[model] = client
			continue
		}
		client, err := NewInferenceClient(gw.toHTTPClientConfig(), logger)
		if err != nil {
			return nil, fmt.Errorf("failed to create inference client for model %q (url %s): %w", model, gw.URL, err)
		}
		pool[gw] = client
		modelClients[model] = client
	}

	return &GatewayResolver{modelClients: modelClients}, nil
}

// IsGlobal returns true if the resolver routes all models to a single global
// client. When true, ClientFor never returns nil.
func (r *GatewayResolver) IsGlobal() bool {
	return r.globalClient != nil
}

// ClientFor returns the inference client for the given model.
// Returns nil if no matching client exists. In normal operation, unregistered
// models are rejected during ingestion, so nil is only expected in recovery
// or defensive-guard paths.
// A zero-value GatewayResolver returns nil for all models; use the public
// constructors (NewGlobalResolver, NewPerModelResolver, NewSingleClientResolver)
// to ensure at least one client is configured.
func (r *GatewayResolver) ClientFor(modelID string) InferenceClient {
	if r.globalClient != nil {
		return r.globalClient
	}
	if c, ok := r.modelClients[modelID]; ok {
		return c
	}
	return nil
}

// NewSingleClientResolver wraps a single InferenceClient in a GatewayResolver
// where all models resolve to that client. Used in tests to inject mock
// inference clients into Clientset.
// TODO: if httptest is imported for other reasons in the future, replace callers
// with NewGlobalResolver + httptest.Server so this function can be removed.
func NewSingleClientResolver(c InferenceClient) *GatewayResolver {
	return &GatewayResolver{globalClient: c}
}
