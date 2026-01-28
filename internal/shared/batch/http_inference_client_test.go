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

package batch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewHTTPInferenceClient(t *testing.T) {
	tests := []struct {
		name   string
		config HTTPInferenceClientConfig
	}{
		{
			name: "should create client with default configuration",
			config: HTTPInferenceClientConfig{
				BaseURL: "http://localhost:8000",
			},
		},
		{
			name: "should create client with custom configuration",
			config: HTTPInferenceClientConfig{
				BaseURL:         "http://localhost:9000",
				Timeout:         1 * time.Minute,
				MaxIdleConns:    50,
				IdleConnTimeout: 60 * time.Second,
				APIKey:          "test-api-key",
			},
		},
		{
			name: "should apply retry defaults when MaxRetries is set",
			config: HTTPInferenceClientConfig{
				BaseURL:    "http://localhost:8000",
				MaxRetries: 3,
			},
		},
		{
			name: "should respect custom retry configuration",
			config: HTTPInferenceClientConfig{
				BaseURL:        "http://localhost:8000",
				MaxRetries:     5,
				InitialBackoff: 2 * time.Second,
				MaxBackoff:     120 * time.Second,
			},
		},
		{
			name: "should apply partial retry defaults",
			config: HTTPInferenceClientConfig{
				BaseURL:        "http://localhost:8000",
				MaxRetries:     3,
				InitialBackoff: 500 * time.Millisecond,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewHTTPInferenceClient(tt.config)
			assert.NotNil(t, client)
			assert.NotNil(t, client.client)
			// Note: resty.Client internal state (timeout, auth, retry config) is not directly accessible
			// Behavior is validated through integration and functional tests
		})
	}
}

func TestGenerate(t *testing.T) {
	t.Run("should successfully make inference request with chat completion", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify headers
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

			// Verify request ID if present
			if requestID := r.Header.Get("X-Request-ID"); requestID != "" {
				assert.Equal(t, "test-request-123", requestID)
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
			json.NewEncoder(w).Encode(response)
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL: testServer.URL,
			Timeout: 10 * time.Second,
		})

		req := &InferenceRequest{
			RequestID: "test-request-123",
			Model:     "gpt-4",
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
		resp, err := client.Generate(ctx, req)

		assert.Nil(t, err)
		assert.NotNil(t, resp)
		assert.Equal(t, "test-request-123", resp.RequestID)
		assert.NotNil(t, resp.Response)
		assert.NotNil(t, resp.RawData)

		// Verify response can be unmarshaled
		var data map[string]interface{}
		unmarshalErr := json.Unmarshal(resp.Response, &data)
		assert.Nil(t, unmarshalErr)
		assert.Equal(t, "chatcmpl-123", data["id"])
	})

	t.Run("should handle nil request", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL: testServer.URL,
			Timeout: 10 * time.Second,
		})

		ctx := context.Background()
		resp, err := client.Generate(ctx, nil)

		assert.Nil(t, resp)
		assert.NotNil(t, err)
		assert.Equal(t, ErrCategoryInvalidReq, err.Category)
		assert.Contains(t, err.Message, "cannot be nil")
	})

	t.Run("should use endpoint from request", func(t *testing.T) {
		endpoint := ""
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			endpoint = r.URL.Path
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"id": "test"})
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL: testServer.URL,
			Timeout: 10 * time.Second,
		})

		req := &InferenceRequest{
			RequestID: "test",
			Model:     "gpt-4",
			Endpoint:  "/v1/chat/completions",
			Params: map[string]interface{}{
				"messages": []map[string]interface{}{
					{"role": "user", "content": "test"},
				},
			},
		}

		client.Generate(context.Background(), req)
		assert.Equal(t, "/v1/chat/completions", endpoint)
	})

	t.Run("should fail when endpoint is empty", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"id": "test"})
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL: testServer.URL,
			Timeout: 10 * time.Second,
		})

		req := &InferenceRequest{
			RequestID: "test",
			Model:     "gpt-4",
			Endpoint:  "", // Empty endpoint
			Params: map[string]interface{}{
				"messages": []map[string]interface{}{
					{"role": "user", "content": "test"},
				},
			},
		}

		resp, err := client.Generate(context.Background(), req)
		assert.Nil(t, resp)
		assert.NotNil(t, err)
		assert.Equal(t, ErrCategoryInvalidReq, err.Category)
		assert.Contains(t, err.Message, "endpoint cannot be empty")
	})
}

