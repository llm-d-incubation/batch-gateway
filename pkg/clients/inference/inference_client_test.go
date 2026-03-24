//go:build !integration
// +build !integration

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
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	httpclient "github.com/llm-d-incubation/batch-gateway/pkg/clients/http"
)

// TestInferenceClient aggregates all HTTPClient test cases
// Run with: go test -run TestInferenceClient
func TestInferenceClient(t *testing.T) {
	t.Run("NewHTTPClient", testNewHTTPInferenceClient)
	t.Run("Generate", testGenerate)
	t.Run("ErrorHandling", testErrorHandling)
	t.Run("RetryLogic", testRetryLogic)
	t.Run("TLSConfiguration", testTLSConfiguration)
	t.Run("Authentication", testAuthentication)
	t.Run("NetworkErrors", testNetworkErrors)
}

func testNewHTTPInferenceClient(t *testing.T) {
	tests := []struct {
		name   string
		config HTTPClientConfig
	}{
		{
			name: "should create client with default configuration",
			config: HTTPClientConfig{
				BaseURL: "http://localhost:8000",
			},
		},
		{
			name: "should create client with custom configuration",
			config: HTTPClientConfig{
				BaseURL:         "http://localhost:9000",
				Timeout:         1 * time.Minute,
				MaxIdleConns:    50,
				IdleConnTimeout: 60 * time.Second,
				APIKey:          "test-api-key",
			},
		},
		{
			name: "should apply retry defaults when MaxRetries is set",
			config: HTTPClientConfig{
				BaseURL:    "http://localhost:8000",
				MaxRetries: 3,
			},
		},
		{
			name: "should respect custom retry configuration",
			config: HTTPClientConfig{
				BaseURL:        "http://localhost:8000",
				MaxRetries:     5,
				InitialBackoff: 2 * time.Second,
				MaxBackoff:     120 * time.Second,
			},
		},
		{
			name: "should apply partial retry defaults",
			config: HTTPClientConfig{
				BaseURL:        "http://localhost:8000",
				MaxRetries:     3,
				InitialBackoff: 500 * time.Millisecond,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewInferenceClient(&tt.config)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if client == nil {
				t.Fatal("expected non-nil client")
			}
			if client.client == nil {
				t.Error("expected non-nil client.client")
			}
			// Note: resty.Client internal state (timeout, auth, retry config) is not directly accessible
			// Behavior is validated through integration and functional tests
		})
	}
}

