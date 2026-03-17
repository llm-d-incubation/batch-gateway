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

// The file contains unit tests for the panic recovery middleware.
package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
)

var dummyRoute = common.Route{Method: http.MethodGet, Pattern: "/test"}

func TestRecoveryMiddleware(t *testing.T) {
	t.Run("NoPanic", doTestRecoveryMiddlewareNoPanic)
	t.Run("WithPanic", doTestRecoveryMiddlewareWithPanic)
}

func doTestRecoveryMiddlewareNoPanic(t *testing.T) {
	handler := Recovery(dummyRoute, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("success"))
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	body := w.Body.String()
	if body != "success" {
		t.Errorf("expected body %q, got %q", "success", body)
	}
}

func doTestRecoveryMiddlewareWithPanic(t *testing.T) {
	tests := []struct {
		name       string
		panicValue interface{}
	}{
		{
			name:       "string panic",
			panicValue: "error message",
		},
		{
			name:       "error panic",
			panicValue: errors.New("error object"),
		},
		{
			name:       "int panic",
			panicValue: 42,
		},
		{
			name:       "nil panic",
			panicValue: nil,
		},
		{
			name:       "struct panic",
			panicValue: struct{ Code int }{Code: 500},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := Recovery(dummyRoute, func(w http.ResponseWriter, r *http.Request) {
				panic(tt.panicValue)
			})

			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			ctx := context.WithValue(req.Context(), common.RequestIDKey, "test-request-id-123")
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			// Check status code
			if w.Code != http.StatusInternalServerError {
				t.Errorf("expected status %d, got %d", http.StatusInternalServerError, w.Code)
			}

			// Check Content-Type header
			contentType := w.Header().Get("Content-Type")
			if contentType != "application/json" {
				t.Errorf("expected Content-Type %q, got %q", "application/json", contentType)
			}

			// Check response body is valid JSON with OpenAI error format
			var resp openai.ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("failed to decode JSON response: %v", err)
			}

			// Check error fields
			if resp.Error.Code != http.StatusInternalServerError {
				t.Errorf("expected error code %d, got %d", http.StatusInternalServerError, resp.Error.Code)
			}

			if resp.Error.Type != "InternalServerError" {
				t.Errorf("expected error type %q, got %q", "InternalServerError", resp.Error.Type)
			}

			if resp.Error.Message != "The server had an error while processing your request" {
				t.Errorf("expected error message %q, got %q", "The server had an error while processing your request", resp.Error.Message)
			}

			// Check requestID is in param field
			if resp.Error.Param == nil || *resp.Error.Param != "test-request-id-123" {
				if resp.Error.Param == nil {
					t.Errorf("expected param (requestID) %q, got nil", "test-request-id-123")
				} else {
					t.Errorf("expected param (requestID) %q, got %q", "test-request-id-123", *resp.Error.Param)
				}
			}

		})
	}
}

func BenchmarkRecoveryMiddleware_NoPanic(b *testing.B) {
	handler := Recovery(dummyRoute, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}
}

func BenchmarkRecoveryMiddleware_WithPanic(b *testing.B) {
	handler := Recovery(dummyRoute, func(w http.ResponseWriter, r *http.Request) {
		panic("benchmark panic")
	})
	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}
}