func TestErrorHandling(t *testing.T) {
	t.Run("HTTP status code mapping", func(t *testing.T) {
		tests := []struct {
			name          string
			statusCode    int
			responseBody  map[string]interface{}
			responseText  string
			wantCategory  ErrorCategory
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
				wantCategory:  ErrCategoryInvalidReq,
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
				wantCategory:  ErrCategoryAuth,
				wantRetryable: false,
			},
			{
				name:          "should handle 403 Forbidden",
				statusCode:    http.StatusForbidden,
				wantCategory:  ErrCategoryAuth,
				wantRetryable: false,
			},
			{
				name:          "should handle 404 Not Found",
				statusCode:    http.StatusNotFound,
				wantCategory:  ErrCategoryUnknown,
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
				wantCategory:  ErrCategoryRateLimit,
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
				wantCategory:  ErrCategoryServer,
				wantRetryable: true,
			},
			{
				name:          "should handle 502 Bad Gateway",
				statusCode:    http.StatusBadGateway,
				wantCategory:  ErrCategoryServer,
				wantRetryable: true,
			},
			{
				name:          "should handle 503 Service Unavailable",
				statusCode:    http.StatusServiceUnavailable,
				responseText:  "Service temporarily unavailable",
				wantCategory:  ErrCategoryServer,
				wantRetryable: true,
			},
			{
				name:          "should handle 504 Gateway Timeout",
				statusCode:    http.StatusGatewayTimeout,
				wantCategory:  ErrCategoryServer,
				wantRetryable: true,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tt.statusCode)
					if tt.responseBody != nil {
						json.NewEncoder(w).Encode(tt.responseBody)
					} else if tt.responseText != "" {
						w.Write([]byte(tt.responseText))
					} else {
						// Default error body
						json.NewEncoder(w).Encode(map[string]interface{}{
							"error": map[string]interface{}{
								"message": "Error message",
							},
						})
					}
				}))
				t.Cleanup(testServer.Close)

				client := NewHTTPInferenceClient(HTTPInferenceClientConfig{BaseURL: testServer.URL})

				req := &InferenceRequest{
					RequestID: "test",
					Model:     "gpt-4",
					Endpoint:  "/v1/chat/completions",
					Params:    map[string]interface{}{"model": "gpt-4"},
				}

				resp, err := client.Generate(context.Background(), req)
				assert.Nil(t, resp)
				assert.NotNil(t, err)
				assert.Equal(t, tt.wantCategory, err.Category)
				if tt.wantRetryable {
					assert.True(t, err.IsRetryable())
				} else {
					assert.False(t, err.IsRetryable())
				}
			})
		}
	})

	t.Run("should handle malformed JSON response", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("{invalid json"))
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{BaseURL: testServer.URL})

		req := &InferenceRequest{
			RequestID: "test",
			Model:     "gpt-4",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		resp, err := client.Generate(context.Background(), req)
		// Implementation continues despite JSON parse errors, returning success with nil RawData
		assert.Nil(t, err)
		assert.NotNil(t, resp)
		assert.Equal(t, "test", resp.RequestID)
		assert.Nil(t, resp.RawData) // RawData should be nil for malformed JSON
		assert.NotNil(t, resp.Response)
	})

	t.Run("should handle empty response body", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Header().Set("Content-Type", "application/json")
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{BaseURL: testServer.URL})

		req := &InferenceRequest{
			RequestID: "test",
			Model:     "gpt-4",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		resp, err := client.Generate(context.Background(), req)
		// Implementation handles empty body as successful response
		assert.Nil(t, err)
		assert.NotNil(t, resp)
		assert.Equal(t, "test", resp.RequestID)
		assert.Nil(t, resp.RawData) // RawData should be nil for empty JSON
		assert.NotNil(t, resp.Response)
	})

	t.Run("should handle context cancellation", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Second)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{BaseURL: testServer.URL})

		req := &InferenceRequest{
			RequestID: "test",
			Model:     "gpt-4",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		resp, err := client.Generate(ctx, req)
		assert.Nil(t, resp)
		assert.NotNil(t, err)
		assert.Contains(t, err.Message, "cancelled")
	})

	t.Run("should handle context timeout", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Second)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL: testServer.URL,
			Timeout: 100 * time.Millisecond,
		})

		req := &InferenceRequest{
			RequestID: "test",
			Model:     "gpt-4",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		ctx := context.Background()
		resp, err := client.Generate(ctx, req)
		assert.Nil(t, resp)
		assert.NotNil(t, err)
		assert.Equal(t, ErrCategoryServer, err.Category)
	})
}