func testGenerate(t *testing.T) {
	t.Run("should successfully make inference request with chat completion", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify headers
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("got Content-Type %v, want %v", got, "application/json")
			}

			// Verify request ID if present
			if requestID := r.Header.Get("X-Request-ID"); requestID != "" {
				if requestID != "test-request-123" {
					t.Errorf("got X-Request-ID %v, want %v", requestID, "test-request-123")
				}
			}

			// Return success response
			response := map[string]interface{}{
				"id":      "chatcmpl-123",
				"object":  "chat.completion",
				"created": 1699896916,
				"model":   "gpt-4",
				"choices": []map[string]interface{}{
					{
						"index": 0,
						"message": map[string]interface{}{
							"role":    "assistant",
							"content": "Hello! How can I help you?",
						},
						"finish_reason": "stop",
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		}))
		t.Cleanup(testServer.Close)

		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL: testServer.URL,
			Timeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req := &GenerateRequest{
			RequestID: "test-request-123",
			Endpoint:  "/v1/chat/completions",
			Params: map[string]interface{}{
				"model": "gpt-4",
				"messages": []map[string]interface{}{
					{
						"role":    "user",
						"content": "Hello",
					},
				},
			},
		}

		ctx := context.Background()
		resp, genErr := client.Generate(ctx, req)

		if genErr != nil {
			t.Errorf("expected no error, got %v", genErr)
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		if resp.RequestID != "test-request-123" {
			t.Errorf("got RequestID %v, want %v", resp.RequestID, "test-request-123")
		}
		if resp.Response == nil {
			t.Error("expected non-nil Response")
		}
		if resp.RawData == nil {
			t.Error("expected non-nil RawData")
		}

		// Verify response can be unmarshaled
		var data map[string]interface{}
		unmarshalErr := json.Unmarshal(resp.Response, &data)
		if unmarshalErr != nil {
			t.Errorf("expected no error, got %v", unmarshalErr)
		}
		if data["id"] != "chatcmpl-123" {
			t.Errorf("got id %v, want %v", data["id"], "chatcmpl-123")
		}
	})

	t.Run("should handle nil request", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(testServer.Close)

		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL: testServer.URL,
			Timeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ctx := context.Background()
		resp, genErr := client.Generate(ctx, nil)

		if resp != nil {
			t.Errorf("expected nil response, got %v", resp)
		}
		if genErr == nil {
			t.Fatal("expected non-nil error")
		}
		if genErr.Category != httpclient.ErrCategoryInvalidReq {
			t.Errorf("got Category %v, want %v", genErr.Category, httpclient.ErrCategoryInvalidReq)
		}
		if !strings.Contains(genErr.Message, "cannot be nil") {
			t.Errorf("expected %q to contain %q", genErr.Message, "cannot be nil")
		}
	})

	t.Run("should use endpoint from request", func(t *testing.T) {
		endpoint := ""
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			endpoint = r.URL.Path
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "test"})
		}))
		t.Cleanup(testServer.Close)

		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL: testServer.URL,
			Timeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req := &GenerateRequest{
			RequestID: "test",
			Endpoint:  "/v1/chat/completions",
			Params: map[string]interface{}{
				"messages": []map[string]interface{}{
					{"role": "user", "content": "test"},
				},
			},
		}

		_, _ = client.Generate(context.Background(), req)
		if endpoint != "/v1/chat/completions" {
			t.Errorf("got endpoint %v, want %v", endpoint, "/v1/chat/completions")
		}
	})

	t.Run("should fail when endpoint is empty", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "test"})
		}))
		t.Cleanup(testServer.Close)

		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL: testServer.URL,
			Timeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req := &GenerateRequest{
			RequestID: "test",
			Endpoint:  "", // Empty endpoint
			Params: map[string]interface{}{
				"messages": []map[string]interface{}{
					{"role": "user", "content": "test"},
				},
			},
		}

		resp, genErr := client.Generate(context.Background(), req)
		if resp != nil {
			t.Errorf("expected nil response, got %v", resp)
		}
		if genErr == nil {
			t.Fatal("expected non-nil error")
		}
		if genErr.Category != httpclient.ErrCategoryInvalidReq {
			t.Errorf("got Category %v, want %v", genErr.Category, httpclient.ErrCategoryInvalidReq)
		}
		if !strings.Contains(genErr.Message, "endpoint cannot be empty") {
			t.Errorf("expected %q to contain %q", genErr.Message, "endpoint cannot be empty")
		}
	})
}

