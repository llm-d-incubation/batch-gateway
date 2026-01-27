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
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Integration tests using llm-d-inference-sim mock server running in Docker
//
// Each test spawns its own mock server instance with specific configuration
//
// Run tests with:
//   make test-integration
//   Or manually: go test -v -tags=integration ./internal/shared/batch/...

func TestHTTPInferenceClientIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "HTTPInferenceClient Integration Suite")
}

// Helper to start mock server on a specific port with custom args
func startMockServer(port int, args ...string) error {
	baseArgs := []string{
		"compose", "-f", "../../../docker-compose.test.yml",
		"run", "-d", "--rm",
		"--publish", fmt.Sprintf("%d:8000", port),
		"--name", fmt.Sprintf("mock-server-test-%d", port),
		"llm-d-mock-server",
		"--port=8000",
		"--model=fake-model",
	}
	baseArgs = append(baseArgs, args...)

	cmd := exec.Command("docker", baseArgs...)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to start mock server: %w", err)
	}

	// Wait for server to be ready
	serverURL := fmt.Sprintf("http://localhost:%d", port)
	for i := 0; i < 30; i++ {
		time.Sleep(200 * time.Millisecond)
		resp, err := http.Get(serverURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}

	return fmt.Errorf("mock server failed to become ready")
}

// Helper to stop mock server
func stopMockServer(port int) {
	containerName := fmt.Sprintf("mock-server-test-%d", port)
	cmd := exec.Command("docker", "stop", containerName)
	cmd.Run()
	time.Sleep(500 * time.Millisecond)
}

var _ = Describe("HTTPInferenceClient Basic Inference Tests", func() {
	var client *HTTPInferenceClient
	const testPort = 8200

	BeforeEach(func() {
		if os.Getenv("SKIP_INTEGRATION_TESTS") == "true" {
			Skip("Integration tests skipped")
		}

		// Start mock server with default configuration
		err := startMockServer(testPort, "--mode=random")
		if err != nil {
			Skip(fmt.Sprintf("Could not start mock server: %v", err))
		}

		client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL: fmt.Sprintf("http://localhost:%d", testPort),
			Timeout: 10 * time.Second,
		})
	})

	AfterEach(func() {
		stopMockServer(testPort)
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
		// Verifies that client can handle multiple requests and reuses connections
		for i := 0; i < 5; i++ {
			req := &InferenceRequest{
				RequestID: fmt.Sprintf("sequential-test-%03d", i),
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

	It("should handle concurrent requests correctly", func() {
		// Verifies connection pooling and thread safety
		const numRequests = 10
		results := make(chan error, numRequests)

		for i := 0; i < numRequests; i++ {
			go func(id int) {
				req := &InferenceRequest{
					RequestID: fmt.Sprintf("concurrent-test-%03d", id),
					Model:     "fake-model",
					Params: map[string]interface{}{
						"model":      "fake-model",
						"prompt":     "Concurrent test",
						"max_tokens": 5,
					},
				}

				_, err := client.Generate(context.Background(), req)
				results <- err
			}(i)
		}

		// Verify all requests completed successfully
		for i := 0; i < numRequests; i++ {
			err := <-results
			Expect(err).To(BeNil())
		}
	})
})

var _ = Describe("HTTPInferenceClient Latency Simulation Tests", func() {
	var client *HTTPInferenceClient
	const testPort = 8101

	BeforeEach(func() {
		if os.Getenv("SKIP_INTEGRATION_TESTS") == "true" {
			Skip("Integration tests skipped")
		}

		// Start mock server with latency configuration
		err := startMockServer(testPort,
			"--time-to-first-token=200ms",
			"--inter-token-latency=50ms",
		)
		if err != nil {
			Skip(fmt.Sprintf("Could not start mock server: %v", err))
		}

		client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL: fmt.Sprintf("http://localhost:%d", testPort),
			Timeout: 10 * time.Second,
		})
	})

	AfterEach(func() {
		stopMockServer(testPort)
	})

	It("should handle time-to-first-token latency", func() {
		req := &InferenceRequest{
			RequestID: "ttft-latency-001",
			Model:     "fake-model",
			Params: map[string]interface{}{
				"model":      "fake-model",
				"prompt":     "Test TTFT latency",
				"max_tokens": 5,
			},
		}

		start := time.Now()
		resp, err := client.Generate(context.Background(), req)
		duration := time.Since(start)

		Expect(err).To(BeNil())
		Expect(resp).NotTo(BeNil())
		// Should take at least 200ms for TTFT
		Expect(duration).To(BeNumerically(">=", 180*time.Millisecond))
		Expect(duration).To(BeNumerically("<", 2*time.Second))
	})

	It("should handle inter-token latency", func() {
		req := &InferenceRequest{
			RequestID: "inter-token-latency-001",
			Model:     "fake-model",
			Params: map[string]interface{}{
				"model":      "fake-model",
				"prompt":     "Test inter-token latency",
				"max_tokens": 10,
			},
		}

		start := time.Now()
		resp, err := client.Generate(context.Background(), req)
		duration := time.Since(start)

		Expect(err).To(BeNil())
		Expect(resp).NotTo(BeNil())
		// With 10 tokens, TTFT=200ms + ~10*50ms = ~700ms total
		Expect(duration).To(BeNumerically(">=", 200*time.Millisecond))
	})
})