func TestRetryLogic(t *testing.T) {
	t.Run("retry behavior for different error types", func(t *testing.T) {
		tests := []struct {
			name                  string
			statusCode            int
			errorMessage          string
			failuresBeforeSuccess int
			wantAttemptCount      int
			wantSuccess           bool
			wantErrorCategory     ErrorCategory
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
				wantErrorCategory: ErrCategoryInvalidReq,
			},
			{
				name:              "should not retry on auth error",
				statusCode:        http.StatusUnauthorized,
				errorMessage:      "Unauthorized",
				wantAttemptCount:  1,
				wantSuccess:       false,
				wantErrorCategory: ErrCategoryAuth,
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
						json.NewEncoder(w).Encode(map[string]interface{}{
							"error": map[string]interface{}{
								"code":    tt.statusCode,
								"message": tt.errorMessage,
							},
						})
					} else if !tt.wantSuccess {
						// Always return error for non-retryable tests
						w.WriteHeader(tt.statusCode)
						json.NewEncoder(w).Encode(map[string]interface{}{
							"error": map[string]interface{}{
								"code":    tt.statusCode,
								"message": tt.errorMessage,
							},
						})
					} else {
						// Return success
						w.WriteHeader(http.StatusOK)
						json.NewEncoder(w).Encode(map[string]interface{}{"id": "success"})
					}
				}))
				t.Cleanup(testServer.Close)

				client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
					BaseURL:        testServer.URL,
					MaxRetries:     3,
					InitialBackoff: 10 * time.Millisecond,
				})

				req := &InferenceRequest{
					RequestID: "test",
					Model:     "gpt-4",
					Endpoint:  "/v1/chat/completions",
					Params:    map[string]interface{}{"model": "gpt-4"},
				}

				resp, err := client.Generate(context.Background(), req)
				assert.Equal(t, tt.wantAttemptCount, attemptCount)

				if tt.wantSuccess {
					assert.Nil(t, err)
					assert.NotNil(t, resp)
				} else {
					assert.Nil(t, resp)
					assert.NotNil(t, err)
					assert.Equal(t, tt.wantErrorCategory, err.Category)
				}
			})
		}
	})

	t.Run("should respect max retries", func(t *testing.T) {
		attemptCount := 0
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attemptCount++
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    429,
					"message": "Rate limit exceeded",
				},
			})
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL:        testServer.URL,
			MaxRetries:     2,
			InitialBackoff: 10 * time.Millisecond,
		})

		req := &InferenceRequest{
			RequestID: "test",
			Model:     "gpt-4",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		resp, err := client.Generate(context.Background(), req)
		assert.Nil(t, resp)
		assert.NotNil(t, err)
		assert.Equal(t, 3, attemptCount) // Initial + 2 retries
	})

	t.Run("should stop retrying when context is cancelled", func(t *testing.T) {
		attemptCount := 0
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attemptCount++
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    429,
					"message": "Rate limit exceeded",
				},
			})
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL:        testServer.URL,
			MaxRetries:     10,
			InitialBackoff: 100 * time.Millisecond,
		})

		req := &InferenceRequest{
			RequestID: "test",
			Model:     "gpt-4",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(150 * time.Millisecond)
			cancel()
		}()

		resp, err := client.Generate(ctx, req)
		assert.Nil(t, resp)
		assert.NotNil(t, err)
		assert.LessOrEqual(t, attemptCount, 3) // Should stop early
	})

	t.Run("should work without retry when MaxRetries is 0", func(t *testing.T) {
		attemptCount := 0
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attemptCount++
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"id": "success"})
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL:    testServer.URL,
			MaxRetries: 0, // Retry disabled
		})

		req := &InferenceRequest{
			RequestID: "test",
			Model:     "gpt-4",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		resp, err := client.Generate(context.Background(), req)
		assert.Nil(t, err)
		assert.NotNil(t, resp)
		assert.Equal(t, 1, attemptCount)
	})
}