func testErrorHandling(t *testing.T) {
	t.Run("HTTP status code mapping", func(t *testing.T) {
		tests := []struct {
			name          string
			statusCode    int
			responseBody  map[string]interface{}
			responseText  string
			wantCategory  httpclient.ErrorCategory
			wantRetryable bool
		}{
			// 4xx client errors
			{
				name:       "should handle 400 Bad Request",
				statusCode: http.StatusBadRequest,
				responseBody: map[string]interface{}{
					"error": map[string]interface{}{
						"code":    400,
						"message": "Invalid request parameters",
					},
				},
				wantCategory:  httpclient.ErrCategoryInvalidReq,
				wantRetryable: false,
			},
			{
				name:       "should handle 401 Unauthorized",
				statusCode: http.StatusUnauthorized,
				responseBody: map[string]interface{}{
					"error": map[string]interface{}{
						"code":    401,
						"message": "Invalid API key",
					},
				},
				wantCategory:  httpclient.ErrCategoryAuth,
				wantRetryable: false,
			},
			{
				name:          "should handle 403 Forbidden",
				statusCode:    http.StatusForbidden,
				wantCategory:  httpclient.ErrCategoryAuth,
				wantRetryable: false,
			},
			{
				name:          "should handle 404 Not Found",
				statusCode:    http.StatusNotFound,
				wantCategory:  httpclient.ErrCategoryUnknown,
				wantRetryable: false,
			},
			{
				name:       "should handle 429 Rate Limit",
				statusCode: http.StatusTooManyRequests,
				responseBody: map[string]interface{}{
					"error": map[string]interface{}{
						"code":    429,
						"message": "Rate limit exceeded",
					},
				},
				wantCategory:  httpclient.ErrCategoryRateLimit,
				wantRetryable: true,
			},
			// 5xx server errors
			{
				name:       "should handle 500 Internal Server Error",
				statusCode: http.StatusInternalServerError,
				responseBody: map[string]interface{}{
					"error": map[string]interface{}{
						"code":    500,
						"message": "Internal server error",
					},
				},
				wantCategory:  httpclient.ErrCategoryServer,
				wantRetryable: true,
			},
			{
				name:          "should handle 502 Bad Gateway",
				statusCode:    http.StatusBadGateway,
				wantCategory:  httpclient.ErrCategoryServer,
				wantRetryable: true,
			},
			{
				name:          "should handle 503 Service Unavailable",
				statusCode:    http.StatusServiceUnavailable,
				responseText:  "Service temporarily unavailable",
				wantCategory:  httpclient.ErrCategoryServer,
				wantRetryable: true,
			},
			{
				name:          "should handle 504 Gateway Timeout",
				statusCode:    http.StatusGatewayTimeout,
				wantCategory:  httpclient.ErrCategoryServer,
				wantRetryable: true,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tt.statusCode)
					if tt.responseBody != nil {
						_ = json.NewEncoder(w).Encode(tt.responseBody)
					} else if tt.responseText != "" {
						_, _ = w.Write([]byte(tt.responseText))
					} else {
						// Default error body
						_ = json.NewEncoder(w).Encode(map[string]interface{}{
							"error": map[string]interface{}{
								"message": "Error message",
							},
						})
					}
				}))
				t.Cleanup(testServer.Close)

				client, err := NewInferenceClient(&HTTPClientConfig{BaseURL: testServer.URL})
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				req := &GenerateRequest{
					RequestID: "test",
					Endpoint:  "/v1/chat/completions",
					Params:    map[string]interface{}{"model": "gpt-4"},
				}

				resp, genErr := client.Generate(context.Background(), req)
				if resp != nil {
					t.Errorf("expected nil response, got %v", resp)
				}
				if genErr == nil {
					t.Fatal("expected non-nil error")
				}
				if genErr.Category != tt.wantCategory {
					t.Errorf("got Category %v, want %v", genErr.Category, tt.wantCategory)
				}
				if tt.wantRetryable {
					if !genErr.IsRetryable() {
						t.Error("expected true")
					}
				} else {
					if genErr.IsRetryable() {
						t.Error("expected false")
					}
				}
			})
		}
	})

	t.Run("should handle malformed JSON response", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{invalid json"))
		}))
		t.Cleanup(testServer.Close)

		client, err := NewInferenceClient(&HTTPClientConfig{BaseURL: testServer.URL})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req := &GenerateRequest{
			RequestID: "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		resp, genErr := client.Generate(context.Background(), req)
		// Implementation continues despite JSON parse errors, returning success with nil RawData
		if genErr != nil {
			t.Errorf("expected no error, got %v", genErr)
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		if resp.RequestID != "test" {
			t.Errorf("got RequestID %v, want %v", resp.RequestID, "test")
		}
		if resp.RawData != nil { // RawData should be nil for malformed JSON
			t.Errorf("expected nil RawData, got %v", resp.RawData)
		}
		if resp.Response == nil {
			t.Error("expected non-nil Response")
		}
	})

	t.Run("should handle empty response body", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Header().Set("Content-Type", "application/json")
		}))
		t.Cleanup(testServer.Close)

		client, err := NewInferenceClient(&HTTPClientConfig{BaseURL: testServer.URL})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req := &GenerateRequest{
			RequestID: "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		resp, genErr := client.Generate(context.Background(), req)
		// Implementation handles empty body as successful response
		if genErr != nil {
			t.Errorf("expected no error, got %v", genErr)
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		if resp.RequestID != "test" {
			t.Errorf("got RequestID %v, want %v", resp.RequestID, "test")
		}
		if resp.RawData != nil { // RawData should be nil for empty JSON
			t.Errorf("expected nil RawData, got %v", resp.RawData)
		}
		if resp.Response == nil {
			t.Error("expected non-nil Response")
		}
	})

	t.Run("should handle context cancellation", func(t *testing.T) {
		serverReached := make(chan struct{})
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(serverReached) // Signal that server was reached
			time.Sleep(2 * time.Second)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(testServer.Close)

		client, err := NewInferenceClient(&HTTPClientConfig{BaseURL: testServer.URL})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req := &GenerateRequest{
			RequestID: "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		// Cancel after the server handler is reached
		go func() {
			<-serverReached // Wait for server to be reached
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		resp, genErr := client.Generate(ctx, req)
		elapsed := time.Since(start)

		if resp != nil {
			t.Errorf("expected nil response, got %v", resp)
		}
		if genErr == nil {
			t.Fatal("expected non-nil error")
		}
		if !strings.Contains(genErr.Message, "cancelled") {
			t.Errorf("expected %q to contain %q", genErr.Message, "cancelled")
		}
		// Should cancel quickly after server is reached, not wait for full 2s sleep
		if elapsed >= 500*time.Millisecond {
			t.Errorf("expected %v < %v", elapsed, 500*time.Millisecond)
		}
	})

	t.Run("should handle context timeout", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Second)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(testServer.Close)

		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL: testServer.URL,
			Timeout: 100 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req := &GenerateRequest{
			RequestID: "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		ctx := context.Background()
		resp, genErr := client.Generate(ctx, req)
		if resp != nil {
			t.Errorf("expected nil response, got %v", resp)
		}
		if genErr == nil {
			t.Fatal("expected non-nil error")
		}
		if genErr.Category != httpclient.ErrCategoryServer {
			t.Errorf("got Category %v, want %v", genErr.Category, httpclient.ErrCategoryServer)
		}
	})
}

