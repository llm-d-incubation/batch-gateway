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

// The file contains unit tests for the request middleware, focusing on tenant
// header extraction and the last-value workaround for ext_authz header append behavior.
package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/health"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/metrics"
)

// newTestConfig returns a minimal ServerConfig suitable for middleware tests.
func newTestConfig(tenantHeader string) *common.ServerConfig {
	return &common.ServerConfig{
		Port:         "8080",
		TenantHeader: tenantHeader,
	}
}

// captureHandler returns an http.Handler that captures the tenant ID from context.
func captureHandler(captured *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v, ok := r.Context().Value(common.TenantIDKey).(string); ok {
			*captured = v
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestRequestMiddleware_TenantHeader(t *testing.T) {
	const tenantHeader = "X-MaaS-Username"

	tests := []struct {
		name           string
		headerValues   []string // values to add for the tenant header
		expectedTenant string
	}{
		{
			name:           "single tenant header uses that value",
			headerValues:   []string{"real-user"},
			expectedTenant: "real-user",
		},
		{
			name:           "no tenant header falls back to default",
			headerValues:   nil,
			expectedTenant: common.DefaultTenantID,
		},
		{
			// Workaround for ext_authz append behavior: when a client sends a
			// spoofed header and the auth service appends the real value, the
			// middleware must use the last value (the auth-injected one).
			name:           "multiple tenant headers takes last value (ext_authz workaround)",
			headerValues:   []string{"attacker", "real-user"},
			expectedTenant: "real-user",
		},
		{
			name:           "three tenant headers takes last value",
			headerValues:   []string{"first", "second", "third"},
			expectedTenant: "third",
		},
		{
			name:           "empty single header falls back to default",
			headerValues:   []string{""},
			expectedTenant: common.DefaultTenantID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := newTestConfig(tenantHeader)
			var captured string
			handler := RequestMiddleware(config)(captureHandler(&captured))

			req := httptest.NewRequest(http.MethodGet, "/v1/batches", nil)
			for _, v := range tt.headerValues {
				req.Header.Add(tenantHeader, v)
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if captured != tt.expectedTenant {
				t.Errorf("expected tenant %q, got %q", tt.expectedTenant, captured)
			}
		})
	}
}

func TestRequestMiddleware_RequestID(t *testing.T) {
	config := newTestConfig("X-MaaS-Username")

	t.Run("preserves existing request ID", func(t *testing.T) {
		var captured string
		handler := RequestMiddleware(config)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if v, ok := r.Context().Value(common.RequestIDKey).(string); ok {
				captured = v
			}
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodGet, "/v1/batches", nil)
		req.Header.Set(RequestIdHeaderKey, "my-request-id")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if captured != "my-request-id" {
			t.Errorf("expected request ID %q, got %q", "my-request-id", captured)
		}
		if w.Header().Get(RequestIdHeaderKey) != "my-request-id" {
			t.Errorf("expected response header %q, got %q", "my-request-id", w.Header().Get(RequestIdHeaderKey))
		}
	})

	t.Run("generates request ID when absent", func(t *testing.T) {
		var captured string
		handler := RequestMiddleware(config)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if v, ok := r.Context().Value(common.RequestIDKey).(string); ok {
				captured = v
			}
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodGet, "/v1/batches", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if captured == "" {
			t.Error("expected generated request ID, got empty string")
		}
		headerID := w.Header().Get(RequestIdHeaderKey)
		if headerID == "" {
			t.Error("expected response header to have generated request ID")
		}
		if captured != headerID {
			t.Errorf("expected context request ID %q to match response header %q", captured, headerID)
		}
	})
}

func TestRequestMiddleware_SkipsMetricsAndHealth(t *testing.T) {
	config := newTestConfig("X-MaaS-Username")
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		// If the middleware processed this, there would be a request ID in context.
		// For skipped paths, the middleware delegates directly without enriching context.
		if _, ok := r.Context().Value(common.RequestIDKey).(string); ok {
			t.Error("expected no request ID in context for skipped path")
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := RequestMiddleware(config)(inner)

	for _, path := range []string{metrics.MetricsPath, health.HealthPath} {
		called = false
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if !called {
			t.Errorf("expected handler to be called for path %s", path)
		}
	}
}
