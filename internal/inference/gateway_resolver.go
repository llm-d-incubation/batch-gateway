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
)

// GatewayClientConfig holds a fully-resolved, self-contained gateway configuration
// for one model. APIKey is the actual secret value (already read from disk).
// Every entry in the map passed to NewGatewayResolver must be fully specified —
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

// gwToHTTPCfg maps a GatewayClientConfig directly to an HTTPClientConfig.
func gwToHTTPCfg(gw GatewayClientConfig) HTTPClientConfig {
	return HTTPClientConfig{
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

// GatewayResolver routes inference requests to the correct gateway client
// based on the model name. Models without an explicit mapping fall back to
// the default client.
//
// GatewayResolver is immutable after construction — safe for concurrent reads.
// TODO: When dynamic config reload is added, wrap with atomic.Pointer[GatewayResolver]
// and swap the entire resolver on reload.
//
// This is a concrete struct rather than an interface because there is only one
// routing strategy. Tests inject mock Client instances via NewSingleClientResolver.
// TODO: Extract an interface if multiple routing strategies are needed.
type GatewayResolver struct {
	defaultClient Client
	modelClients  map[string]Client
}

// clientKey is the deduplication key for the HTTP client pool.
// Two gateways sharing identical URL, API key, and HTTP/TLS settings reuse a
// single client instance (and therefore the same connection pool).
// API key is included because the same URL may be reached with different keys.
type clientKey struct {
	url            string
	apiKey         string
	timeout        time.Duration
	maxRetries     int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	tlsSkipVerify  bool
	tlsCAFile      string
	tlsClientCert  string
	tlsClientKey   string
}

func clientKeyFromHTTPCfg(cfg HTTPClientConfig) clientKey {
	return clientKey{
		url:            cfg.BaseURL,
		apiKey:         cfg.APIKey,
		timeout:        cfg.Timeout,
		maxRetries:     cfg.MaxRetries,
		initialBackoff: cfg.InitialBackoff,
		maxBackoff:     cfg.MaxBackoff,
		tlsSkipVerify:  cfg.TLSInsecureSkipVerify,
		tlsCAFile:      cfg.TLSCACertFile,
		tlsClientCert:  cfg.TLSClientCertFile,
		tlsClientKey:   cfg.TLSClientKeyFile,
	}
}

// NewGatewayResolver creates a GatewayResolver from a map of model-to-gateway
// configs. The reserved key "default" is required and becomes the fallback client
// for models without an explicit entry. Every entry must be fully self-contained.
//
// Clients with identical settings (URL, API key, HTTP/TLS config) share a single
// HTTPClient instance to reuse connection pools.
func NewGatewayResolver(modelGateways map[string]GatewayClientConfig) (*GatewayResolver, error) {
	defaultGW, ok := modelGateways["default"]
	if !ok {
		// this is checked on validation but we'll be defensive here
		return nil, fmt.Errorf("modelGateways must contain a \"default\" entry")
	}

	defaultCfg := gwToHTTPCfg(defaultGW)
	defaultClient, err := NewHTTPClient(defaultCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create default inference client: %w", err)
	}

	// Pool of clients for reuse. The key is the HTTPClientConfig.
	// enables sharing of clients with the same URL, API key, and HTTP/TLS config.
	pool := map[clientKey]Client{
		clientKeyFromHTTPCfg(defaultCfg): defaultClient,
	}
	modelClients := make(map[string]Client, len(modelGateways))

	for model, gw := range modelGateways {
		if model == "default" {
			continue
		}

		cfg := gwToHTTPCfg(gw)
		key := clientKeyFromHTTPCfg(cfg)

		if client, ok := pool[key]; ok {
			modelClients[model] = client
			continue
		}

		client, err := NewHTTPClient(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create inference client for model %q (url %s): %w", model, gw.URL, err)
		}
		pool[key] = client
		modelClients[model] = client
	}

	return &GatewayResolver{
		defaultClient: defaultClient,
		modelClients:  modelClients,
	}, nil
}

// ClientFor returns the inference client for the given model.
// Falls back to the default client if no model-specific mapping exists.
func (r *GatewayResolver) ClientFor(modelID string) Client {
	if c, ok := r.modelClients[modelID]; ok {
		return c
	}
	return r.defaultClient
}

// NewSingleClientResolver wraps a single Client in a GatewayResolver
// where all models resolve to that client. Currently used only in tests
// to inject mock inference clients into Clientset.
func NewSingleClientResolver(c Client) *GatewayResolver {
	return &GatewayResolver{defaultClient: c}
}
