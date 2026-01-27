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
	"reflect"
	"strings"
	"testing"
	"time"
)

// Test helper functions

func assertEqual(t *testing.T, got, want interface{}, msgAndArgs ...interface{}) {
	t.Helper()
	if got != want {
		msg := ""
		if len(msgAndArgs) > 0 {
			if format, ok := msgAndArgs[0].(string); ok {
				msg = format
			}
		}
		if msg != "" {
			t.Errorf("%s: got %v, want %v", msg, got, want)
		} else {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

func assertNotNil(t *testing.T, obj interface{}, msgAndArgs ...interface{}) {
	t.Helper()
	if obj == nil {
		msg := ""
		if len(msgAndArgs) > 0 {
			if format, ok := msgAndArgs[0].(string); ok {
				msg = format
			}
		}
		if msg != "" {
			t.Errorf("%s: expected non-nil value", msg)
		} else {
			t.Errorf("expected non-nil value")
		}
	}
}

func assertNil(t *testing.T, obj interface{}, msgAndArgs ...interface{}) {
	t.Helper()
	// Use reflection to properly check for nil, including typed nil pointers
	if obj == nil {
		return
	}

	v := reflect.ValueOf(obj)
	if v.Kind() == reflect.Ptr && v.IsNil() {
		return
	}

	msg := ""
	if len(msgAndArgs) > 0 {
		if format, ok := msgAndArgs[0].(string); ok {
			msg = format
		}
	}
	if msg != "" {
		t.Errorf("%s: expected nil, got %v", msg, obj)
	} else {
		t.Errorf("expected nil, got %v", obj)
	}
}

func assertTrue(t *testing.T, condition bool, msgAndArgs ...interface{}) {
	t.Helper()
	if !condition {
		msg := ""
		if len(msgAndArgs) > 0 {
			if format, ok := msgAndArgs[0].(string); ok {
				msg = format
			}
		}
		if msg != "" {
			t.Errorf("%s: expected true", msg)
		} else {
			t.Errorf("expected true")
		}
	}
}

func assertFalse(t *testing.T, condition bool, msgAndArgs ...interface{}) {
	t.Helper()
	if condition {
		msg := ""
		if len(msgAndArgs) > 0 {
			if format, ok := msgAndArgs[0].(string); ok {
				msg = format
			}
		}
		if msg != "" {
			t.Errorf("%s: expected false", msg)
		} else {
			t.Errorf("expected false")
		}
	}
}

func assertContains(t *testing.T, s, substr string, msgAndArgs ...interface{}) {
	t.Helper()
	if !strings.Contains(s, substr) {
		msg := ""
		if len(msgAndArgs) > 0 {
			if format, ok := msgAndArgs[0].(string); ok {
				msg = format
			}
		}
		if msg != "" {
			t.Errorf("%s: string %q does not contain %q", msg, s, substr)
		} else {
			t.Errorf("string %q does not contain %q", s, substr)
		}
	}
}

func assertEmpty(t *testing.T, s string, msgAndArgs ...interface{}) {
	t.Helper()
	if s != "" {
		msg := ""
		if len(msgAndArgs) > 0 {
			if format, ok := msgAndArgs[0].(string); ok {
				msg = format
			}
		}
		if msg != "" {
			t.Errorf("%s: expected empty string, got %q", msg, s)
		} else {
			t.Errorf("expected empty string, got %q", s)
		}
	}
}

func assertNotEmpty(t *testing.T, s string, msgAndArgs ...interface{}) {
	t.Helper()
	if s == "" {
		msg := ""
		if len(msgAndArgs) > 0 {
			if format, ok := msgAndArgs[0].(string); ok {
				msg = format
			}
		}
		if msg != "" {
			t.Errorf("%s: expected non-empty string", msg)
		} else {
			t.Errorf("expected non-empty string")
		}
	}
}

func assertGreaterOrEqual(t *testing.T, actual, expected int, msgAndArgs ...interface{}) {
	t.Helper()
	if actual < expected {
		msg := ""
		if len(msgAndArgs) > 0 {
			if format, ok := msgAndArgs[0].(string); ok {
				msg = format
			}
		}
		if msg != "" {
			t.Errorf("%s: expected %d >= %d", msg, actual, expected)
		} else {
			t.Errorf("expected %d >= %d", actual, expected)
		}
	}
}

func assertLessOrEqual(t *testing.T, actual, expected int, msgAndArgs ...interface{}) {
	t.Helper()
	if actual > expected {
		msg := ""
		if len(msgAndArgs) > 0 {
			if format, ok := msgAndArgs[0].(string); ok {
				msg = format
			}
		}
		if msg != "" {
			t.Errorf("%s: expected %d <= %d", msg, actual, expected)
		} else {
			t.Errorf("expected %d <= %d", actual, expected)
		}
	}
}

func assertDurationGreaterOrEqual(t *testing.T, actual, expected time.Duration, msgAndArgs ...interface{}) {
	t.Helper()
	if actual < expected {
		msg := ""
		if len(msgAndArgs) > 0 {
			if format, ok := msgAndArgs[0].(string); ok {
				msg = format
			}
		}
		if msg != "" {
			t.Errorf("%s: expected %v >= %v", msg, actual, expected)
		} else {
			t.Errorf("expected %v >= %v", actual, expected)
		}
	}
}

func TestNewHTTPInferenceClient(t *testing.T) {
	tests := []struct {
		name                    string
		config                  HTTPInferenceClientConfig
		wantBaseURL             string
		wantTimeout             time.Duration
		wantAPIKey              string
		wantMaxRetries          int
		wantInitialBackoff      time.Duration
		wantMaxBackoff          time.Duration
		wantBackoffFactor       float64
		wantJitterFraction      float64
	}{
		{
			name: "should create client with default configuration",
			config: HTTPInferenceClientConfig{
				BaseURL: "http://localhost:8000",
			},
			wantBaseURL:        "http://localhost:8000",
			wantTimeout:        5 * time.Minute,
			wantAPIKey:         "",
			wantMaxRetries:     0,
			wantInitialBackoff: 0,
			wantMaxBackoff:     0,
			wantBackoffFactor:  0,
			wantJitterFraction: 0,
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
			wantBaseURL:        "http://localhost:9000",
			wantTimeout:        1 * time.Minute,
			wantAPIKey:         "test-api-key",
			wantMaxRetries:     0,
			wantInitialBackoff: 0,
			wantMaxBackoff:     0,
			wantBackoffFactor:  0,
			wantJitterFraction: 0,
		},
		{
			name: "should apply retry defaults when MaxRetries is set",
			config: HTTPInferenceClientConfig{
				BaseURL:    "http://localhost:8000",
				MaxRetries: 3,
			},
			wantBaseURL:        "http://localhost:8000",
			wantTimeout:        5 * time.Minute,
			wantAPIKey:         "",
			wantMaxRetries:     3,
			wantInitialBackoff: 1 * time.Second,
			wantMaxBackoff:     60 * time.Second,
			wantBackoffFactor:  2.0,
			wantJitterFraction: 0.1,
		},
		{
			name: "should respect custom retry configuration",
			config: HTTPInferenceClientConfig{
				BaseURL:        "http://localhost:8000",
				MaxRetries:     5,
				InitialBackoff: 2 * time.Second,
				MaxBackoff:     120 * time.Second,
				BackoffFactor:  3.0,
				JitterFraction: 0.2,
			},
			wantBaseURL:        "http://localhost:8000",
			wantTimeout:        5 * time.Minute,
			wantAPIKey:         "",
			wantMaxRetries:     5,
			wantInitialBackoff: 2 * time.Second,
			wantMaxBackoff:     120 * time.Second,
			wantBackoffFactor:  3.0,
			wantJitterFraction: 0.2,
		},
		{
			name: "should apply partial retry defaults",
			config: HTTPInferenceClientConfig{
				BaseURL:        "http://localhost:8000",
				MaxRetries:     3,
				InitialBackoff: 500 * time.Millisecond,
			},
			wantBaseURL:        "http://localhost:8000",
			wantTimeout:        5 * time.Minute,
			wantAPIKey:         "",
			wantMaxRetries:     3,
			wantInitialBackoff: 500 * time.Millisecond,
			wantMaxBackoff:     60 * time.Second,
			wantBackoffFactor:  2.0,
			wantJitterFraction: 0.1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewHTTPInferenceClient(tt.config)
			assertNotNil(t, client)
			assertEqual(t, client.baseURL, tt.wantBaseURL)
			assertEqual(t, client.client.Timeout, tt.wantTimeout)
			assertEqual(t, client.apiKey, tt.wantAPIKey)
			assertEqual(t, client.retryConfig.MaxRetries, tt.wantMaxRetries)
			assertEqual(t, client.retryConfig.InitialBackoff, tt.wantInitialBackoff)
			assertEqual(t, client.retryConfig.MaxBackoff, tt.wantMaxBackoff)
			assertEqual(t, client.retryConfig.BackoffFactor, tt.wantBackoffFactor)
			assertEqual(t, client.retryConfig.JitterFraction, tt.wantJitterFraction)
		})
	}
}

func TestGenerate(t *testing.T) {
	t.Run("should successfully make inference request with chat completion", func(t *testing.T) {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify headers
			assertEqual(t, r.Header.Get("Content-Type"), "application/json")

			// Verify request ID if present
			if requestID := r.Header.Get("X-Request-ID"); requestID != "" {
				assertEqual(t, requestID, "test-request-123")
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

		assertNil(t, err)
		assertNotNil(t, resp)
		assertEqual(t, resp.RequestID, "test-request-123")
		assertNotNil(t, resp.Response)
		assertNotNil(t, resp.RawData)

		// Verify response can be unmarshaled
		var data map[string]interface{}
		unmarshalErr := json.Unmarshal(resp.Response, &data)
		assertNil(t, unmarshalErr)
		assertEqual(t, data["id"], "chatcmpl-123")
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

		assertNil(t, resp)
		assertNotNil(t, err)
		assertEqual(t, err.Category, ErrCategoryInvalidReq)
		assertContains(t, err.Message, "cannot be nil")
	})

	t.Run("should use chat completions endpoint for messages", func(t *testing.T) {
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
			Params: map[string]interface{}{
				"messages": []map[string]interface{}{
					{"role": "user", "content": "test"},
				},
			},
		}

		client.Generate(context.Background(), req)
		assertEqual(t, endpoint, "/v1/chat/completions")
	})

	t.Run("should use completions endpoint for prompt", func(t *testing.T) {
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
			Params: map[string]interface{}{
				"prompt": "Hello world",
			},
		}

		client.Generate(context.Background(), req)
		assertEqual(t, endpoint, "/v1/completions")
	})
}

func TestErrorHandling(t *testing.T) {
	t.Run("HTTP status code errors", func(t *testing.T) {
		tests := []struct {
			name            string
			statusCode      int
			responseBody    map[string]interface{}
			responseText    string
			wantCategory    ErrorCategory
			wantRetryable   bool
		}{
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
				name:         "should handle 503 Service Unavailable",
				statusCode:   http.StatusServiceUnavailable,
				responseText: "Service temporarily unavailable",
				wantCategory: ErrCategoryServer,
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
					}
				}))
				t.Cleanup(testServer.Close)

				client := NewHTTPInferenceClient(HTTPInferenceClientConfig{BaseURL: testServer.URL})

				req := &InferenceRequest{
					RequestID: "test",
					Model:     "gpt-4",
					Params:    map[string]interface{}{"model": "gpt-4"},
				}

				resp, err := client.Generate(context.Background(), req)
				assertNil(t, resp)
				assertNotNil(t, err)
				assertEqual(t, err.Category, tt.wantCategory)
				if tt.wantRetryable {
					assertTrue(t, err.IsRetryable())
				} else {
					assertFalse(t, err.IsRetryable())
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
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		resp, err := client.Generate(context.Background(), req)
		// Implementation continues despite JSON parse errors, returning success with nil RawData
		assertNil(t, err)
		assertNotNil(t, resp)
		assertEqual(t, resp.RequestID, "test")
		assertNil(t, resp.RawData) // RawData should be nil for malformed JSON
		assertNotNil(t, resp.Response)
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
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		resp, err := client.Generate(context.Background(), req)
		// Implementation handles empty body as successful response
		assertNil(t, err)
		assertNotNil(t, resp)
		assertEqual(t, resp.RequestID, "test")
		assertNil(t, resp.RawData) // RawData should be nil for empty JSON
		assertNotNil(t, resp.Response)
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
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		resp, err := client.Generate(ctx, req)
		assertNil(t, resp)
		assertNotNil(t, err)
		assertContains(t, err.Message, "cancelled")
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
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		ctx := context.Background()
		resp, err := client.Generate(ctx, req)
		assertNil(t, resp)
		assertNotNil(t, err)
		assertEqual(t, err.Category, ErrCategoryServer)
	})
}

