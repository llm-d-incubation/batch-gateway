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

// The file contains unit tests for the health check handlers.
package health

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
)

func TestHealthHandler(t *testing.T) {
	mux := http.NewServeMux()
	handler := NewHealthApiHandler()
	common.RegisterHandler(mux, handler)

	tests := []struct {
		name           string
		method         string
		path           string
		expectedStatus int
	}{
		{
			name:           "GET api health returns 200",
			method:         http.MethodGet,
			path:           HealthPath,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "HEAD api health returns 200",
			method:         http.MethodHead,
			path:           HealthPath,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "POST api health returns 405",
			method:         http.MethodPost,
			path:           HealthPath,
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "PUT api health returns 405",
			method:         http.MethodPut,
			path:           HealthPath,
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "DELETE api health returns 405",
			method:         http.MethodDelete,
			path:           HealthPath,
			expectedStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			w := httptest.NewRecorder()

			mux.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			if tt.expectedStatus == http.StatusOK && tt.method == http.MethodGet {
				contentType := w.Header().Get("Content-Type")
				if contentType != "text/plain; charset=utf-8" {
					t.Errorf("Content-Type header not set correctly, got %q", contentType)
				}

				body := w.Body.String()
				if body != "OK" {
					t.Errorf("expected body %q, got %q", "OK", body)
				}
			}
		})
	}
}

func BenchmarkHealthHandler(b *testing.B) {
	handler := NewHealthApiHandler()
	req := httptest.NewRequest(http.MethodGet, HealthPath, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		handler.HealthHandler(w, req)
	}
}
