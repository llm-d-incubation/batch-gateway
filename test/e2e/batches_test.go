// Copyright 2026 The llm-d Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e_test

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func testBatches(t *testing.T) {
	t.Run("Lifecycle", doTestBatchLifecycle)
	t.Run("Cancel", doTestBatchCancel)
	t.Run("PassThroughHeaders", doTestPassThroughHeaders)
}

func doTestBatchCancel(t *testing.T) {
	t.Helper()

	// Use high max_tokens so each request takes ~50s at 500ms inter-token-latency,
	// ensuring the cancel event arrives before inference completes.
	slowJSONL := strings.Join([]string{
		`{"custom_id":"slow-1","method":"POST","url":"/v1/chat/completions","body":{"model":"sim-model","max_tokens":100,"messages":[{"role":"user","content":"Tell me a story"}]}}`,
		`{"custom_id":"slow-2","method":"POST","url":"/v1/chat/completions","body":{"model":"sim-model","max_tokens":100,"messages":[{"role":"user","content":"Tell me a joke"}]}}`,
		`{"custom_id":"slow-3","method":"POST","url":"/v1/chat/completions","body":{"model":"sim-model","max_tokens":100,"messages":[{"role":"user","content":"Tell me a poem"}]}}`,
		`{"custom_id":"slow-4","method":"POST","url":"/v1/chat/completions","body":{"model":"sim-model","max_tokens":100,"messages":[{"role":"user","content":"Tell me a fact"}]}}`,
	}, "\n")
	fileID := mustCreateFile(t, fmt.Sprintf("test-batch-cancel-%s.jsonl", testRunID), slowJSONL)
	batchID := mustCreateBatch(t, fileID)

	// Wait for the processor to pick up the batch and start inference.
	waitForBatchStatus(t, batchID, 2*time.Minute, openai.BatchStatusInProgress)

	// Cancel the batch while inference is running.
	batch, err := newClient().Batches.Cancel(context.Background(), batchID)
	if err != nil {
		t.Fatalf("cancel batch failed: %v", err)
	}
	t.Logf("cancel response status: %s", batch.Status)

	// The cancel response should be cancelling (batch is in_progress, so
	// the apiserver sends a cancel event rather than directly cancelling).
	if batch.Status != openai.BatchStatusCancelling {
		t.Errorf("expected status %q immediately after cancel call, got %q",
			openai.BatchStatusCancelling, batch.Status)
	}

	// Wait for the batch to reach cancelled state.
	finalBatch := waitForBatchStatus(t, batchID, 2*time.Minute, openai.BatchStatusCancelled)

	t.Logf("batch %s cancelled (completed=%d, failed=%d, total=%d)",
		batchID,
		finalBatch.RequestCounts.Completed,
		finalBatch.RequestCounts.Failed,
		finalBatch.RequestCounts.Total)

	if finalBatch.RequestCounts.Total != int64(len(strings.Split(strings.TrimSpace(slowJSONL), "\n"))) {
		t.Errorf("Total = %d, want %d", finalBatch.RequestCounts.Total, len(strings.Split(strings.TrimSpace(slowJSONL), "\n")))
	}
	if finalBatch.RequestCounts.Completed+finalBatch.RequestCounts.Failed != finalBatch.RequestCounts.Total {
		t.Errorf("Completed(%d) + Failed(%d) != Total(%d)",
			finalBatch.RequestCounts.Completed, finalBatch.RequestCounts.Failed, finalBatch.RequestCounts.Total)
	}
	if finalBatch.RequestCounts.Completed >= finalBatch.RequestCounts.Total {
		t.Errorf("expected some requests to not complete after cancellation, but all %d completed",
			finalBatch.RequestCounts.Total)
	}

	// Verify that in-flight inference requests were actually aborted by the inference client
	// (context cancellation propagated through inferCtx → execCtx → HTTP request).
	if testKubectlAvailable {
		out, err := exec.Command("kubectl", "logs",
			"-l", fmt.Sprintf("app.kubernetes.io/instance=%s,app.kubernetes.io/component=processor", testHelmRelease),
			"-n", testNamespace,
			"--tail=500",
		).CombinedOutput()
		if err != nil {
			t.Logf("kubectl logs failed (non-fatal): %v\n%s", err, out)
		} else {
			logs := string(out)
			if !strings.Contains(logs, "Request cancelled for request_id") {
				t.Errorf("expected processor logs to contain 'Request cancelled for request_id', indicating in-flight HTTP requests were aborted")
			}
		}
	}
}

