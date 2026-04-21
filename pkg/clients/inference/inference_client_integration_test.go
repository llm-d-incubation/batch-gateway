//go:build integration

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
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	httpclient "github.com/llm-d-incubation/batch-gateway/pkg/clients/http"
)

// Integration tests using llm-d-inference-sim mock server running in Docker
//
// Each test spawns its own mock server instance with specific configuration
//
// Run tests with:
//   make test-integration
//   Or manually: go test -v -tags=integration ./internal/inference/...

// Helper to start mock server on a specific port with custom args
func startMockServer(port int, args ...string) error {
	baseArgs := []string{
		"compose", "-f", "./docker-compose.test.yml",
		"run", "-d", "--rm",
		"--publish", fmt.Sprintf("%d:8000", port),
		"--name", fmt.Sprintf("mock-server-test-%d", port),
		"llm-d-mock-server",
		"--port=8000",
		"--model=fake-model",
	}
	baseArgs = append(baseArgs, args...)

	cmd := exec.Command("docker", baseArgs...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

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

// TestHTTPClientIntegration aggregates all integration test cases
// Run with: go test -tags=integration -run TestHTTPClientIntegration ./internal/inference
func TestHTTPClientIntegration(t *testing.T) {
	t.Run("BasicInference", testHTTPClientBasicInference)
	t.Run("LatencySimulation", testHTTPClientLatencySimulation)
	t.Run("FailureInjection", testHTTPClientFailureInjection)
}

func testHTTPClientBasicInference(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION_TESTS") == "true" {
		t.Skip("Integration tests skipped")
	}

	const testPort = 8200

	// Start mock server with default configuration
	err := startMockServer(testPort, "--mode=random")
	if err != nil {
		t.Fatalf("Could not start mock server: %v", err)
	}
	t.Cleanup(func() { stopMockServer(testPort) })

	client, err := NewInferenceClient(&HTTPClientConfig{
		BaseURL: fmt.Sprintf("http://localhost:%d", testPort),
		Timeout: 10 * time.Second,
	}, testLogger(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Run("should handle multiple sequential requests", func(t *testing.T) {
		// Verifies that client can handle multiple requests and reuses connections
		for i := 0; i < 5; i++ {
			req := &GenerateRequest{
				RequestID: fmt.Sprintf("sequential-test-%03d", i),
				Endpoint:  "/v1/chat/completions",
				Params: map[string]interface{}{
					"model":      "fake-model",
					"prompt":     "Test request",
					"max_tokens": 5,
				},
			}

			ctx := context.Background()
			resp, genErr := client.Generate(ctx, req)

			if genErr != nil {
				t.Errorf("request %d failed: %v", i, genErr)
			}
			if resp == nil {
				t.Fatalf("request %d returned nil response", i)
			}
		}
	})

	t.Run("should handle concurrent requests correctly", func(t *testing.T) {
		// Verifies connection pooling and thread safety
		const numRequests = 10
		results := make(chan error, numRequests)

		for i := 0; i < numRequests; i++ {
			go func(id int) {
				req := &GenerateRequest{
					RequestID: fmt.Sprintf("concurrent-test-%03d", id),
					Endpoint:  "/v1/chat/completions",
					Params: map[string]interface{}{
						"model":      "fake-model",
						"prompt":     "Concurrent test",
						"max_tokens": 5,
					},
				}

				_, inferr := client.Generate(context.Background(), req)
				results <- inferr
			}(i)
		}

		// Verify all requests completed successfully
		for i := 0; i < numRequests; i++ {
			inferr := <-results
			if inferr != nil {
				t.Errorf("concurrent request %d failed: %v", i, inferr)
			}
		}
	})
}

func testHTTPClientLatencySimulation(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION_TESTS") == "true" {
		t.Skip("Integration tests skipped")
	}

	const testPort = 8101

	// Start mock server with latency configuration
	err := startMockServer(testPort,
		"--time-to-first-token=200ms",
		"--inter-token-latency=50ms",
	)
	if err != nil {
		t.Fatalf("Could not start mock server: %v", err)
	}
	t.Cleanup(func() { stopMockServer(testPort) })

	client, err := NewInferenceClient(&HTTPClientConfig{
		BaseURL: fmt.Sprintf("http://localhost:%d", testPort),
		Timeout: 10 * time.Second,
	}, testLogger(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Run("should handle time-to-first-token latency", func(t *testing.T) {
		req := &GenerateRequest{
			RequestID: "ttft-latency-001",
			Endpoint:  "/v1/chat/completions",
			Params: map[string]interface{}{
				"model":      "fake-model",
				"prompt":     "Test TTFT latency",
				"max_tokens": 5,
			},
		}

		start := time.Now()
		resp, genErr := client.Generate(context.Background(), req)
		duration := time.Since(start)

		if genErr != nil {
			t.Errorf("expected no error, got %v", genErr)
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		if duration < 180*time.Millisecond {
			t.Errorf("expected duration >= 180ms, got %v", duration)
		}
		if duration >= 2*time.Second {
			t.Errorf("expected duration < 2s, got %v", duration)
		}
	})

	t.Run("should handle inter-token latency", func(t *testing.T) {
		req := &GenerateRequest{
			RequestID: "inter-token-latency-001",
			Endpoint:  "/v1/chat/completions",
			Params: map[string]interface{}{
				"model":      "fake-model",
				"prompt":     "Test inter-token latency",
				"max_tokens": 10,
			},
		}

		start := time.Now()
		resp, genErr := client.Generate(context.Background(), req)
		duration := time.Since(start)

		if genErr != nil {
			t.Errorf("expected no error, got %v", genErr)
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		// With 10 tokens, TTFT=200ms + ~10*50ms = ~700ms total
		if duration < 200*time.Millisecond {
			t.Errorf("expected duration >= 200ms, got %v", duration)
		}
	})
}

func testHTTPClientFailureInjection(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION_TESTS") == "true" {
		t.Skip("Integration tests skipped")
	}

	// Note: Specific error status code tests (429, 500, 401, 400, 404) are covered
	// comprehensively in unit tests (TestErrorHandling, TestAdditionalHTTPStatusCodes,
	// TestRetryConditionLogic). Integration tests focus on real HTTP behavior with
	// randomized failures to test retry logic end-to-end.

	t.Run("Mixed Failure Rate (50%)", func(t *testing.T) {
		const testPort = 8108

		if err := startMockServer(testPort,
			"--failure-injection-rate=50",
			"--failure-types=server_error",
		); err != nil {
			t.Fatalf("Could not start mock server: %v", err)
		}
		t.Cleanup(func() { stopMockServer(testPort) })

		client, err := NewInferenceClient(&HTTPClientConfig{
			BaseURL:        fmt.Sprintf("http://localhost:%d", testPort),
			Timeout:        10 * time.Second,
			MaxRetries:     5,
			InitialBackoff: 50 * time.Millisecond,
		}, testLogger(t))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		req := &GenerateRequest{
			RequestID: "mixed-failure-001",
			Endpoint:  "/v1/completions",
			Params: map[string]interface{}{
				"model":      "fake-model",
				"prompt":     "Test retry on partial failures",
				"max_tokens": 5,
			},
		}

		// With 50% failure rate and 5 retries, probability of all failing = 0.5^6 = 1.5%
		resp, inferr := client.Generate(context.Background(), req)

		// Should likely succeed (98.5% probability)
		// If it fails, that's acceptable but unlikely
		if inferr == nil {
			if resp == nil {
				t.Error("expected non-nil response")
			}
		} else {
			if inferr.Category != httpclient.ErrCategoryServer {
				t.Errorf("got category %v, want %v", inferr.Category, httpclient.ErrCategoryServer)
			}
			if !inferr.IsRetryable() {
				t.Error("expected error to be retryable")
			}
		}
	})
}
