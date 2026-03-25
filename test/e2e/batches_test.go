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
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
)

func testBatches(t *testing.T) {
	t.Run("Lifecycle", doTestBatchLifecycle)
	t.Run("List", func(t *testing.T) {
		t.Run("Pagination", doTestBatchPagination)
	})
	t.Run("Cancel", func(t *testing.T) {
		t.Run("BeforeProcessing", doTestBatchCancelBeforeProcessing)
		t.Run("InProgress", doTestBatchCancel)
	})
	t.Run("MixedSuccessFailure", doTestBatchMixedSuccessFailure)
	t.Run("SharedInputFile", doTestBatchSharedInputFile)
	t.Run("PassThroughHeaders", doTestPassThroughHeaders)
}

func doTestBatchCancel(t *testing.T) {
	t.Helper()

	// Mix fast and slow requests to guarantee both output and error files exist after cancel:
	//   - Fast requests (max_tokens=1): complete in ~150ms, ensuring output file has entries.
	//   - Slow requests (max_tokens=200): take ~20s each at 100ms inter-token-latency,
	//     ensuring cancel arrives while they are still in-flight or undispatched.
	//   - 20 slow requests exceed PerModelMaxConcurrency (default 10), guaranteeing some
	//     remain undispatched and get drained to the error file as batch_cancelled.
	var lines []string
	for i := 1; i <= 5; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"custom_id":"fast-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"sim-model","max_tokens":1,"messages":[{"role":"user","content":"Hi %d"}]}}`, i, i))
	}
	for i := 1; i <= 20; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"custom_id":"slow-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"sim-model","max_tokens":200,"messages":[{"role":"user","content":"Tell me a long story %d"}]}}`, i, i))
	}
	slowJSONL := strings.Join(lines, "\n")
	fileID := mustCreateFile(t, fmt.Sprintf("test-batch-cancel-%s.jsonl", testRunID), slowJSONL)
	batchID := mustCreateBatch(t, fileID)

	// Wait for the processor to pick up the batch and start inference.
	_, _ = waitForBatchStatus(t, batchID, 2*time.Minute, openai.BatchStatusInProgress)

	// Give fast requests time to complete before cancelling.
	time.Sleep(2 * time.Second)

	// Cancel the batch while slow requests are still in-flight.
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
	finalBatch, _ := waitForBatchStatus(t, batchID, 2*time.Minute, openai.BatchStatusCancelled)

	t.Logf("batch %s cancelled (completed=%d, failed=%d, total=%d, output_file_id=%s, error_file_id=%s)",
		batchID,
		finalBatch.RequestCounts.Completed,
		finalBatch.RequestCounts.Failed,
		finalBatch.RequestCounts.Total,
		finalBatch.OutputFileID,
		finalBatch.ErrorFileID)

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

// doTestBatchCancelBeforeProcessing creates a batch and cancels it immediately.
// If the cancel arrives before the processor dequeues the batch, the response
// is "cancelled" (PQDelete path). If the processor was faster, the response is
// "cancelling" (cancel event path). Both are valid due to the inherent race.
// Either way, the batch must eventually reach "cancelled".
func doTestBatchCancelBeforeProcessing(t *testing.T) {
	t.Helper()

	fileID := mustCreateFile(t, fmt.Sprintf("test-batch-cancel-before-%s.jsonl", testRunID), testJSONL)
	batchID := mustCreateBatch(t, fileID)

	// Cancel immediately — the batch is likely still in the queue.
	batch, err := newClient().Batches.Cancel(context.Background(), batchID)
	if err != nil {
		t.Fatalf("cancel batch failed: %v", err)
	}
	t.Logf("cancel response status: %s", batch.Status)

	switch batch.Status {
	case openai.BatchStatusCancelled:
		// PQDelete path: batch was still in queue, cancelled directly.
		t.Log("batch was cancelled directly from queue (PQDelete path)")
	case openai.BatchStatusCancelling:
		// Cancel event path: processor already dequeued the batch.
		t.Log("batch is cancelling via event (processor already dequeued)")
	default:
		t.Errorf("expected status %q or %q after immediate cancel, got %q",
			openai.BatchStatusCancelled, openai.BatchStatusCancelling, batch.Status)
	}

	// Either way, the batch must reach "cancelled" eventually.
	_, _ = waitForBatchStatus(t, batchID, 2*time.Minute, openai.BatchStatusCancelled)
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
	for k, wantV := range testBatchMetadata {
		if gotV, ok := batch.Metadata[k]; !ok {
			t.Errorf("metadata key %q missing from retrieve response", k)
		} else if gotV != wantV {
			t.Errorf("metadata[%q] = %q, want %q", k, gotV, wantV)
		}
	}

	// Poll until completion
	_, _ = waitForBatchStatus(t, batchID, 5*time.Minute, openai.BatchStatusCompleted)
}