func testRetryLogic(t *testing.T) {
	t.Run("retry behavior for different error types", func(t *testing.T) {
		tests := []struct {
			name                  string
			statusCode            int
			errorMessage          string
			failuresBeforeSuccess int
			wantAttemptCount      int
			wantSuccess           bool
			wantErrorCategory     httpclient.ErrorCategory
		}{
			{
				name:                  "should retry on rate limit error",
				statusCode:            http.StatusTooManyRequests,
				errorMessage:          "Rate limit exceeded",
				failuresBeforeSuccess: 2,
				wantAttemptCount:      3,
				wantSuccess:           true,
			},
			{
				name:                  "should retry on server error",
				statusCode:            http.StatusInternalServerError,
				errorMessage:          "Internal server error",
				failuresBeforeSuccess: 1,
				wantAttemptCount:      2,
				wantSuccess:           true,
			},
			{
				name:              "should not retry on bad request error",
				statusCode:        http.StatusBadRequest,
				errorMessage:      "Bad request",
				wantAttemptCount:  1,
				wantSuccess:       false,
				wantErrorCategory: httpclient.ErrCategoryInvalidReq,
			},
			{
				name:              "should not retry on auth error",
				statusCode:        http.StatusUnauthorized,
				errorMessage:      "Unauthorized",
				wantAttemptCount:  1,
				wantSuccess:       false,
				wantErrorCategory: httpclient.ErrCategoryAuth,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				attemptCount := 0
				testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attemptCount++
					if tt.wantSuccess && attemptCount <= tt.failuresBeforeSuccess {
						// Return error for retryable tests until we reach the success attempt
						w.WriteHeader(tt.statusCode)
						_ = json.NewEncoder(w).Encode(map[string]interface{}{
							"error": map[string]interface{}{
								"code":    tt.statusCode,
								"message": tt.errorMessage,
							},
						})
					} else if !tt.wantSuccess {
						// Always return error for non-retryable tests
						w.WriteHeader(tt.statusCode)
						_ = json.NewEncoder(w).Encode(map[string]interface{}{
							"error": map[string]interface{}{
								"code":    tt.statusCode,
								"message": tt.errorMessage,
							},
						})
					} else {
						// Return success
						w.WriteHeader(http.StatusOK)
						_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "success"})
					}
				}))
				t.Cleanup(testServer.Close)

				client, err := NewInferenceClient(&HTTPClientConfig{
					BaseURL:        testServer.URL,
					MaxRetries:     3,
					InitialBackoff: 10 * time.Millisecond,
				})
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				req := &GenerateRequest{
					RequestID: "test",
					Endpoint:  "/v1/chat/completions",
					Params:    map[string]interface{}{"model": "gpt-4"},
				}

				resp, genErr := client.Generate(context.Background(), req)
				if attemptCount != tt.wantAttemptCount {
					t.Errorf("got attemptCount %v, want %v", attemptCount, tt.wantAttemptCount)
				}

				if tt.wantSuccess {
					if genErr != nil {
						t.Errorf("expected no error, got %v", genErr)
					}
					if resp == nil {
						t.Error("expected non-nil response")
					}
				} else {
					if resp != nil {
						t.Errorf("expected nil response, got %v", resp)
					}
					if genErr == nil {
						t.Fatal("expected non-nil error")
					}
					if genErr.Category != tt.wantErrorCategory {
						t.Errorf("got Category %v, want %v", genErr.Category, tt.wantErrorCategory)
					}
				}
			})
		}
	})

	t.Run("should respect max retries", func(t *testing.T) {
		attemptCount := 0
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attemptCount++
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    429,
					"message": "Rate limit exceeded",
				},
			})
		}))
		t.Cleanup(testServer.Close)

		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL:        testServer.URL,
			MaxRetries:     2,
			InitialBackoff: 10 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req := &GenerateRequest{
			RequestID: "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		resp, genErr := client.Generate(context.Background(), req)
		if resp != nil {
			t.Errorf("expected nil response, got %v", resp)
		}
		if genErr == nil {
			t.Fatal("expected non-nil error")
		}
		if attemptCount != 3 { // Initial + 2 retries
			t.Errorf("got attemptCount %v, want %v", attemptCount, 3)
		}
	})

	t.Run("should work without retry when MaxRetries is 0", func(t *testing.T) {
		attemptCount := 0
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attemptCount++
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "success"})
		}))
		t.Cleanup(testServer.Close)

		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL:    testServer.URL,
			MaxRetries: 0, // Retry disabled
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req := &GenerateRequest{
			RequestID: "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		resp, genErr := client.Generate(context.Background(), req)
		if genErr != nil {
			t.Errorf("expected no error, got %v", genErr)
		}
		if resp == nil {
			t.Error("expected non-nil response")
		}
		if attemptCount != 1 {
			t.Errorf("got attemptCount %v, want %v", attemptCount, 1)
		}
	})
}

