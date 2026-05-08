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
	c := &stubClient{id: "global"}
	r := NewSingleClientResolver(c)

	got := r.ClientFor("any-model")
	if got != c {
		t.Fatalf("expected global client for any model")
	}
}

func TestGatewayResolver_GlobalClient_OverridesPerModel(t *testing.T) {
	globalC := &stubClient{id: "global"}
	modelC := &stubClient{id: "model-a"}

	r := &GatewayResolver{
		globalClient: globalC,
		modelClients: map[string]InferenceClient{"model-a": modelC},
	}

	if got := r.ClientFor("model-a"); got != globalC {
		t.Fatalf("expected global client even for model-a when global is set")
	}
	if got := r.ClientFor("unknown"); got != globalC {
		t.Fatalf("expected global client for unknown model")
	}
}

func TestGatewayResolver_PerModelOnly_ExactMatch(t *testing.T) {
	modelC := &stubClient{id: "model-a"}

	r := &GatewayResolver{
		modelClients: map[string]InferenceClient{"model-a": modelC},
	}

	if got := r.ClientFor("model-a"); got != modelC {
		t.Fatalf("expected model-specific client for model-a")
	}
}

func TestGatewayResolver_PerModelOnly_UnknownReturnsNil(t *testing.T) {
	modelC := &stubClient{id: "model-a"}

	r := &GatewayResolver{
		modelClients: map[string]InferenceClient{"model-a": modelC},
	}

	if got := r.ClientFor("unknown"); got != nil {
		t.Fatalf("expected nil for unknown model without global, got %v", got)
	}
}

func TestNewGlobalResolver(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	r, err := NewGlobalResolver(GatewayClientConfig{URL: srv.URL}, testLogger(t))
	if err != nil {
		t.Fatalf("NewGlobalResolver: %v", err)
	}

	if got := r.ClientFor("any-model"); got == nil {
		t.Fatal("expected non-nil client from global gateway")
	}
}

func TestNewPerModelResolver(t *testing.T) {
	srvA := newTestServer(t, nil)
	defer srvA.Close()
	srvB := newTestServer(t, nil)
	defer srvB.Close()

	perModel := map[string]GatewayClientConfig{
		"model-a": {URL: srvA.URL},
		"model-b": {URL: srvB.URL},
	}

	r, err := NewPerModelResolver(perModel, testLogger(t))
	if err != nil {
		t.Fatalf("NewPerModelResolver: %v", err)
	}

	if got := r.ClientFor("model-a"); got == nil {
		t.Fatal("expected non-nil client for model-a")
	}
	if got := r.ClientFor("model-b"); got == nil {
		t.Fatal("expected non-nil client for model-b")
	}
	if got := r.ClientFor("unknown"); got != nil {
		t.Fatalf("expected nil for unknown model, got %v", got)
	}
}

func TestNewPerModelResolver_SharesClientsForSameConfig(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()
	srvOther := newTestServer(t, nil)
	defer srvOther.Close()

	perModel := map[string]GatewayClientConfig{
		"model-a": {URL: srv.URL},
		"model-b": {URL: srv.URL},
		"model-c": {URL: srvOther.URL},
	}

	r, err := NewPerModelResolver(perModel, testLogger(t))
	if err != nil {
		t.Fatalf("NewPerModelResolver: %v", err)
	}

	cA := r.ClientFor("model-a")
	cB := r.ClientFor("model-b")
	cC := r.ClientFor("model-c")

	if cA != cB {
		t.Fatal("expected model-a and model-b to share client (same URL)")
	}
	if cA == cC {
		t.Fatal("expected model-c to have a different client (different URL)")
	}
}

func TestNewPerModelResolver_SameURLDifferentKey_DifferentClients(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	perModel := map[string]GatewayClientConfig{
		"model-a": {URL: srv.URL, APIKey: "key-a", Timeout: 5 * time.Minute, MaxRetries: 3, InitialBackoff: time.Second, MaxBackoff: time.Minute},
		"model-b": {URL: srv.URL, APIKey: "key-b", Timeout: 5 * time.Minute, MaxRetries: 3, InitialBackoff: time.Second, MaxBackoff: time.Minute},
	}

	r, err := NewPerModelResolver(perModel, testLogger(t))
	if err != nil {
		t.Fatalf("NewPerModelResolver: %v", err)
	}

	cA := r.ClientFor("model-a")
	cB := r.ClientFor("model-b")

	if cA == cB {
		t.Fatal("expected different clients for different API keys")
	}
}

