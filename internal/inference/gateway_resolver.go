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

	ucom "github.com/llm-d-incubation/batch-gateway/internal/util/com"
)

// GatewayEndpoint holds a resolved per-model gateway configuration.
// APIKey is the actual key value (not a file path). If empty, the base
// config's API key is inherited.
type GatewayEndpoint struct {
	URL    string
	APIKey string
}

// NewGatewayEndpoint creates a GatewayEndpoint by resolving the optional API
// key from the mounted secrets directory (/etc/.secrets/).
// If apiKeyName is empty or the secret does not exist, the returned endpoint
// inherits the default API key from the base config at resolver construction time.
func NewGatewayEndpoint(url, apiKeyName string) (GatewayEndpoint, error) {
	ep := GatewayEndpoint{URL: url}
	if apiKeyName != "" {
		key, err := ucom.ReadSecretFile(apiKeyName)
		if err != nil {
			return GatewayEndpoint{}, fmt.Errorf("read secret %q: %w", apiKeyName, err)
		}
		ep.APIKey = key
	}
	return ep, nil
}

// GatewayResolver routes inference requests to the correct gateway client
// based on the model name. Models without an explicit mapping fall back to
// the default client.
//
// This is a concrete struct rather than an interface because there is only one
// routing strategy. Tests inject mock Client instances via NewSingleClientResolver.
// TODO: Extract an interface if multiple routing strategies are needed
//
// GatewayResolver is immutable after construction — safe for concurrent reads.
// TODO: When dynamic config reload is added, wrap with atomic.Pointer[GatewayResolver]
// and swap the entire resolver on reload.
type GatewayResolver struct {
	defaultClient Client
	modelClients  map[string]Client
}

// clientKey is a deduplication key for the HTTP client pool.
// Gateways sharing both the same URL and API key reuse a single client.
type clientKey struct {
	url    string
	apiKey string
}

// NewGatewayResolver creates a GatewayResolver from a base config and per-model
// gateway overrides. Clients with the same URL and API key share a single
// HTTPClient instance to reuse connection pools.
func NewGatewayResolver(baseCfg HTTPClientConfig, modelGateways map[string]GatewayEndpoint) (*GatewayResolver, error) {
	defaultClient, err := NewHTTPClient(baseCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create default inference client: %w", err)
	}

	// Pool of clients for reuse. The key is the URL + API key.
	// This covers the case where multiple models share the same URL but different API keys.
	pool := map[clientKey]Client{
		{baseCfg.BaseURL, baseCfg.APIKey}: defaultClient,
	}
	modelClients := make(map[string]Client, len(modelGateways))

	for model, gatewayEndpoint := range modelGateways {
		cfg := baseCfg
		cfg.BaseURL = gatewayEndpoint.URL

		if gatewayEndpoint.APIKey != "" {
			cfg.APIKey = gatewayEndpoint.APIKey
		}

		key := clientKey{cfg.BaseURL, cfg.APIKey}

		// Reuse existing client if it already exists
		if client, ok := pool[key]; ok {
			modelClients[model] = client
			continue
		}

		client, err := NewHTTPClient(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create inference client for model %q (url %s): %w", model, gatewayEndpoint.URL, err)
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