// generateTestCerts creates test certificates in a temporary directory
// Returns: certDir, caCertFile, clientCertFile, clientKeyFile, invalidPemFile
func generateTestCerts(t *testing.T) (string, string, string, string, string) {
	certDir := t.TempDir() // Automatically cleaned up after test

	// Generate CA private key
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate CA key: %v", err)
	}

	// Create CA certificate
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test CA"},
			CommonName:   "Test CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("Failed to create CA certificate: %v", err)
	}

	// Write CA certificate to file
	caCertFile := filepath.Join(certDir, "ca-cert.pem")
	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})
	if err := os.WriteFile(caCertFile, caCertPEM, 0644); err != nil {
		t.Fatalf("Failed to write CA cert: %v", err)
	}

	// Generate client private key
	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate client key: %v", err)
	}

	// Create client certificate
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"Test Client"},
			CommonName:   "Test Client",
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	clientCertDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("Failed to create client certificate: %v", err)
	}

	// Write client certificate to file
	clientCertFile := filepath.Join(certDir, "client-cert.pem")
	clientCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientCertDER})
	if err := os.WriteFile(clientCertFile, clientCertPEM, 0644); err != nil {
		t.Fatalf("Failed to write client cert: %v", err)
	}

	// Write client key to file
	clientKeyFile := filepath.Join(certDir, "client-key.pem")
	clientKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)})
	if err := os.WriteFile(clientKeyFile, clientKeyPEM, 0600); err != nil {
		t.Fatalf("Failed to write client key: %v", err)
	}

	// Create invalid PEM file
	invalidPemFile := filepath.Join(certDir, "invalid.pem")
	if err := os.WriteFile(invalidPemFile, []byte("not a valid pem file"), 0644); err != nil {
		t.Fatalf("Failed to write invalid PEM: %v", err)
	}

	return certDir, caCertFile, clientCertFile, clientKeyFile, invalidPemFile
}

