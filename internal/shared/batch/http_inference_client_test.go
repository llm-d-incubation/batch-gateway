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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestHTTPInferenceClient(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "HTTPInferenceClient Suite")
}

var _ = Describe("HTTPInferenceClient", func() {
	var (
		client     *HTTPInferenceClient
		testServer *httptest.Server
	)

	AfterEach(func() {
		if testServer != nil {
			testServer.Close()
		}
	})

	Context("NewHTTPInferenceClient", func() {
		It("should create client with default configuration", func() {
			config := HTTPInferenceClientConfig{
				BaseURL: "http://localhost:8000",
			}
			client := NewHTTPInferenceClient(config)
			Expect(client).NotTo(BeNil())
			Expect(client.baseURL).To(Equal("http://localhost:8000"))
			Expect(client.client.Timeout).To(Equal(5 * time.Minute))
			Expect(client.retryConfig.MaxRetries).To(Equal(0)) // Retry disabled by default
		})

		It("should create client with custom configuration", func() {
			config := HTTPInferenceClientConfig{
				BaseURL:         "http://localhost:9000",
				Timeout:         1 * time.Minute,
				MaxIdleConns:    50,
				IdleConnTimeout: 60 * time.Second,
				APIKey:          "test-api-key",
			}
			client := NewHTTPInferenceClient(config)
			Expect(client).NotTo(BeNil())
			Expect(client.baseURL).To(Equal("http://localhost:9000"))
			Expect(client.client.Timeout).To(Equal(1 * time.Minute))
			Expect(client.apiKey).To(Equal("test-api-key"))
		})

		It("should apply retry defaults when MaxRetries is set", func() {
			config := HTTPInferenceClientConfig{
				BaseURL:    "http://localhost:8000",
				MaxRetries: 3, // Enable retry
			}
			client := NewHTTPInferenceClient(config)
			Expect(client).NotTo(BeNil())
			Expect(client.retryConfig.MaxRetries).To(Equal(3))
			Expect(client.retryConfig.InitialBackoff).To(Equal(1 * time.Second))
			Expect(client.retryConfig.MaxBackoff).To(Equal(60 * time.Second))
			Expect(client.retryConfig.BackoffFactor).To(Equal(2.0))
			Expect(client.retryConfig.JitterFraction).To(Equal(0.1))
		})

		It("should respect custom retry configuration", func() {
			config := HTTPInferenceClientConfig{
				BaseURL:         "http://localhost:8000",
				MaxRetries:      5,
				InitialBackoff:  2 * time.Second,
				MaxBackoff:      120 * time.Second,
				BackoffFactor:   3.0,
				JitterFraction:  0.2,
			}
			client := NewHTTPInferenceClient(config)
			Expect(client).NotTo(BeNil())
			Expect(client.retryConfig.MaxRetries).To(Equal(5))
			Expect(client.retryConfig.InitialBackoff).To(Equal(2 * time.Second))
			Expect(client.retryConfig.MaxBackoff).To(Equal(120 * time.Second))
			Expect(client.retryConfig.BackoffFactor).To(Equal(3.0))
			Expect(client.retryConfig.JitterFraction).To(Equal(0.2))
		})

		It("should apply partial retry defaults", func() {
			config := HTTPInferenceClientConfig{
				BaseURL:        "http://localhost:8000",
				MaxRetries:     3,
				InitialBackoff: 500 * time.Millisecond, // Custom
				// Other retry fields not set - should use defaults
			}
			client := NewHTTPInferenceClient(config)
			Expect(client).NotTo(BeNil())
			Expect(client.retryConfig.MaxRetries).To(Equal(3))
			Expect(client.retryConfig.InitialBackoff).To(Equal(500 * time.Millisecond))
			Expect(client.retryConfig.MaxBackoff).To(Equal(60 * time.Second))      // Default
			Expect(client.retryConfig.BackoffFactor).To(Equal(2.0))                 // Default
			Expect(client.retryConfig.JitterFraction).To(Equal(0.1))                // Default
		})
	})

	Context("Generate", func() {
		BeforeEach(func() {
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify headers
				Expect(r.Header.Get("Content-Type")).To(Equal("application/json"))

				// Verify request ID if present
				if requestID := r.Header.Get("X-Request-ID"); requestID != "" {
					Expect(requestID).To(Equal("test-request-123"))
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

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL: testServer.URL,
				Timeout: 10 * time.Second,
			})
		})

		It("should successfully make inference request with chat completion", func() {
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

			Expect(err).To(BeNil())
			Expect(resp).NotTo(BeNil())
			Expect(resp.RequestID).To(Equal("test-request-123"))
			Expect(resp.Response).NotTo(BeNil())
			Expect(resp.RawData).NotTo(BeNil())

			// Verify response can be unmarshaled
			var data map[string]interface{}
			unmarshalErr := json.Unmarshal(resp.Response, &data)
			Expect(unmarshalErr).To(BeNil())
			Expect(data["id"]).To(Equal("chatcmpl-123"))
		})

		It("should handle nil request", func() {
			ctx := context.Background()
			resp, err := client.Generate(ctx, nil)

			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Category).To(Equal(ErrCategoryInvalidReq))
			Expect(err.Message).To(ContainSubstring("cannot be nil"))
		})

		It("should use chat completions endpoint for messages", func() {
			endpoint := ""
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				endpoint = r.URL.Path
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{"id": "test"})
			}))
			client.baseURL = testServer.URL

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
			Expect(endpoint).To(Equal("/v1/chat/completions"))
		})

		It("should use completions endpoint for prompt", func() {
			endpoint := ""
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				endpoint = r.URL.Path
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{"id": "test"})
			}))
			client.baseURL = testServer.URL

			req := &InferenceRequest{
				RequestID: "test",
				Model:     "gpt-4",
				Params: map[string]interface{}{
					"prompt": "Hello world",
				},
			}

			client.Generate(context.Background(), req)
			Expect(endpoint).To(Equal("/v1/completions"))
		})
	})

	Context("Error Handling", func() {
		It("should handle 400 Bad Request", func() {
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    400,
						"message": "Invalid request parameters",
					},
				})
			}))
			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{BaseURL: testServer.URL})

			req := &InferenceRequest{
				RequestID: "test",
				Model:     "gpt-4",
				Params:    map[string]interface{}{"model": "gpt-4"},
			}

			resp, err := client.Generate(context.Background(), req)
			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Category).To(Equal(ErrCategoryInvalidReq))
			Expect(err.IsRetryable()).To(BeFalse())
		})

		It("should handle 401 Unauthorized", func() {
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    401,
						"message": "Invalid API key",
					},
				})
			}))
			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{BaseURL: testServer.URL})

			req := &InferenceRequest{
				RequestID: "test",
				Model:     "gpt-4",
				Params:    map[string]interface{}{"model": "gpt-4"},
			}

			resp, err := client.Generate(context.Background(), req)
			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Category).To(Equal(ErrCategoryAuth))
			Expect(err.IsRetryable()).To(BeFalse())
		})

		It("should handle 429 Rate Limit", func() {
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    429,
						"message": "Rate limit exceeded",
					},
				})
			}))
			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{BaseURL: testServer.URL})

			req := &InferenceRequest{
				RequestID: "test",
				Model:     "gpt-4",
				Params:    map[string]interface{}{"model": "gpt-4"},
			}

			resp, err := client.Generate(context.Background(), req)
			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Category).To(Equal(ErrCategoryRateLimit))
			Expect(err.IsRetryable()).To(BeTrue())
		})

		It("should handle 500 Internal Server Error", func() {
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    500,
						"message": "Internal server error",
					},
				})
			}))
			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{BaseURL: testServer.URL})

			req := &InferenceRequest{
				RequestID: "test",
				Model:     "gpt-4",
				Params:    map[string]interface{}{"model": "gpt-4"},
			}

			resp, err := client.Generate(context.Background(), req)
			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Category).To(Equal(ErrCategoryServer))
			Expect(err.IsRetryable()).To(BeTrue())
		})

		It("should handle 503 Service Unavailable", func() {
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte("Service temporarily unavailable"))
			}))
			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{BaseURL: testServer.URL})

			req := &InferenceRequest{
				RequestID: "test",
				Model:     "gpt-4",
				Params:    map[string]interface{}{"model": "gpt-4"},
			}

			resp, err := client.Generate(context.Background(), req)
			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Category).To(Equal(ErrCategoryServer))
			Expect(err.IsRetryable()).To(BeTrue())
		})

		It("should handle context cancellation", func() {
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(2 * time.Second)
				w.WriteHeader(http.StatusOK)
			}))
			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{BaseURL: testServer.URL})

			req := &InferenceRequest{
				RequestID: "test",
				Model:     "gpt-4",
				Params:    map[string]interface{}{"model": "gpt-4"},
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel() // Cancel immediately

			resp, err := client.Generate(ctx, req)
			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Message).To(ContainSubstring("cancelled"))
		})

		It("should handle context timeout", func() {
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(2 * time.Second)
				w.WriteHeader(http.StatusOK)
			}))
			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
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
			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Category).To(Equal(ErrCategoryServer))
		})
	})

	Context("Retry Logic", func() {
		It("should retry on rate limit error", func() {
			attemptCount := 0
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attemptCount++
				if attemptCount < 3 {
					w.WriteHeader(http.StatusTooManyRequests)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"error": map[string]interface{}{
							"code":    429,
							"message": "Rate limit exceeded",
						},
					})
				} else {
					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(map[string]interface{}{"id": "success"})
				}
			}))

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
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
			Expect(err).To(BeNil())
			Expect(resp).NotTo(BeNil())
			Expect(attemptCount).To(Equal(3))
		})

		It("should retry on server error", func() {
			attemptCount := 0
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attemptCount++
				if attemptCount < 2 {
					w.WriteHeader(http.StatusInternalServerError)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"error": map[string]interface{}{
							"code":    500,
							"message": "Internal server error",
						},
					})
				} else {
					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(map[string]interface{}{"id": "success"})
				}
			}))

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
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
			Expect(err).To(BeNil())
			Expect(resp).NotTo(BeNil())
			Expect(attemptCount).To(Equal(2))
		})

		It("should not retry on bad request error", func() {
			attemptCount := 0
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attemptCount++
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    400,
						"message": "Bad request",
					},
				})
			}))

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
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
			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Category).To(Equal(ErrCategoryInvalidReq))
			Expect(attemptCount).To(Equal(1)) // Should not retry
		})

		It("should not retry on auth error", func() {
			attemptCount := 0
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attemptCount++
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    401,
						"message": "Unauthorized",
					},
				})
			}))

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
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
			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Category).To(Equal(ErrCategoryAuth))
			Expect(attemptCount).To(Equal(1)) // Should not retry
		})

		It("should respect max retries", func() {
			attemptCount := 0
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attemptCount++
				w.WriteHeader(http.StatusTooManyRequests)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    429,
						"message": "Rate limit exceeded",
					},
				})
			}))

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
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
			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(attemptCount).To(Equal(3)) // Initial + 2 retries
		})

		It("should stop retrying when context is cancelled", func() {
			attemptCount := 0
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attemptCount++
				w.WriteHeader(http.StatusTooManyRequests)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    429,
						"message": "Rate limit exceeded",
					},
				})
			}))

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
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
			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(attemptCount).To(BeNumerically("<=", 3)) // Should stop early
		})

		It("should work without retry when MaxRetries is 0", func() {
			attemptCount := 0
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attemptCount++
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{"id": "success"})
			}))

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL:    testServer.URL,
				MaxRetries: 0, // Retry disabled
			})

			req := &InferenceRequest{
				RequestID: "test",
				Model:     "gpt-4",
				Params:    map[string]interface{}{"model": "gpt-4"},
			}

			resp, err := client.Generate(context.Background(), req)
			Expect(err).To(BeNil())
			Expect(resp).NotTo(BeNil())
			Expect(attemptCount).To(Equal(1))
		})

		It("should apply exponential backoff", func() {
			attemptCount := 0
			attemptTimes := []time.Time{}
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attemptTimes = append(attemptTimes, time.Now())
				attemptCount++
				w.WriteHeader(http.StatusServiceUnavailable)
			}))

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
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

			Expect(attemptCount).To(Equal(4)) // Initial + 3 retries

			// Verify exponential backoff (with some tolerance for jitter and timing)
			if len(attemptTimes) >= 2 {
				firstBackoff := attemptTimes[1].Sub(attemptTimes[0])
				Expect(firstBackoff).To(BeNumerically(">=", 40*time.Millisecond))
			}
		})
	})

	Context("Authentication", func() {
		It("should include API key in Authorization header", func() {
			var authHeader string
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authHeader = r.Header.Get("Authorization")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{"id": "test"})
			}))

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL: testServer.URL,
				APIKey:  "sk-test-key-123",
			})

			req := &InferenceRequest{
				RequestID: "test",
				Model:     "gpt-4",
				Params:    map[string]interface{}{"model": "gpt-4"},
			}

			client.Generate(context.Background(), req)
			Expect(authHeader).To(Equal("Bearer sk-test-key-123"))
		})

		It("should not include Authorization header when API key is empty", func() {
			var authHeader string
			testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authHeader = r.Header.Get("Authorization")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{"id": "test"})
			}))

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL: testServer.URL,
			})

			req := &InferenceRequest{
				RequestID: "test",
				Model:     "gpt-4",
				Params:    map[string]interface{}{"model": "gpt-4"},
			}

			client.Generate(context.Background(), req)
			Expect(authHeader).To(BeEmpty())
		})
	})
})
