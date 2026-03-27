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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
)

func testLogger(t testing.TB) logr.Logger {
	return testr.NewWithInterface(t, testr.Options{})
}

func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	if handler == nil {
		handler = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}
	}
	return httptest.NewServer(handler)
}

type stubClient struct{ id string }

func (s *stubClient) Generate(_ context.Context, _ *GenerateRequest) (*GenerateResponse, *ClientError) {
	return &GenerateResponse{RequestID: s.id}, nil
}

func TestNewSingleClientResolver(t *testing.T) {
	c := &stubClient{id: "default"}
	r := NewSingleClientResolver(c)

	got := r.ClientFor("any-model-without-override")
	if got != c {
		t.Fatalf("expected default client for any model without override")
	}
}

func TestGatewayResolver_ClientFor_DefaultFallback(t *testing.T) {
	defaultC := &stubClient{id: "default"}
	modelC := &stubClient{id: "model-a"}

	r := &GatewayResolver{
		defaultClient: defaultC,
		modelClients:  map[string]InferenceClient{"model-a": modelC},
	}

	if got := r.ClientFor("model-a"); got != modelC {
		t.Fatalf("expected model-specific client for model-a, got %v", got)
	}
	if got := r.ClientFor("unknown"); got != defaultC {
		t.Fatalf("expected default client for unknown model, got %v", got)
	}
}

func TestNewGatewayResolver_SharesClientsForSameURL(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()
	srvOther := newTestServer(t, nil)
	defer srvOther.Close()

	modelGateways := map[string]GatewayClientConfig{
		"default": {URL: srv.URL},
		"model-a": {URL: srv.URL},
		"model-b": {URL: srv.URL},
		"model-c": {URL: srvOther.URL},
	}

	r, err := NewGatewayResolver(modelGateways, testLogger(t))
	if err != nil {
		t.Fatalf("NewGatewayResolver: %v", err)
	}

	cA := r.ClientFor("model-a")
	cB := r.ClientFor("model-b")
	cC := r.ClientFor("model-c")
	cDefault := r.ClientFor("unknown")

	if cA != cB {
		t.Fatal("expected model-a and model-b to share the same client since they have the same URL")
	}
	if cA != cDefault {
		t.Fatal("expected models with base URL to share the default client")
	}
	if cC == cDefault {
		t.Fatal("expected model-c to have a different client since it has a different URL")
	}
}

func TestNewGatewayResolver_DifferentURLs(t *testing.T) {
	srvA := newTestServer(t, nil)
	defer srvA.Close()
	srvB := newTestServer(t, nil)
	defer srvB.Close()

	modelGateways := map[string]GatewayClientConfig{
		"default": {URL: srvA.URL},
		"model-b": {URL: srvB.URL},
	}

	r, err := NewGatewayResolver(modelGateways, testLogger(t))
	if err != nil {
		t.Fatalf("NewGatewayResolver: %v", err)
	}

	cDefault := r.ClientFor("model-a")
	cB := r.ClientFor("model-b")

	if cDefault == cB {
		t.Fatal("expected different clients for different gateway URLs")
	}
}

func TestNewGatewayResolver_MissingDefault_ReturnsError(t *testing.T) {
	modelGateways := map[string]GatewayClientConfig{
		"llama-3": {URL: "http://gateway-a:8000"},
	}

	_, err := NewGatewayResolver(modelGateways, testLogger(t))
	if err == nil {
		t.Fatal("expected error when \"default\" key is missing, got nil")
	}
}

func TestNewGatewayResolver_PerGatewayAPIKey(t *testing.T) {
	srvA := newTestServer(t, nil)
	defer srvA.Close()
	srvB := newTestServer(t, nil)
	defer srvB.Close()

	modelGateways := map[string]GatewayClientConfig{
		"default": {URL: srvA.URL, APIKey: "default-key"},
		"model-a": {URL: srvB.URL, APIKey: "key-a"},
		"model-b": {URL: srvB.URL, APIKey: "key-b"},
		"model-c": {URL: srvB.URL, APIKey: "key-a"},
	}

	r, err := NewGatewayResolver(modelGateways, testLogger(t))
	if err != nil {
		t.Fatalf("NewGatewayResolver: %v", err)
	}

	cA := r.ClientFor("model-a")
	cB := r.ClientFor("model-b")
	cC := r.ClientFor("model-c")
	cDefault := r.ClientFor("unknown")

	// model-a and model-c share URL + API key → same client
	if cA != cC {
		t.Fatal("expected model-a and model-c to share client (same URL + API key)")
	}
	// model-b has same URL but different API key → different client
	if cA == cB {
		t.Fatal("expected model-a and model-b to have different clients (different API keys)")
	}
	// all differ from default (different URL)
	if cA == cDefault || cB == cDefault {
		t.Fatal("expected per-gateway clients to differ from default")
	}
}

func TestNewGatewayResolver_SameURLDifferentKey_DifferentClients(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// With no field inheritance, model-a has an empty APIKey while default has "default-key".
	// Different clientKeys → separate client instances.
	modelGateways := map[string]GatewayClientConfig{
		"default": {URL: srv.URL, APIKey: "default-key", Timeout: 5 * time.Minute, MaxRetries: 3, InitialBackoff: time.Second, MaxBackoff: time.Minute},
		"model-a": {URL: srv.URL, Timeout: 5 * time.Minute, MaxRetries: 3, InitialBackoff: time.Second, MaxBackoff: time.Minute},
	}

	r, err := NewGatewayResolver(modelGateways, testLogger(t))
	if err != nil {
		t.Fatalf("NewGatewayResolver: %v", err)
	}

	cA := r.ClientFor("model-a")
	cDefault := r.ClientFor("unknown")

	// Different API keys → different client instances.
	if cA == cDefault {
		t.Fatal("expected model with no API key and default with a key to use different clients")
	}
}