func testTLSConfiguration(t *testing.T) {
	t.Run("should return nil TLS config when no custom options specified", func(t *testing.T) {
		config := HTTPClientConfig{
			BaseURL: "https://localhost:8000",
			// No TLS options specified
		}

		tlsConfig, err := httpclient.BuildTLSConfig(&config)
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
		if tlsConfig != nil {
			t.Errorf("expected nil TLS config to use Go's default, got %v", tlsConfig)
		}
	})

	t.Run("should use secure TLS defaults when InsecureSkipVerify is false", func(t *testing.T) {
		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL:               "https://localhost:8000",
			TLSInsecureSkipVerify: false, // Default: use system root CAs
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if client == nil {
			t.Fatal("expected non-nil client")
		}

		// Client created successfully with default TLS settings (system root CAs)
	})

	t.Run("should disable certificate verification when InsecureSkipVerify is true", func(t *testing.T) {
		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL:               "https://localhost:8443",
			TLSInsecureSkipVerify: true, // Skip cert verification for testing
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if client == nil {
			t.Fatal("expected non-nil client")
		}

		// Client created successfully with InsecureSkipVerify enabled
	})

	t.Run("should load custom CA certificate", func(t *testing.T) {
		_, caCertFile, _, _, _ := generateTestCerts(t)

		config := HTTPClientConfig{
			BaseURL:       "https://localhost:8000",
			TLSCACertFile: caCertFile,
		}

		tlsConfig, err := httpclient.BuildTLSConfig(&config)
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
		if tlsConfig == nil {
			t.Fatal("expected non-nil TLS config for custom CA")
		}
		if tlsConfig.RootCAs == nil {
			t.Error("expected non-nil RootCAs")
		}
		if tlsConfig.InsecureSkipVerify {
			t.Error("expected false")
		}
	})

	t.Run("should load client certificate and key for mTLS", func(t *testing.T) {
		_, _, clientCertFile, clientKeyFile, _ := generateTestCerts(t)

		config := HTTPClientConfig{
			BaseURL:           "https://localhost:8000",
			TLSClientCertFile: clientCertFile,
			TLSClientKeyFile:  clientKeyFile,
		}

		tlsConfig, err := httpclient.BuildTLSConfig(&config)
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
		if tlsConfig == nil {
			t.Fatal("expected non-nil TLS config for mTLS")
		}
		if len(tlsConfig.Certificates) != 1 {
			t.Errorf("got %d certificates, want 1", len(tlsConfig.Certificates))
		}
	})

	t.Run("should combine custom CA and mTLS", func(t *testing.T) {
		_, caCertFile, clientCertFile, clientKeyFile, _ := generateTestCerts(t)

		config := HTTPClientConfig{
			BaseURL:           "https://localhost:8000",
			TLSCACertFile:     caCertFile,
			TLSClientCertFile: clientCertFile,
			TLSClientKeyFile:  clientKeyFile,
		}

		tlsConfig, err := httpclient.BuildTLSConfig(&config)
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
		if tlsConfig == nil {
			t.Fatal("expected non-nil TLS config")
		}
		if tlsConfig.RootCAs == nil {
			t.Error("expected non-nil RootCAs")
		}
		if len(tlsConfig.Certificates) != 1 {
			t.Errorf("got %d certificates, want 1", len(tlsConfig.Certificates))
		}
	})

	t.Run("should set TLS version constraints", func(t *testing.T) {
		config := HTTPClientConfig{
			BaseURL:       "https://localhost:8000",
			TLSMinVersion: tls.VersionTLS12,
			TLSMaxVersion: tls.VersionTLS13,
		}

		tlsConfig, err := httpclient.BuildTLSConfig(&config)
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
		if tlsConfig == nil {
			t.Fatal("expected non-nil TLS config for version constraints")
		}
		if tlsConfig.MinVersion != uint16(tls.VersionTLS12) {
			t.Errorf("got MinVersion %v, want %v", tlsConfig.MinVersion, uint16(tls.VersionTLS12))
		}
		if tlsConfig.MaxVersion != uint16(tls.VersionTLS13) {
			t.Errorf("got MaxVersion %v, want %v", tlsConfig.MaxVersion, uint16(tls.VersionTLS13))
		}
	})

	t.Run("should fail with missing CA certificate file", func(t *testing.T) {
		certDir := t.TempDir()

		config := HTTPClientConfig{
			BaseURL:       "https://localhost:8000",
			TLSCACertFile: filepath.Join(certDir, "nonexistent.pem"),
		}

		tlsConfig, err := httpclient.BuildTLSConfig(&config)
		if err == nil {
			t.Error("expected non-nil error for missing CA cert file")
		}
		if tlsConfig != nil {
			t.Errorf("expected nil TLS config, got %v", tlsConfig)
		}
		if err != nil && !strings.Contains(err.Error(), "failed to read CA certificate file") {
			t.Errorf("expected %q to contain %q", err.Error(), "failed to read CA certificate file")
		}
	})

	t.Run("should fail with invalid CA certificate PEM", func(t *testing.T) {
		_, _, _, _, invalidPemFile := generateTestCerts(t)

		config := HTTPClientConfig{
			BaseURL:       "https://localhost:8000",
			TLSCACertFile: invalidPemFile,
		}

		tlsConfig, err := httpclient.BuildTLSConfig(&config)
		if err == nil {
			t.Error("expected non-nil error for invalid PEM")
		}
		if tlsConfig != nil {
			t.Errorf("expected nil TLS config, got %v", tlsConfig)
		}
		if err != nil && !strings.Contains(err.Error(), "failed to parse CA certificate") {
			t.Errorf("expected %q to contain %q", err.Error(), "failed to parse CA certificate")
		}
	})

	t.Run("should fail with missing client certificate file", func(t *testing.T) {
		certDir, _, _, clientKeyFile, _ := generateTestCerts(t)

		config := HTTPClientConfig{
			BaseURL:           "https://localhost:8000",
			TLSClientCertFile: filepath.Join(certDir, "nonexistent-cert.pem"),
			TLSClientKeyFile:  clientKeyFile,
		}

		tlsConfig, err := httpclient.BuildTLSConfig(&config)
		if err == nil {
			t.Error("expected non-nil error for missing client cert")
		}
		if tlsConfig != nil {
			t.Errorf("expected nil TLS config, got %v", tlsConfig)
		}
		if err != nil && !strings.Contains(err.Error(), "failed to load client certificate/key pair") {
			t.Errorf("expected %q to contain %q", err.Error(), "failed to load client certificate/key pair")
		}
	})

	t.Run("should fail with incomplete mTLS config - cert without key", func(t *testing.T) {
		_, _, clientCertFile, _, _ := generateTestCerts(t)

		config := HTTPClientConfig{
			BaseURL:           "https://localhost:8000",
			TLSClientCertFile: clientCertFile,
			// Missing TLSClientKeyFile
		}

		tlsConfig, err := httpclient.BuildTLSConfig(&config)
		if err == nil {
			t.Error("expected non-nil error for incomplete mTLS config")
		}
		if tlsConfig != nil {
			t.Errorf("expected nil TLS config, got %v", tlsConfig)
		}
		if err != nil && !strings.Contains(err.Error(), "both TLSClientCertFile and TLSClientKeyFile must be specified") {
			t.Errorf("expected %q to contain %q", err.Error(), "both TLSClientCertFile and TLSClientKeyFile must be specified")
		}
	})

	t.Run("should fail with incomplete mTLS config - key without cert", func(t *testing.T) {
		_, _, _, clientKeyFile, _ := generateTestCerts(t)

		config := HTTPClientConfig{
			BaseURL:          "https://localhost:8000",
			TLSClientKeyFile: clientKeyFile,
			// Missing TLSClientCertFile
		}

		tlsConfig, err := httpclient.BuildTLSConfig(&config)
		if err == nil {
			t.Error("expected non-nil error for incomplete mTLS config")
		}
		if tlsConfig != nil {
			t.Errorf("expected nil TLS config, got %v", tlsConfig)
		}
		if err != nil && !strings.Contains(err.Error(), "both TLSClientCertFile and TLSClientKeyFile must be specified") {
			t.Errorf("expected %q to contain %q", err.Error(), "both TLSClientCertFile and TLSClientKeyFile must be specified")
		}
	})

	t.Run("should create client with all TLS options combined", func(t *testing.T) {
		_, caCertFile, clientCertFile, clientKeyFile, _ := generateTestCerts(t)

		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL:           "https://localhost:8000",
			TLSCACertFile:     caCertFile,
			TLSClientCertFile: clientCertFile,
			TLSClientKeyFile:  clientKeyFile,
			TLSMinVersion:     tls.VersionTLS12,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if client == nil {
			t.Fatal("expected non-nil client")
		}

		// Client created successfully with all TLS options combined
	})
}