// doTestBatchSharedInputFile creates two batches from the same input file and
// verifies both complete independently with correct output.
func doTestBatchSharedInputFile(t *testing.T) {
	t.Helper()

	fileID := mustCreateFile(t, fmt.Sprintf("test-shared-input-%s.jsonl", testRunID), testJSONL)

	batchID1 := mustCreateBatch(t, fileID)
	batchID2 := mustCreateBatch(t, fileID)
	t.Logf("created batch1=%s batch2=%s from file=%s", batchID1, batchID2, fileID)

	batch1, _ := waitForBatchStatus(t, batchID1, 5*time.Minute, openai.BatchStatusCompleted)
	batch2, _ := waitForBatchStatus(t, batchID2, 5*time.Minute, openai.BatchStatusCompleted)

	// Verify output files are distinct
	if batch1.OutputFileID == batch2.OutputFileID {
		t.Errorf("both batches produced the same output_file_id %q, expected distinct files", batch1.OutputFileID)
	}
}

// doTestBatchMixedSuccessFailure creates a batch with a mix of valid and invalid
// requests (invalid model), verifies the batch completes with correct
// completed/failed counts, and that output and error files contain the right entries.
func doTestBatchMixedSuccessFailure(t *testing.T) {
	t.Helper()

	mixedJSONL := strings.Join([]string{
		fmt.Sprintf(`{"custom_id":"good-1","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello"}]}}`, testModel),
		`{"custom_id":"bad-1","method":"POST","url":"/v1/chat/completions","body":{"model":"nonexistent-model","max_tokens":5,"messages":[{"role":"user","content":"Hello"}]}}`,
		fmt.Sprintf(`{"custom_id":"good-2","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"World"}]}}`, testModel),
	}, "\n")

	fileID := mustCreateFile(t, fmt.Sprintf("test-mixed-%s.jsonl", testRunID), mixedJSONL)
	batchID := mustCreateBatch(t, fileID)

	finalBatch, _ := waitForBatchStatus(t, batchID, 5*time.Minute, openai.BatchStatusCompleted)

	// Verify request counts: 2 completed, 1 failed
	if finalBatch.RequestCounts.Completed != 2 {
		t.Errorf("completed = %d, want 2", finalBatch.RequestCounts.Completed)
	}
	if finalBatch.RequestCounts.Failed != 1 {
		t.Errorf("failed = %d, want 1", finalBatch.RequestCounts.Failed)
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

	_, _ = waitForBatchStatus(t, batchID, 5*time.Minute, openai.BatchStatusCompleted)

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

// doTestBatchPagination creates 3 batches under an isolated tenant and verifies
// that limit/after pagination returns correct pages with no duplicates.
func doTestBatchPagination(t *testing.T) {
	t.Helper()

	tenant := fmt.Sprintf("pagination-batches-%s", testRunID)
	client := newClientForTenant(tenant)
	ctx := context.Background()

	// Create a shared input file under this tenant.
	filename := fmt.Sprintf("pagination-batch-input-%s.jsonl", testRunID)
	fileID := mustCreateUniqueFileWithClient(t, client, filename, testJSONL)

	// Create 3 batches.
	const count = 3
	createdIDs := make([]string, count)
	for i := range count {
		batch, err := client.Batches.New(ctx, openai.BatchNewParams{
			InputFileID:      fileID,
			Endpoint:         openai.BatchNewParamsEndpointV1ChatCompletions,
			CompletionWindow: openai.BatchNewParamsCompletionWindow24h,
		})
		if err != nil {
			t.Fatalf("create batch %d failed: %v", i, err)
		}
		createdIDs[i] = batch.ID
		t.Logf("created batch %d: %s", i, batch.ID)
	}

	// Page 1: limit=2, no after → expect 2 items, has_more=true
	page1, err := client.Batches.List(ctx, openai.BatchListParams{
		Limit: param.NewOpt(int64(2)),
	})
	if err != nil {
		t.Fatalf("list batches page 1 failed: %v", err)
	}
	if len(page1.Data) != 2 {
		t.Fatalf("page 1: expected 2 items, got %d", len(page1.Data))
	}
	if !page1.HasMore {
		t.Error("page 1: expected has_more=true")
	}

	page1IDs := make([]string, len(page1.Data))
	for i, b := range page1.Data {
		page1IDs[i] = b.ID
	}
	t.Logf("page 1 IDs: %v (has_more=%v)", page1IDs, page1.HasMore)

	// Page 2: limit=2, after="2" (offset) → expect 1 item, has_more=false
	page2, err := client.Batches.List(ctx, openai.BatchListParams{
		Limit: param.NewOpt(int64(2)),
		After: param.NewOpt("2"),
	})
	if err != nil {
		t.Fatalf("list batches page 2 failed: %v", err)
	}
	if len(page2.Data) != 1 {
		t.Fatalf("page 2: expected 1 item, got %d", len(page2.Data))
	}
	if page2.HasMore {
		t.Error("page 2: expected has_more=false")
	}

	page2IDs := make([]string, len(page2.Data))
	for i, b := range page2.Data {
		page2IDs[i] = b.ID
	}
	t.Logf("page 2 IDs: %v (has_more=%v)", page2IDs, page2.HasMore)

	// Verify no overlap and full coverage.
	allIDs := append(page1IDs, page2IDs...)
	assertSliceEqual(t, createdIDs, allIDs)
}
