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
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Integration tests using llm-d-inference-sim mock server running in Docker
//
// Each test spawns its own mock server instance with specific configuration
//
// Run tests with:
//   make test-integration
//   Or manually: go test -v -tags=integration ./internal/shared/batch/...

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

func TestHTTPInferenceClientBasicInference(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION_TESTS") == "true" {
		t.Skip("Integration tests skipped")
	}

	const testPort = 8200

	// Start mock server with default configuration
	err := startMockServer(testPort, "--mode=random")
	if err != nil {
		t.Skipf("Could not start mock server: %v", err)
	}
	t.Cleanup(func() { stopMockServer(testPort) })

	client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
		BaseURL: fmt.Sprintf("http://localhost:%d", testPort),
		Timeout: 10 * time.Second,
	})

	t.Run("should successfully make text completion request", func(t *testing.T) {
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

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		if resp.RequestID != "test-completion-001" {
			t.Errorf("expected RequestID %q, got %q", "test-completion-001", resp.RequestID)
		}
		if len(resp.Response) == 0 {
			t.Error("expected non-empty response")
		}

		// Verify response structure
		var result map[string]interface{}
		unmarshalErr := json.Unmarshal(resp.Response, &result)
		if unmarshalErr != nil {
			t.Fatalf("failed to unmarshal response: %v", unmarshalErr)
		}
		if result["id"] == nil {
			t.Error("expected id field in response")
		}
		if result["choices"] == nil {
			t.Error("expected choices field in response")
		}
	})

	t.Run("should successfully make chat completion request", func(t *testing.T) {
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

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}

		// Verify response structure
		var result map[string]interface{}
		unmarshalErr := json.Unmarshal(resp.Response, &result)
		if unmarshalErr != nil {
			t.Fatalf("failed to unmarshal response: %v", unmarshalErr)
		}
		if result["choices"] == nil {
			t.Error("expected choices field in response")
		}
	})

	t.Run("should handle multiple sequential requests", func(t *testing.T) {
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

			if err != nil {
				t.Fatalf("request %d failed: %v", i, err)
			}
			if resp == nil {
				t.Fatalf("request %d returned nil response", i)
			}
		}
	})

	t.Run("should handle concurrent requests correctly", func(t *testing.T) {
		// Verifies connection pooling and thread safety
		const numRequests = 10
		results := make(chan *InferenceError, numRequests)

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

func TestHTTPInferenceClientLatencySimulation(t *testing.T) {
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
		t.Skipf("Could not start mock server: %v", err)
	}
	t.Cleanup(func() { stopMockServer(testPort) })

	client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
		BaseURL: fmt.Sprintf("http://localhost:%d", testPort),
		Timeout: 10 * time.Second,
	})

	t.Run("should handle time-to-first-token latency", func(t *testing.T) {
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

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		// Should take at least 200ms for TTFT
		if duration < 180*time.Millisecond {
			t.Errorf("expected duration >= 180ms, got %v", duration)
		}
		if duration >= 2*time.Second {
			t.Errorf("expected duration < 2s, got %v", duration)
		}
	})

	t.Run("should handle inter-token latency", func(t *testing.T) {
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

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
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

func TestHTTPInferenceClientFailureInjection(t *testing.T) {
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
			t.Skipf("Could not start mock server: %v", err)
		}
		t.Cleanup(func() { stopMockServer(testPort) })

		client := NewHTTPInferenceClient(HTTPInferenceClientConfig{
			BaseURL:        fmt.Sprintf("http://localhost:%d", testPort),
			Timeout:        10 * time.Second,
			MaxRetries:     5,
			InitialBackoff: 50 * time.Millisecond,
		})

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
		resp, inferr := client.Generate(context.Background(), req)

		// Should likely succeed (98.5% probability)
		// If it fails, that's acceptable but unlikely
		if inferr == nil {
			if resp == nil {
				t.Error("expected non-nil response when error is nil")
			}
		} else {
			// If it did fail, verify it's the right type
			if inferr.Category != ErrCategoryServer {
				t.Errorf("expected error category %v, got %v", ErrCategoryServer, inferr.Category)
			}
			if !inferr.IsRetryable() {
				t.Error("expected error to be retryable")
			}
		}
	})
}
