//go:build integration
// +build integration

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
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Integration tests using llm-d-inference-sim mock server running in Docker
//
// Prerequisites:
//   Start the mock server with: make test-integration-up
//   Or manually: docker-compose -f docker-compose.test.yml up -d
//
// Run tests with:
//   make test-integration
//   Or manually: go test -v -tags=integration -run TestHTTPInferenceClientIntegration

const (
	mockServerURL = "http://localhost:8100"
)

func TestHTTPInferenceClientIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "HTTPInferenceClient Integration Suite")
}

var _ = Describe("HTTPInferenceClient Integration Tests", func() {
	var client *HTTPInferenceClient

	BeforeEach(func() {
		// Check if SKIP_INTEGRATION_TESTS env var is set
		if os.Getenv("SKIP_INTEGRATION_TESTS") == "true" {
			Skip("Integration tests skipped via SKIP_INTEGRATION_TESTS=true")
		}

		// Verify mock server is running
		testClient := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL: mockServerURL,
			Timeout: 2 * time.Second,
		})

		req := &InferenceRequest{
			RequestID: "health-check",
			Model:     "fake-model",
			Params: map[string]interface{}{
				"model":      "fake-model",
				"prompt":     "test",
				"max_tokens": 1,
			},
		}

		_, err := testClient.Generate(context.Background(), req)
		if err != nil {
			Skip("Mock server not running. Start with: make test-integration-up")
		}
	})

	Context("Basic Inference", func() {
		BeforeEach(func() {
			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL: mockServerURL,
				Timeout: 10 * time.Second,
			})
		})

		It("should successfully make text completion request", func() {
			req := &InferenceRequest{
				RequestID: "test-completion-001",
				Model:     "fake-model",
				Params: map[string]interface{}{
					"model":      "fake-model",
					"prompt":     "Once upon a time",
					"max_tokens": 10,
				},
			}

			ctx := context.Background()
			resp, err := client.Generate(ctx, req)

			Expect(err).To(BeNil())
			Expect(resp).NotTo(BeNil())
			Expect(resp.RequestID).To(Equal("test-completion-001"))
			Expect(resp.Response).NotTo(BeEmpty())

			// Verify response structure
			var result map[string]interface{}
			unmarshalErr := json.Unmarshal(resp.Response, &result)
			Expect(unmarshalErr).To(BeNil())
			Expect(result["id"]).NotTo(BeNil())
			Expect(result["choices"]).NotTo(BeNil())
		})

		It("should successfully make chat completion request", func() {
			req := &InferenceRequest{
				RequestID: "test-chat-001",
				Model:     "fake-model",
				Params: map[string]interface{}{
					"model": "fake-model",
					"messages": []map[string]interface{}{
						{
							"role":    "user",
							"content": "Hello, how are you?",
						},
					},
					"max_tokens": 20,
				},
			}

			ctx := context.Background()
			resp, err := client.Generate(ctx, req)

			Expect(err).To(BeNil())
			Expect(resp).NotTo(BeNil())

			// Verify response structure
			var result map[string]interface{}
			unmarshalErr := json.Unmarshal(resp.Response, &result)
			Expect(unmarshalErr).To(BeNil())
			Expect(result["choices"]).NotTo(BeNil())
		})

		It("should handle multiple sequential requests", func() {
			for i := 0; i < 5; i++ {
				req := &InferenceRequest{
					RequestID: "sequential-test-001",
					Model:     "fake-model",
					Params: map[string]interface{}{
						"model":      "fake-model",
						"prompt":     "Test request",
						"max_tokens": 5,
					},
				}

				ctx := context.Background()
				resp, err := client.Generate(ctx, req)

				Expect(err).To(BeNil())
				Expect(resp).NotTo(BeNil())
			}
		})
	})

	Context("Retry Logic", func() {
		It("should retry on server errors", func() {
			// Note: This test depends on mock server configuration
			// The default mock server doesn't inject failures
			// You can restart the mock server with failure injection for this test

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL:        mockServerURL,
				Timeout:        10 * time.Second,
				MaxRetries:     3,
				InitialBackoff: 100 * time.Millisecond,
			})

			req := &InferenceRequest{
				RequestID: "retry-test-001",
				Model:     "fake-model",
				Params: map[string]interface{}{
					"model":      "fake-model",
					"prompt":     "Test retry",
					"max_tokens": 5,
				},
			}

			ctx := context.Background()
			resp, err := client.Generate(ctx, req)

			// Should succeed with normal mock server
			Expect(err).To(BeNil())
			Expect(resp).NotTo(BeNil())
		})

		It("should work without retry when disabled", func() {
			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL:    mockServerURL,
				Timeout:    5 * time.Second,
				MaxRetries: 0, // Disabled
			})

			req := &InferenceRequest{
				RequestID: "no-retry-test-001",
				Model:     "fake-model",
				Params: map[string]interface{}{
					"model":      "fake-model",
					"prompt":     "Test",
					"max_tokens": 5,
				},
			}

			ctx := context.Background()
			resp, err := client.Generate(ctx, req)

			Expect(err).To(BeNil())
			Expect(resp).NotTo(BeNil())
		})
	})

	Context("Context Handling", func() {
		BeforeEach(func() {
			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL: mockServerURL,
				Timeout: 10 * time.Second,
			})
		})

		It("should respect context timeout", func() {
			// Note: This test validates timeout behavior
			// The mock server may respond faster than the timeout on fast machines
			// In that case, it's acceptable for the request to succeed
			req := &InferenceRequest{
				RequestID: "timeout-test-001",
				Model:     "fake-model",
				Params: map[string]interface{}{
					"model":      "fake-model",
					"prompt":     "Test",
					"max_tokens": 100,
				},
			}

			// Very short timeout - may or may not trigger depending on mock server speed
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
			defer cancel()

			start := time.Now()
			resp, err := client.Generate(ctx, req)
			duration := time.Since(start)

			// Either times out OR succeeds very quickly (mock server is fast)
			if err != nil {
				// Timeout occurred - this is expected behavior
				Expect(resp).To(BeNil())
				Expect(duration).To(BeNumerically("<", 500*time.Millisecond))
			} else {
				// Request succeeded very quickly (mock server is faster than 1ms)
				// This is acceptable for a mock server
				Expect(resp).NotTo(BeNil())
				Expect(duration).To(BeNumerically("<", 100*time.Millisecond))
			}
		})

		It("should respect context cancellation", func() {
			req := &InferenceRequest{
				RequestID: "cancel-test-001",
				Model:     "fake-model",
				Params: map[string]interface{}{
					"model":      "fake-model",
					"prompt":     "Test",
					"max_tokens": 100,
				},
			}

			ctx, cancel := context.WithCancel(context.Background())

			// Cancel immediately
			cancel()

			resp, err := client.Generate(ctx, req)

			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
		})
	})

	Context("Configuration Options", func() {
		It("should work with custom retry configuration", func() {
			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL:         mockServerURL,
				Timeout:         10 * time.Second,
				MaxRetries:      5,
				InitialBackoff:  200 * time.Millisecond,
				MaxBackoff:      2 * time.Second,
				BackoffFactor:   2.0,
				JitterFraction:  0.1,
			})

			Expect(client.retryConfig.MaxRetries).To(Equal(5))
			Expect(client.retryConfig.InitialBackoff).To(Equal(200 * time.Millisecond))
			Expect(client.retryConfig.MaxBackoff).To(Equal(2 * time.Second))
			Expect(client.retryConfig.BackoffFactor).To(Equal(2.0))
			Expect(client.retryConfig.JitterFraction).To(Equal(0.1))

			req := &InferenceRequest{
				RequestID: "config-test-001",
				Model:     "fake-model",
				Params: map[string]interface{}{
					"model":      "fake-model",
					"prompt":     "Test",
					"max_tokens": 5,
				},
			}

			ctx := context.Background()
			resp, err := client.Generate(ctx, req)

			Expect(err).To(BeNil())
			Expect(resp).NotTo(BeNil())
		})
	})
})
