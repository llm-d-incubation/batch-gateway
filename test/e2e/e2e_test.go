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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

var (
	baseURL  = getEnvOrDefault("TEST_BASE_URL", "http://localhost:8000")
	tenantID = getEnvOrDefault("TEST_TENANT_ID", "default")

	testRunID = fmt.Sprintf("%d", time.Now().UnixNano())
)

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// testJSONL is a valid batch input file with two requests
var testJSONL = strings.Join([]string{
	`{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"sim-model","messages":[{"role":"user","content":"Hello"}]}}`,
	`{"custom_id":"req-2","method":"POST","url":"/v1/chat/completions","body":{"model":"sim-model","messages":[{"role":"user","content":"World"}]}}`,
}, "\n")

func newClient() *openai.Client {
	c := openai.NewClient(
		option.WithBaseURL(baseURL+"/v1/"),
		option.WithAPIKey("unused"),
		option.WithHeader("X-MaaS-Username", tenantID),
	)
	return &c
}

func validateAndLogJSONL(t *testing.T, label string, content string) {
	t.Helper()

	var pretty strings.Builder
	lines := strings.Split(strings.TrimSpace(content), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			t.Errorf("%s: line %d is not valid JSON: %q", label, i+1, line)
			pretty.WriteString(line + "\n")
			continue
		}
		// pretty print json
		var buf bytes.Buffer
		if err := json.Indent(&buf, []byte(line), "", "  "); err == nil {
			pretty.WriteString(buf.String() + "\n")
		} else {
			pretty.WriteString(line + "\n")
		}
	}
	t.Logf("=== %s ===\n%s", label, strings.TrimSpace(pretty.String()))
}

func mustCreateFile(t *testing.T, filename, content string) string {
	t.Helper()

	file, err := newClient().Files.New(context.Background(),
		openai.FileNewParams{
			File:    openai.File(strings.NewReader(content), filename, "application/jsonl"),
			Purpose: openai.FilePurposeBatch,
		})
	if err != nil {
		t.Fatalf("create file failed: %v", err)
	}
	if file.ID == "" {
		t.Fatal("create file response has empty ID")
	}
	if file.Filename != filename {
		t.Errorf("expected filename %q, got %q", filename, file.Filename)
	}
	if file.Purpose != openai.FileObjectPurposeBatch {
		t.Errorf("expected purpose %q, got %q", openai.FileObjectPurposeBatch, file.Purpose)
	}
	return file.ID
}

func mustCreateBatch(t *testing.T, fileID string) string {
	t.Helper()

	batch, err := newClient().Batches.New(context.Background(),
		openai.BatchNewParams{
			InputFileID:      fileID,
			Endpoint:         openai.BatchNewParamsEndpointV1ChatCompletions,
			CompletionWindow: openai.BatchNewParamsCompletionWindow24h,
		})
	if err != nil {
		t.Fatalf("create batch failed: %v", err)
	}
	if batch.ID == "" {
		t.Fatal("create batch response has empty ID")
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
	return batch.ID
}

// ── Files subtests ────────────────────────────────────────────────────────────

// doTestFileLifecycle uploads a file, verifies list, retrieve, download, then deletes it.
func doTestFileLifecycle(t *testing.T) {
	t.Helper()

	client := newClient()

	// Create
	filename := fmt.Sprintf("test-file-lifecycle-%s.jsonl", testRunID)
	fileID := mustCreateFile(t, filename, testJSONL)

	// List
	page, err := client.Files.List(context.Background(), openai.FileListParams{})
	if err != nil {
		t.Fatalf("list files failed: %v", err)
	}
	t.Logf("list files: got %d items", len(page.Data))
	/*
		for _, f := range page.Data {
			t.Logf("  file: id=%s name=%s purpose=%s", f.ID, f.Filename, f.Purpose)
		}
	*/

	// Retrieve
	got, err := client.Files.Get(context.Background(), fileID)
	if err != nil {
		t.Fatalf("retrieve file failed: %v", err)
	}
	if got.ID != fileID {
		t.Errorf("expected ID %q, got %q", fileID, got.ID)
	}
	if got.Filename != filename {
		t.Errorf("expected filename %q, got %q", filename, got.Filename)
	}
	if got.Purpose != openai.FileObjectPurposeBatch {
		t.Errorf("expected purpose %q, got %q", openai.FileObjectPurposeBatch, got.Purpose)
	}

	// Download
	resp, err := client.Files.Content(context.Background(), fileID)
	if err != nil {
		t.Fatalf("download file failed: %v", err)
	}
	content, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("failed to read file content: %v", err)
	}
	if strings.TrimSpace(string(content)) != strings.TrimSpace(testJSONL) {
		t.Errorf("downloaded content does not match uploaded content\ngot:  %q\nwant: %q", string(content), testJSONL)
	}

	// Delete and verify a subsequent Get returns 404.
	result, err := client.Files.Delete(context.Background(), fileID)
	if err != nil {
		t.Fatalf("delete file failed: %v", err)
	}
	if !result.Deleted {
		t.Error("expected deleted to be true")
	}

	_, err = client.Files.Get(context.Background(), fileID)
	if err == nil {
		t.Error("expected error after deletion, got nil")
	} else {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 after deletion, got %d", apiErr.StatusCode)
		}
	}
}