func testAuthentication(t *testing.T) {
	t.Run("should include API key in Authorization header", func(t *testing.T) {
		var authHeader string
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "test"})
		}))
		t.Cleanup(testServer.Close)

		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL: testServer.URL,
			APIKey:  "sk-test-key-123",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req := &GenerateRequest{
			RequestID: "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		_, _ = client.Generate(context.Background(), req)
		if authHeader != "Bearer sk-test-key-123" {
			t.Errorf("got Authorization %v, want %v", authHeader, "Bearer sk-test-key-123")
		}
	})

	t.Run("should not include Authorization header when API key is empty", func(t *testing.T) {
		var authHeader string
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "test"})
		}))
		t.Cleanup(testServer.Close)

		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL: testServer.URL,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req := &GenerateRequest{
			RequestID: "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		_, _ = client.Generate(context.Background(), req)
		if authHeader != "" {
			t.Errorf("expected empty Authorization header, got %q", authHeader)
		}
	})
}

func testNetworkErrors(t *testing.T) {
	t.Run("should handle connection refused", func(t *testing.T) {
		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL:        "http://localhost:9999", // Non-existent server
			Timeout:        1 * time.Second,
			MaxRetries:     2,
			InitialBackoff: 10 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		resp, genErr := client.Generate(context.Background(), &GenerateRequest{
			RequestID: "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "test"},
		})

		if resp != nil {
			t.Errorf("expected nil response, got %v", resp)
		}
		if genErr == nil {
			t.Fatal("expected non-nil error")
		}
		if genErr.Category != httpclient.ErrCategoryServer {
			t.Errorf("got Category %v, want %v", genErr.Category, httpclient.ErrCategoryServer)
		}
		if !genErr.IsRetryable() {
			t.Error("expected true")
		}
		if !strings.Contains(genErr.Message, "failed to execute request") {
			t.Errorf("expected %q to contain %q", genErr.Message, "failed to execute request")
		}
	})

	t.Run("should handle DNS resolution failure", func(t *testing.T) {
		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL:        "http://nonexistent.invalid.domain.local",
			Timeout:        1 * time.Second,
			MaxRetries:     1,
			InitialBackoff: 10 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		resp, genErr := client.Generate(context.Background(), &GenerateRequest{
			RequestID: "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "test"},
		})

		if resp != nil {
			t.Errorf("expected nil response, got %v", resp)
		}
		if genErr == nil {
			t.Fatal("expected non-nil error")
		}
		if genErr.Category != httpclient.ErrCategoryServer {
			t.Errorf("got Category %v, want %v", genErr.Category, httpclient.ErrCategoryServer)
		}
	})

	t.Run("should retry and recover from connection close", func(t *testing.T) {
		attemptCount := 0

		// Create a server that closes connection abruptly
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attemptCount++
			if attemptCount < 2 {
				// Close connection without response
				hj, ok := w.(http.Hijacker)
				if ok {
					conn, _, _ := hj.Hijack()
					conn.Close()
				}
			} else {
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "success"})
			}
		}))
		t.Cleanup(testServer.Close)

		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL:        testServer.URL,
			MaxRetries:     3,
			InitialBackoff: 10 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		resp, genErr := client.Generate(context.Background(), &GenerateRequest{
			RequestID: "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "test"},
		})

		// Should eventually succeed after retries
		if genErr != nil {
			t.Errorf("expected no error, got %v", genErr)
		}
		if resp == nil {
			t.Error("expected non-nil response")
		}
		if attemptCount < 2 {
			t.Errorf("expected %v >= %v", attemptCount, 2)
		}
	})
}