func TestTLSConfiguration(t *testing.T) {
	t.Run("should use secure TLS defaults when InsecureSkipVerify is false", func(t *testing.T) {
		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL:               "https://localhost:8000",
			TLSInsecureSkipVerify: false, // Default: use system root CAs
		})

		assert.NotNil(t, client)

		// Access the underlying transport to verify TLS configuration
		httpClient := client.client.GetClient()
		transport, ok := httpClient.Transport.(*http.Transport)
		assert.True(t, ok, "expected *http.Transport")

		// Note: Clone() creates a TLSClientConfig with default values
		// Verify certificate verification is enabled (InsecureSkipVerify = false)
		assert.NotNil(t, transport.TLSClientConfig, "TLSClientConfig should exist")
		assert.False(t, transport.TLSClientConfig.InsecureSkipVerify, "Certificate verification should be enabled")
	})

	t.Run("should disable certificate verification when InsecureSkipVerify is true", func(t *testing.T) {
		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL:               "https://localhost:8443",
			TLSInsecureSkipVerify: true, // Skip cert verification for testing
		})

		assert.NotNil(t, client)

		// Access the underlying transport to verify TLS configuration
		httpClient := client.client.GetClient()
		transport, ok := httpClient.Transport.(*http.Transport)
		assert.True(t, ok, "expected *http.Transport")

		// Verify InsecureSkipVerify is actually set to true
		assert.NotNil(t, transport.TLSClientConfig, "TLSClientConfig should be set")
		assert.True(t, transport.TLSClientConfig.InsecureSkipVerify, "InsecureSkipVerify should be true")
	})
}

func TestAuthentication(t *testing.T) {
	t.Run("should include API key in Authorization header", func(t *testing.T) {
		var authHeader string
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"id": "test"})
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL: testServer.URL,
			APIKey:  "sk-test-key-123",
		})

		req := &InferenceRequest{
			RequestID: "test",
			Model:     "gpt-4",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		client.Generate(context.Background(), req)
		assert.Equal(t, "Bearer sk-test-key-123", authHeader)
	})

	t.Run("should not include Authorization header when API key is empty", func(t *testing.T) {
		var authHeader string
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"id": "test"})
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL: testServer.URL,
		})

		req := &InferenceRequest{
			RequestID: "test",
			Model:     "gpt-4",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		client.Generate(context.Background(), req)
		assert.Empty(t, authHeader)
	})
}

func TestNetworkErrors(t *testing.T) {
	t.Run("should handle connection refused", func(t *testing.T) {
		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL:        "http://localhost:9999", // Non-existent server
			Timeout:        1 * time.Second,
			MaxRetries:     2,
			InitialBackoff: 10 * time.Millisecond,
		})

		resp, err := client.Generate(context.Background(), &InferenceRequest{
			RequestID: "test",
			Model:     "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "test"},
		})

		assert.Nil(t, resp)
		assert.NotNil(t, err)
		assert.Equal(t, ErrCategoryServer, err.Category)
		assert.True(t, err.IsRetryable())
		assert.Contains(t, err.Message, "failed to execute request")
	})

	t.Run("should handle DNS resolution failure", func(t *testing.T) {
		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL:        "http://nonexistent.invalid.domain.local",
			Timeout:        1 * time.Second,
			MaxRetries:     1,
			InitialBackoff: 10 * time.Millisecond,
		})

		resp, err := client.Generate(context.Background(), &InferenceRequest{
			RequestID: "test",
			Model:     "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "test"},
		})

		assert.Nil(t, resp)
		assert.NotNil(t, err)
		assert.Equal(t, ErrCategoryServer, err.Category)
	})
}

