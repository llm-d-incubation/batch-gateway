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
	"errors"
	"fmt"
	"io"
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
// Dynamic config reload builds a fresh GatewayResolver and swaps it
// atomically into the processor's routing state (see
// internal/processor/worker/config_reload.go); old resolvers are closed
// after a grace period once they fall out of the routing path.
type GatewayResolver struct {
	globalClient InferenceClient
	modelClients map[string]InferenceClient
	clientURLs   map[InferenceClient]string
	closers      []io.Closer
}

// NewGlobalResolver creates a GatewayResolver where all models resolve to a
// single global inference gateway. Use this when the downstream gateway handles
// model routing internally.
func NewGlobalResolver(config GatewayClientConfig, logger logr.Logger) (*GatewayResolver, error) {
	client, err := NewInferenceClient(config.toHTTPClientConfig(), logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create global inference client: %w", err)
	}
	return &GatewayResolver{
		globalClient: client,
		clientURLs:   map[InferenceClient]string{client: config.URL},
		closers:      asClosers([]InferenceClient{client}),
	}, nil
}

// newInferenceClient is the client constructor used by NewPerModelResolver.
// Declared as a var so tests can substitute a failing constructor over
// closeable fakes when asserting failure-path cleanup.
var newInferenceClient = func(config *HTTPClientConfig, logger logr.Logger) (InferenceClient, error) {
	return NewInferenceClient(config, logger)
}

// NewPerModelResolver creates a GatewayResolver that routes each model to its
// own inference gateway. Clients with identical settings share a single
// HTTPClient instance to reuse connection pools.
func NewPerModelResolver(configs map[string]GatewayClientConfig, logger logr.Logger) (*GatewayResolver, error) {
	pool := make(map[GatewayClientConfig]InferenceClient)
	modelClients := make(map[string]InferenceClient, len(configs))
	urls := make(map[InferenceClient]string)

	for model, gw := range configs {
		if client, ok := pool[gw]; ok {
			modelClients[model] = client
			continue
		}
		client, err := newInferenceClient(gw.toHTTPClientConfig(), logger)
		if err != nil {
			// Close the clients already built for earlier gateways: the
			// caller (config reload) retries failed construction every
			// poll, so leaked HTTP transports would accumulate. Iterating
			// the pool closes each unique client exactly once.
			for _, pooled := range pool {
				if closer, ok := pooled.(io.Closer); ok {
					_ = closer.Close()
				}
			}
			return nil, fmt.Errorf("failed to create inference client for model %q (url %s): %w", model, gw.URL, err)
		}
		pool[gw] = client
		urls[client] = gw.URL
		modelClients[model] = client
	}

	closers := make([]InferenceClient, 0, len(pool))
	for _, client := range pool {
		closers = append(closers, client)
	}

	return &GatewayResolver{modelClients: modelClients, clientURLs: urls, closers: asClosers(closers)}, nil
}

// asClosers returns the io.Closer views of clients that support closing.
// Mock clients used in tests do not implement io.Closer and are skipped.
func asClosers(clients []InferenceClient) []io.Closer {
	closers := make([]io.Closer, 0, len(clients))
	for _, c := range clients {
		if closer, ok := c.(io.Closer); ok {
			closers = append(closers, closer)
		}
	}
	return closers
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

// Clients returns the deduplicated set of InferenceClient instances managed by
// this resolver. In global mode this is a single-element slice; in per-model
// mode it reflects the pooled clients (models sharing identical gateway configs
// share a single InferenceClient).
func (r *GatewayResolver) Clients() []InferenceClient {
	if r.globalClient != nil {
		return []InferenceClient{r.globalClient}
	}
	seen := make(map[InferenceClient]struct{}, len(r.modelClients))
	clients := make([]InferenceClient, 0, len(r.modelClients))
	for _, c := range r.modelClients {
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		clients = append(clients, c)
	}
	return clients
}

// ClientLabel returns a human-readable identifier (the gateway URL) for the
// given client. Returns "unknown" if the client was not created by this resolver.
func (r *GatewayResolver) ClientLabel(c InferenceClient) string {
	if url, ok := r.clientURLs[c]; ok {
		return url
	}
	return "unknown"
}

// Close releases resources held by the resolver (e.g. Redis connections for
// async dispatch). Safe to call on resolvers that hold no closeable resources.
func (r *GatewayResolver) Close() error {
	var errs []error
	for _, c := range r.closers {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// NewSingleClientResolver wraps a single InferenceClient in a GatewayResolver
// where all models resolve to that client. Used in tests to inject mock
// inference clients into Clientset.
// TODO: if httptest is imported for other reasons in the future, replace callers
// with NewGlobalResolver + httptest.Server so this function can be removed.
func NewSingleClientResolver(c InferenceClient) *GatewayResolver {
	return &GatewayResolver{
		globalClient: c,
		clientURLs:   map[InferenceClient]string{c: "test"},
		closers:      asClosers([]InferenceClient{c}),
	}
}

// NewPerModelClientResolver creates a GatewayResolver that maps each model name
// to a pre-built InferenceClient. Used in tests to inject per-model mock clients
// without creating real HTTP connections.
func NewPerModelClientResolver(clients map[string]InferenceClient) *GatewayResolver {
	urls := make(map[InferenceClient]string, len(clients))
	for model, c := range clients {
		if _, ok := urls[c]; !ok {
			urls[c] = "test-" + model
		}
	}
	return &GatewayResolver{modelClients: clients, clientURLs: urls}
}