func TestRetryLogic(t *testing.T) {
	t.Run("retry behavior for different error types", func(t *testing.T) {
		tests := []struct {
			name                   string
			statusCode             int
			errorMessage           string
			failuresBeforeSuccess  int
			wantAttemptCount       int
			wantSuccess            bool
			wantErrorCategory      ErrorCategory
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
					Params:    map[string]interface{}{"model": "gpt-4"},
				}

				resp, err := client.Generate(context.Background(), req)
				assertEqual(t, attemptCount, tt.wantAttemptCount)

				if tt.wantSuccess {
					assertNil(t, err)
					assertNotNil(t, resp)
				} else {
					assertNil(t, resp)
					assertNotNil(t, err)
					assertEqual(t, err.Category, tt.wantErrorCategory)
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
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		resp, err := client.Generate(context.Background(), req)
		assertNil(t, resp)
		assertNotNil(t, err)
		assertEqual(t, attemptCount, 3) // Initial + 2 retries
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
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(150 * time.Millisecond)
			cancel()
		}()

		resp, err := client.Generate(ctx, req)
		assertNil(t, resp)
		assertNotNil(t, err)
		assertLessOrEqual(t, attemptCount, 3) // Should stop early
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
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		resp, err := client.Generate(context.Background(), req)
		assertNil(t, err)
		assertNotNil(t, resp)
		assertEqual(t, attemptCount, 1)
	})

	t.Run("should apply exponential backoff", func(t *testing.T) {
		attemptCount := 0
		attemptTimes := []time.Time{}
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attemptTimes = append(attemptTimes, time.Now())
			attemptCount++
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(testServer.Close)

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL:        testServer.URL,
			MaxRetries:     3,
			InitialBackoff: 50 * time.Millisecond,
			BackoffFactor:  2.0,
			JitterFraction: 0.1,
		})

		req := &InferenceRequest{
			RequestID: "test",
			Model:     "gpt-4",
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		client.Generate(context.Background(), req)

		assertEqual(t, attemptCount, 4) // Initial + 3 retries

		// Verify exponential backoff (with some tolerance for jitter and timing)
		if len(attemptTimes) >= 2 {
			firstBackoff := attemptTimes[1].Sub(attemptTimes[0])
			assertDurationGreaterOrEqual(t, firstBackoff, 40*time.Millisecond)
		}
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
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		client.Generate(context.Background(), req)
		assertEqual(t, authHeader, "Bearer sk-test-key-123")
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
			Params:    map[string]interface{}{"model": "gpt-4"},
		}

		client.Generate(context.Background(), req)
		assertEmpty(t, authHeader)
	})
}