func TestGatewayResolver_Clients(t *testing.T) {
	t.Run("global resolver returns single element", func(t *testing.T) {
		srv := newTestServer(t, nil)
		defer srv.Close()

		r, err := NewGlobalResolver(GatewayClientConfig{URL: srv.URL}, testLogger(t))
		if err != nil {
			t.Fatalf("NewGlobalResolver: %v", err)
		}

		clients := r.Clients()
		if len(clients) != 1 {
			t.Fatalf("Clients() returned %d elements, want 1", len(clients))
		}
		if clients[0] != r.ClientFor("any") {
			t.Fatal("Clients()[0] should be the same as ClientFor()")
		}
	})

	t.Run("per-model resolver with shared configs returns deduplicated set", func(t *testing.T) {
		srvA := newTestServer(t, nil)
		defer srvA.Close()
		srvB := newTestServer(t, nil)
		defer srvB.Close()

		perModel := map[string]GatewayClientConfig{
			"model-a": {URL: srvA.URL},
			"model-b": {URL: srvA.URL},
			"model-c": {URL: srvB.URL},
		}

		r, err := NewPerModelResolver(perModel, testLogger(t))
		if err != nil {
			t.Fatalf("NewPerModelResolver: %v", err)
		}

		clients := r.Clients()
		if len(clients) != 2 {
			t.Fatalf("Clients() returned %d elements, want 2 (deduplicated)", len(clients))
		}
	})
}

func TestGatewayResolver_ClientLabel(t *testing.T) {
	t.Run("returns URL for known client", func(t *testing.T) {
		srv := newTestServer(t, nil)
		defer srv.Close()

		r, err := NewGlobalResolver(GatewayClientConfig{URL: srv.URL}, testLogger(t))
		if err != nil {
			t.Fatalf("NewGlobalResolver: %v", err)
		}

		client := r.ClientFor("any")
		label := r.ClientLabel(client)
		if label != srv.URL {
			t.Fatalf("ClientLabel() = %q, want %q", label, srv.URL)
		}
	})

	t.Run("returns unknown for unrecognized client", func(t *testing.T) {
		srv := newTestServer(t, nil)
		defer srv.Close()

		r, err := NewGlobalResolver(GatewayClientConfig{URL: srv.URL}, testLogger(t))
		if err != nil {
			t.Fatalf("NewGlobalResolver: %v", err)
		}

		unknown := &stubClient{id: "stranger"}
		label := r.ClientLabel(unknown)
		if label != "unknown" {
			t.Fatalf("ClientLabel() = %q, want %q", label, "unknown")
		}
	})

	t.Run("NewSingleClientResolver populates label", func(t *testing.T) {
		c := &stubClient{id: "mock"}
		r := NewSingleClientResolver(c)

		label := r.ClientLabel(c)
		if label == "unknown" {
			t.Fatal("ClientLabel() should not return \"unknown\" for client from NewSingleClientResolver")
		}
	})

	t.Run("NewPerModelClientResolver populates labels", func(t *testing.T) {
		cA := &stubClient{id: "a"}
		cB := &stubClient{id: "b"}
		r := NewPerModelClientResolver(map[string]InferenceClient{
			"model-a": cA,
			"model-b": cB,
		})

		if label := r.ClientLabel(cA); label == "unknown" {
			t.Fatal("ClientLabel(cA) should not return \"unknown\"")
		}
		if label := r.ClientLabel(cB); label == "unknown" {
			t.Fatal("ClientLabel(cB) should not return \"unknown\"")
		}

		stranger := &stubClient{id: "stranger"}
		if label := r.ClientLabel(stranger); label != "unknown" {
			t.Fatalf("ClientLabel(stranger) = %q, want \"unknown\"", label)
		}
	})
}