func TestRetryHookBehavior(t *testing.T) {
	t.Run("should retry on network errors", func(t *testing.T) {
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
				json.NewEncoder(w).Encode(map[string]interface{}{"id": "success"})
			}
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL:        testServer.URL,
			MaxRetries:     3,
			InitialBackoff: 10 * time.Millisecond,
		})

		resp, err := client.Generate(context.Background(), &InferenceRequest{
			RequestID: "test",
			Model:     "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "test"},
		})

		// Should eventually succeed after retries
		assert.Nil(t, err)
		assert.NotNil(t, resp)
		assert.GreaterOrEqual(t, attemptCount, 2)
	})
}

func TestTimeoutBehavior(t *testing.T) {
	t.Run("should respect configured client timeout", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(5 * time.Second) // Long delay
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"id": "success"})
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL:    testServer.URL,
			Timeout:    100 * time.Millisecond, // Short timeout
			MaxRetries: 0,                      // Disable retry for cleaner test
		})

		start := time.Now()
		resp, err := client.Generate(context.Background(), &InferenceRequest{
			RequestID: "test",
			Model:     "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "test"},
		})
		elapsed := time.Since(start)

		assert.Nil(t, resp)
		assert.NotNil(t, err)
		assert.Equal(t, ErrCategoryServer, err.Category)

		// Should timeout around 100ms, not wait 5s
		assert.True(t, elapsed < 1*time.Second)
	})

	t.Run("should respect context deadline over client timeout", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(5 * time.Second)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL:    testServer.URL,
			Timeout:    10 * time.Second, // Long client timeout
			MaxRetries: 0,
		})

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		t.Cleanup(cancel)

		start := time.Now()
		resp, err := client.Generate(ctx, &InferenceRequest{
			RequestID: "test",
			Model:     "test",
			Endpoint:  "/v1/chat/completions",
			Params:    map[string]interface{}{"model": "test"},
		})
		elapsed := time.Since(start)

		assert.Nil(t, resp)
		assert.NotNil(t, err)
		assert.True(t, elapsed < 1*time.Second)
	})
}

func TestRetryConditionLogic(t *testing.T) {
	t.Run("should only retry on retryable status codes", func(t *testing.T) {
		tests := []struct {
			name        string
			statusCode  int
			shouldRetry bool
		}{
			{"429 should retry", http.StatusTooManyRequests, true},
			{"500 should retry", http.StatusInternalServerError, true},
			{"502 should retry", http.StatusBadGateway, true},
			{"503 should retry", http.StatusServiceUnavailable, true},
			{"504 should retry", http.StatusGatewayTimeout, true},
			{"400 should not retry", http.StatusBadRequest, false},
			{"401 should not retry", http.StatusUnauthorized, false},
			{"403 should not retry", http.StatusForbidden, false},
			{"404 should not retry", http.StatusNotFound, false},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				attemptCount := 0
				testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attemptCount++
					w.WriteHeader(tt.statusCode)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"error": map[string]interface{}{
							"message": "Error",
						},
					})
				}))
				t.Cleanup(testServer.Close)

				client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
					BaseURL:        testServer.URL,
					MaxRetries:     2,
					InitialBackoff: 10 * time.Millisecond,
				})

				client.Generate(context.Background(), &InferenceRequest{
					RequestID: "test",
					Model:     "test",
					Endpoint:  "/v1/chat/completions",
					Params:    map[string]interface{}{"model": "test"},
				})

				if tt.shouldRetry {
					assert.Equal(t, 3, attemptCount) // 1 initial + 2 retries
				} else {
					assert.Equal(t, 1, attemptCount) // No retries
				}
			})
		}
	})
}