// doTestBatchLifecycle creates a fresh batch, verifies list and retrieve operations,
// polls until it reaches a terminal state, then asserts it completed successfully
// and prints the output/error file contents.
func doTestBatchLifecycle(t *testing.T) {
	t.Helper()

	client := newClient()

	// Create
	fileID := mustCreateFile(t, fmt.Sprintf("test-batch-lifecycle-%s.jsonl", testRunID), testJSONL)
	batchID := mustCreateBatch(t, fileID)

	// List
	page, err := client.Batches.List(context.Background(), openai.BatchListParams{})
	if err != nil {
		t.Fatalf("list batches failed: %v", err)
	}
	t.Logf("list batches: got %d items", len(page.Data))

	// Retrieve
	batch, err := client.Batches.Get(context.Background(), batchID)
	if err != nil {
		t.Fatalf("retrieve batch failed: %v", err)
	}
	if batch.ID != batchID {
		t.Errorf("expected ID %q, got %q", batchID, batch.ID)
	}
	if batch.InputFileID != fileID {
		t.Errorf("expected input_file_id %q, got %q", fileID, batch.InputFileID)
	}
	if batch.Endpoint != "/v1/chat/completions" {
		t.Errorf("expected endpoint %q, got %q", "/v1/chat/completions", batch.Endpoint)
	}
	if batch.CompletionWindow != "24h" {
		t.Errorf("expected completion_window %q, got %q", "24h", batch.CompletionWindow)
	}

	// Poll until completion
	finalBatch := waitForBatchCompletion(t, batchID)

	if finalBatch.Status != openai.BatchStatusCompleted {
		t.Fatalf("expected batch status %q, got %q", openai.BatchStatusCompleted, finalBatch.Status)
	}

	// Download and log output file
	if finalBatch.OutputFileID != "" {
		resp, err := client.Files.Content(context.Background(), finalBatch.OutputFileID)
		if err != nil {
			t.Fatalf("download output file failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		validateAndLogJSONL(t, "output file", string(body))
	}

	// Download and log error file (if any)
	if finalBatch.ErrorFileID != "" {
		resp, err := client.Files.Content(context.Background(), finalBatch.ErrorFileID)
		if err != nil {
			t.Fatalf("download error file failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		validateAndLogJSONL(t, "error file", string(body))
	}
}

// doTestPassThroughHeaders creates a batch with pass-through headers, waits for
// completion, then verifies the processor logged the expected header names.
func doTestPassThroughHeaders(t *testing.T) {
	t.Helper()

	// Verify processor logs contain the pass-through header names
	if !testKubectlAvailable {
		t.Skip("kubectl not available, skipping processor log verification")
	}

	// Create batch with pass-through headers
	fileID := mustCreateFile(t, fmt.Sprintf("test-pass-through-headers-%s.jsonl", testRunID), testJSONL)

	var headerOpts []option.RequestOption
	for k, v := range testPassThroughHeaders {
		headerOpts = append(headerOpts, option.WithHeader(k, v))
	}

	batchID := mustCreateBatch(t, fileID, headerOpts...)

	finalBatch := waitForBatchCompletion(t, batchID)

	if finalBatch.Status != openai.BatchStatusCompleted {
		t.Fatalf("expected batch status %q, got %q", openai.BatchStatusCompleted, finalBatch.Status)
	}

	out, err := exec.Command("kubectl", "logs",
		"-l", fmt.Sprintf("app.kubernetes.io/instance=%s,app.kubernetes.io/component=processor", testHelmRelease),
		"-n", testNamespace,
		"--tail=500",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl logs failed: %v\n%s", err, out)
	}

	logs := string(out)
	for headerName := range testPassThroughHeaders {
		if !strings.Contains(logs, headerName) {
			t.Errorf("expected processor logs to contain header name %q, but it was not found", headerName)
		}
	}
}