// ── Batches subtests ──────────────────────────────────────────────────────────

func doTestBatchCancel(t *testing.T) {
	t.Helper()

	fileID := mustCreateFile(t, fmt.Sprintf("test-batch-cancel-%s.jsonl", testRunID), testJSONL)
	batchID := mustCreateBatch(t, fileID)

	batch, err := newClient().Batches.Cancel(context.Background(), batchID)
	if err != nil {
		t.Fatalf("cancel batch failed: %v", err)
	}

	if batch.Status != openai.BatchStatusCancelled && batch.Status != openai.BatchStatusCancelling {
		t.Errorf("expected status to be cancelled or cancelling, got %q", batch.Status)
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
	/*
		for _, b := range page.Data {
			t.Logf("  batch: id=%s status=%s", b.ID, b.Status)
		}
	*/

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

	// Wait until complete
	const (
		pollInterval = 5 * time.Second
		maxWait      = 5 * time.Minute
	)

	isTerminal := func(s openai.BatchStatus) bool {
		switch s {
		case openai.BatchStatusCompleted, openai.BatchStatusFailed,
			openai.BatchStatusExpired, openai.BatchStatusCancelled:
			return true
		}
		return false
	}

	var finalBatch *openai.Batch

	deadline := time.Now().Add(maxWait)
	if d, ok := t.Deadline(); ok && d.Before(deadline) {
		deadline = d.Add(-5 * time.Second) // leave margin for cleanup
	}
	for time.Now().Before(deadline) {
		batch, err := client.Batches.Get(context.Background(), batchID)
		if err != nil {
			t.Fatalf("retrieve batch failed: %v", err)
		}
		finalBatch = batch

		t.Logf("batch %s status: %s (completed=%d, failed=%d)",
			batchID, batch.Status,
			batch.RequestCounts.Completed, batch.RequestCounts.Failed)

		if isTerminal(batch.Status) {
			break
		}
		time.Sleep(pollInterval)
	}

	if finalBatch == nil || !isTerminal(finalBatch.Status) {
		t.Fatalf("batch %s did not reach a final state within %v (last status: %q)",
			batchID, maxWait, finalBatch.Status)
	}
	t.Logf("batch %s reached terminal state: status=%s total=%d completed=%d failed=%d output_file_id=%q error_file_id=%q",
		batchID, finalBatch.Status,
		finalBatch.RequestCounts.Total, finalBatch.RequestCounts.Completed, finalBatch.RequestCounts.Failed,
		finalBatch.OutputFileID, finalBatch.ErrorFileID)

	if finalBatch.Status != openai.BatchStatusCompleted {
		t.Errorf("expected batch status %q, got %q", openai.BatchStatusCompleted, finalBatch.Status)
	}
	if finalBatch.OutputFileID == "" {
		t.Error("completed batch has empty output_file_id")
	}
	if finalBatch.ErrorFileID != "" {
		t.Error("completed batch has non-empty error_file_id")
	}
	if finalBatch.RequestCounts.Total != 2 {
		t.Errorf("expected request_counts.total=2, got %d", finalBatch.RequestCounts.Total)
	}
	if finalBatch.RequestCounts.Completed != 2 {
		t.Errorf("expected request_counts.completed=2, got %d (failed=%d)",
			finalBatch.RequestCounts.Completed, finalBatch.RequestCounts.Failed)
	}

	// Download output/error
	if finalBatch.OutputFileID != "" {
		resp, err := client.Files.Content(context.Background(), finalBatch.OutputFileID)
		if err != nil {
			t.Errorf("failed to download output file %q: %v", finalBatch.OutputFileID, err)
		} else {
			content, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			validateAndLogJSONL(t, "output file "+finalBatch.OutputFileID, string(content))
		}
	}
	if finalBatch.ErrorFileID != "" {
		resp, err := client.Files.Content(context.Background(), finalBatch.ErrorFileID)
		if err != nil {
			t.Errorf("failed to download error file %q: %v", finalBatch.ErrorFileID, err)
		} else {
			content, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			validateAndLogJSONL(t, "error file "+finalBatch.ErrorFileID, string(content))
		}
	}
}

// ── Entry point ──────────────────────────────────────────────────────────

func TestE2E(t *testing.T) {
	const readyTimeout = 30 * time.Second
	readyDeadline := time.Now().Add(readyTimeout)
	for {
		resp, err := http.Get(baseURL + "/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(readyDeadline) {
			if err != nil {
				t.Fatalf("server not ready after %v: %v; ensure the API server is running at %s", readyTimeout, err, baseURL)
			}
			t.Fatalf("server not ready after %v (status %d); ensure the API server is running at %s", readyTimeout, resp.StatusCode, baseURL)
		}
		time.Sleep(time.Second)
	}

	t.Run("Health", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/health")
		if err != nil {
			t.Fatalf("GET /health failed: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200 from /health, got %d", resp.StatusCode)
		}
	})

	t.Run("Files", func(t *testing.T) {
		t.Run("Lifecycle", func(t *testing.T) { doTestFileLifecycle(t) })
	})

	t.Run("Batches", func(t *testing.T) {
		t.Run("Lifecycle", func(t *testing.T) { doTestBatchLifecycle(t) })
		t.Run("Cancel", func(t *testing.T) { doTestBatchCancel(t) })
	})
}