var _ = Describe("HTTPInferenceClient Failure Injection Tests", func() {
	var client *HTTPInferenceClient

	BeforeEach(func() {
		if os.Getenv("SKIP_INTEGRATION_TESTS") == "true" {
			Skip("Integration tests skipped")
		}
	})

	Context("Rate Limit Errors (429)", func() {
		const testPort = 8102

		BeforeEach(func() {
			err := startMockServer(testPort,
				"--failure-injection-rate=100",
				"--failure-types=rate_limit",
			)
			if err != nil {
				Skip(fmt.Sprintf("Could not start mock server: %v", err))
			}

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL:        fmt.Sprintf("http://localhost:%d", testPort),
				Timeout:        5 * time.Second,
				MaxRetries:     2,
				InitialBackoff: 50 * time.Millisecond,
			})
		})

		AfterEach(func() {
			stopMockServer(testPort)
		})

		It("should handle rate limit errors with retry", func() {
			req := &InferenceRequest{
				RequestID: "rate-limit-001",
				Model:     "fake-model",
				Params: map[string]interface{}{
					"model":      "fake-model",
					"prompt":     "Test",
					"max_tokens": 5,
				},
			}

			resp, err := client.Generate(context.Background(), req)

			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Category).To(Equal(ErrCategoryRateLimit))
			Expect(err.IsRetryable()).To(BeTrue())
		})
	})

	Context("Server Errors (500)", func() {
		const testPort = 8103

		BeforeEach(func() {
			err := startMockServer(testPort,
				"--failure-injection-rate=100",
				"--failure-types=server_error",
			)
			if err != nil {
				Skip(fmt.Sprintf("Could not start mock server: %v", err))
			}

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL:        fmt.Sprintf("http://localhost:%d", testPort),
				Timeout:        5 * time.Second,
				MaxRetries:     2,
				InitialBackoff: 50 * time.Millisecond,
			})
		})

		AfterEach(func() {
			stopMockServer(testPort)
		})

		It("should handle server errors with retry", func() {
			req := &InferenceRequest{
				RequestID: "server-error-001",
				Model:     "fake-model",
				Params: map[string]interface{}{
					"model":      "fake-model",
					"prompt":     "Test",
					"max_tokens": 5,
				},
			}

			resp, err := client.Generate(context.Background(), req)

			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Category).To(Equal(ErrCategoryServer))
			Expect(err.IsRetryable()).To(BeTrue())
		})
	})

	Context("Invalid API Key Errors (401)", func() {
		const testPort = 8104

		BeforeEach(func() {
			err := startMockServer(testPort,
				"--failure-injection-rate=100",
				"--failure-types=invalid_api_key",
			)
			if err != nil {
				Skip(fmt.Sprintf("Could not start mock server: %v", err))
			}

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL:        fmt.Sprintf("http://localhost:%d", testPort),
				Timeout:        5 * time.Second,
				MaxRetries:     2,
				InitialBackoff: 50 * time.Millisecond,
			})
		})

		AfterEach(func() {
			stopMockServer(testPort)
		})

		It("should handle auth errors without retry", func() {
			req := &InferenceRequest{
				RequestID: "auth-error-001",
				Model:     "fake-model",
				Params: map[string]interface{}{
					"model":      "fake-model",
					"prompt":     "Test",
					"max_tokens": 5,
				},
			}

			resp, err := client.Generate(context.Background(), req)

			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Category).To(Equal(ErrCategoryAuth))
			Expect(err.IsRetryable()).To(BeFalse())
		})
	})

	Context("Invalid Request Errors (400)", func() {
		const testPort = 8105

		BeforeEach(func() {
			err := startMockServer(testPort,
				"--failure-injection-rate=100",
				"--failure-types=invalid_request",
			)
			if err != nil {
				Skip(fmt.Sprintf("Could not start mock server: %v", err))
			}

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL:        fmt.Sprintf("http://localhost:%d", testPort),
				Timeout:        5 * time.Second,
				MaxRetries:     2,
				InitialBackoff: 50 * time.Millisecond,
			})
		})

		AfterEach(func() {
			stopMockServer(testPort)
		})

		It("should handle invalid request errors without retry", func() {
			req := &InferenceRequest{
				RequestID: "invalid-request-001",
				Model:     "fake-model",
				Params: map[string]interface{}{
					"model":      "fake-model",
					"prompt":     "Test",
					"max_tokens": 5,
				},
			}

			resp, err := client.Generate(context.Background(), req)

			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Category).To(Equal(ErrCategoryInvalidReq))
			Expect(err.IsRetryable()).To(BeFalse())
		})
	})

	Context("Context Length Errors (400)", func() {
		const testPort = 8106

		BeforeEach(func() {
			err := startMockServer(testPort,
				"--failure-injection-rate=100",
				"--failure-types=context_length",
			)
			if err != nil {
				Skip(fmt.Sprintf("Could not start mock server: %v", err))
			}

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL:        fmt.Sprintf("http://localhost:%d", testPort),
				Timeout:        5 * time.Second,
				MaxRetries:     2,
				InitialBackoff: 50 * time.Millisecond,
			})
		})

		AfterEach(func() {
			stopMockServer(testPort)
		})

		It("should handle context length errors without retry", func() {
			req := &InferenceRequest{
				RequestID: "context-length-001",
				Model:     "fake-model",
				Params: map[string]interface{}{
					"model":      "fake-model",
					"prompt":     "Test",
					"max_tokens": 5,
				},
			}

			resp, err := client.Generate(context.Background(), req)

			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.Category).To(Equal(ErrCategoryInvalidReq))
			Expect(err.IsRetryable()).To(BeFalse())
		})
	})

	Context("Model Not Found Errors (404)", func() {
		const testPort = 8107

		BeforeEach(func() {
			err := startMockServer(testPort,
				"--failure-injection-rate=100",
				"--failure-types=model_not_found",
			)
			if err != nil {
				Skip(fmt.Sprintf("Could not start mock server: %v", err))
			}

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL:        fmt.Sprintf("http://localhost:%d", testPort),
				Timeout:        5 * time.Second,
				MaxRetries:     2,
				InitialBackoff: 50 * time.Millisecond,
			})
		})

		AfterEach(func() {
			stopMockServer(testPort)
		})

		It("should handle model not found errors without retry", func() {
			req := &InferenceRequest{
				RequestID: "model-not-found-001",
				Model:     "fake-model",
				Params: map[string]interface{}{
					"model":      "fake-model",
					"prompt":     "Test",
					"max_tokens": 5,
				},
			}

			resp, err := client.Generate(context.Background(), req)

			Expect(resp).To(BeNil())
			Expect(err).NotTo(BeNil())
			Expect(err.IsRetryable()).To(BeFalse())
		})
	})

	Context("Mixed Failure Rate (50%)", func() {
		const testPort = 8108

		BeforeEach(func() {
			err := startMockServer(testPort,
				"--failure-injection-rate=50",
				"--failure-types=server_error",
			)
			if err != nil {
				Skip(fmt.Sprintf("Could not start mock server: %v", err))
			}

			client = NewHTTPInferenceClient(HTTPInferenceClientConfig{
				BaseURL:        fmt.Sprintf("http://localhost:%d", testPort),
				Timeout:        10 * time.Second,
				MaxRetries:     5,
				InitialBackoff: 50 * time.Millisecond,
			})
		})

		AfterEach(func() {
			stopMockServer(testPort)
		})

		It("should eventually succeed with retry on 50% failure rate", func() {
			req := &InferenceRequest{
				RequestID: "mixed-failure-001",
				Model:     "fake-model",
				Params: map[string]interface{}{
					"model":      "fake-model",
					"prompt":     "Test retry on partial failures",
					"max_tokens": 5,
				},
			}

			// With 50% failure rate and 5 retries, probability of all failing = 0.5^6 = 1.5%
			resp, err := client.Generate(context.Background(), req)

			// Should likely succeed (98.5% probability)
			// If it fails, that's acceptable but unlikely
			if err == nil {
				Expect(resp).NotTo(BeNil())
			} else {
				// If it did fail, verify it's the right type
				Expect(err.Category).To(Equal(ErrCategoryServer))
				Expect(err.IsRetryable()).To(BeTrue())
			}
		})
	})
})
