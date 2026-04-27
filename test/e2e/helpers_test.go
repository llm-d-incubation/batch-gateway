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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// detectDBClientType queries the Helm release for the configured DB client type.
// Falls back to "postgresql" if the value cannot be detected.
func detectDBClientType(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("helm", "get", "values", testHelmRelease,
		"-n", testNamespace, "-o", "json",
	).CombinedOutput()
	if err != nil {
		t.Logf("helm get values failed, defaulting to postgresql: %v", err)
		return "postgresql"
	}
	var vals struct {
		Global struct {
			DBClient struct {
				Type string `json:"type"`
			} `json:"dbClient"`
		} `json:"global"`
	}
	if err := json.Unmarshal(out, &vals); err != nil || vals.Global.DBClient.Type == "" {
		return "postgresql"
	}
	return vals.Global.DBClient.Type
}

// detectExchangeClientType checks the chart installed for the Redis/Valkey
// Helm release and returns "valkey" if the chart name starts with "valkey",
// otherwise "redis".
func detectExchangeClientType(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("helm", "get", "metadata", testRedisRelease,
		"-n", testNamespace, "-o", "json",
	).CombinedOutput()
	if err != nil {
		t.Logf("helm get metadata %s failed, defaulting to redis: %v", testRedisRelease, err)
		return "redis"
	}
	var meta struct {
		Chart string `json:"chart"`
	}
	if err := json.Unmarshal(out, &meta); err != nil {
		t.Logf("failed to parse helm metadata, defaulting to redis: %v", err)
		return "redis"
	}
	if strings.HasPrefix(meta.Chart, "valkey") {
		return "valkey"
	}
	return "redis"
}

// ── Client helpers ───────────────────────────────────────────────────────

func newClient() *openai.Client {
	return newClientForTenant(testTenantID)
}

func newClientForTenant(tenant string) *openai.Client {
	c := openai.NewClient(
		option.WithBaseURL(testApiserverURL+"/v1/"),
		option.WithAPIKey("unused"),
		option.WithHeader(testTenantHeader, tenant),
		option.WithHTTPClient(testHTTPClient),
	)
	return &c
}

// ── File helpers ─────────────────────────────────────────────────────────

func mustCreateFile(t *testing.T, filename, content string) string {
	return mustCreateFileWithClient(t, newClient(), filename, content)
}

func mustCreateFileWithClient(t *testing.T, client *openai.Client, filename, content string) string {
	t.Helper()

	file, err := client.Files.New(context.Background(),
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

func mustCreateUniqueFileWithClient(t *testing.T, client *openai.Client, filename, content string) string {
	// Add unique suffix to prevent conflicts when running tests multiple times
	uniqueFilename := fmt.Sprintf("%s-%d.jsonl",
		strings.TrimSuffix(filename, ".jsonl"),
		time.Now().UnixNano())

	return mustCreateFileWithClient(t, client, uniqueFilename, content)
}

// ── Batch helpers ────────────────────────────────────────────────────────

func mustCreateBatch(t *testing.T, fileID string, opts ...option.RequestOption) string {
	t.Helper()

	batch, err := newClient().Batches.New(context.Background(),
		openai.BatchNewParams{
			InputFileID:      fileID,
			Endpoint:         openai.BatchNewParamsEndpointV1ChatCompletions,
			CompletionWindow: openai.BatchNewParamsCompletionWindow24h,
			Metadata:         testBatchMetadata,
		},
		opts...,
	)
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
	for k, wantV := range testBatchMetadata {
		if gotV, ok := batch.Metadata[k]; !ok {
			t.Errorf("metadata key %q missing from create response", k)
		} else if gotV != wantV {
			t.Errorf("metadata[%q] = %q, want %q", k, gotV, wantV)
		}
	}
	return batch.ID
}

// createBatchRaw calls the batch creation API and returns the response or error
// without fataling. Used by validation tests that expect errors.
func createBatchRaw(client *openai.Client, params openai.BatchNewParams) (*openai.Batch, error) {
	return client.Batches.New(context.Background(), params)
}

// terminalBatchStatuses are statuses that a batch cannot transition out of.
var terminalBatchStatuses = map[openai.BatchStatus]bool{
	openai.BatchStatusCompleted: true,
	openai.BatchStatusFailed:    true,
	openai.BatchStatusExpired:   true,
	openai.BatchStatusCancelled: true,
}

// waitForBatchStatus polls a batch by ID until its status is one of the
// target statuses. It fatals if the batch reaches a terminal state that is
// not one of the targets, or if the timeout (or test deadline) is exceeded.
func waitForBatchStatus(t *testing.T, batchID string, timeout time.Duration, targets ...openai.BatchStatus) (*openai.Batch, *batchResults) {
	t.Helper()

	client := newClient()

	targetSet := make(map[openai.BatchStatus]bool, len(targets))
	for _, s := range targets {
		targetSet[s] = true
	}

	const pollInterval = 2 * time.Second

	var lastBatch *openai.Batch
	deadline := time.Now().Add(timeout)
	if d, ok := t.Deadline(); ok && d.Before(deadline) {
		deadline = d.Add(-5 * time.Second)
	}
	for time.Now().Before(deadline) {
		b, err := client.Batches.Get(context.Background(), batchID)
		if err != nil {
			t.Fatalf("retrieve batch failed: %v", err)
		}
		lastBatch = b

		t.Logf("batch %s status: %s (completed=%d, failed=%d)",
			batchID, b.Status,
			b.RequestCounts.Completed, b.RequestCounts.Failed)

		if terminalBatchStatuses[b.Status] {
			validateTerminalBatch(t, b)
			if !targetSet[b.Status] {
				t.Fatalf("batch %s reached terminal status %q, will never become %v",
					batchID, b.Status, targets)
			}
			res := fetchBatchResults(t, b)
			validateBatchResults(t, b, res)
			return b, &res
		}
		if targetSet[b.Status] {
			return b, nil
		}
		time.Sleep(pollInterval)
	}

	t.Fatalf("batch %s did not reach status %v within %v (last status: %q)",
		batchID, targets, timeout, lastBatch.Status)
	return nil, nil // unreachable
}

// waitForIngestionFailure polls a batch until it reaches "failed" status.
// Unlike waitForBatchStatus, it skips validateBatchResults (which rejects
// Total==0 for non-cancelled batches) and result-file fetching, since
// ingestion failures legitimately have Total==0 and no output files.
func waitForIngestionFailure(t *testing.T, batchID string, timeout time.Duration) *openai.Batch {
	t.Helper()

	client := newClient()
	deadline := time.Now().Add(timeout)
	if d, ok := t.Deadline(); ok && d.Before(deadline) {
		deadline = d.Add(-5 * time.Second)
	}

	for time.Now().Before(deadline) {
		b, err := client.Batches.Get(context.Background(), batchID)
		if err != nil {
			t.Fatalf("retrieve batch failed: %v", err)
		}
		t.Logf("batch %s status: %s", batchID, b.Status)

		if b.Status == openai.BatchStatusFailed {
			return b
		}
		if terminalBatchStatuses[b.Status] {
			t.Fatalf("batch %s reached terminal status %q instead of failed", batchID, b.Status)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("batch %s did not reach failed within %v", batchID, timeout)
	return nil
}

// ── Batch validation ─────────────────────────────────────────────────────

// validateTerminalBatch checks invariants that must hold for any batch in a terminal state:
// request counts, created_at, and status-specific timestamps.
func validateTerminalBatch(t *testing.T, b *openai.Batch) {
	t.Helper()

	if b.RequestCounts.Completed+b.RequestCounts.Failed != b.RequestCounts.Total {
		t.Errorf("batch %s: Completed(%d) + Failed(%d) != Total(%d)",
			b.ID, b.RequestCounts.Completed, b.RequestCounts.Failed, b.RequestCounts.Total)
	}
	if b.CreatedAt == 0 {
		t.Errorf("batch %s: created_at should be > 0", b.ID)
	}

	switch b.Status {
	case openai.BatchStatusCompleted:
		if b.CompletedAt == 0 {
			t.Errorf("batch %s: completed_at should be > 0", b.ID)
		}
		if b.CompletedAt < b.CreatedAt {
			t.Errorf("batch %s: completed_at (%d) < created_at (%d)", b.ID, b.CompletedAt, b.CreatedAt)
		}
		if b.InProgressAt != 0 && b.InProgressAt < b.CreatedAt {
			t.Errorf("batch %s: in_progress_at (%d) < created_at (%d)", b.ID, b.InProgressAt, b.CreatedAt)
		}
		if b.InProgressAt != 0 && b.CompletedAt < b.InProgressAt {
			t.Errorf("batch %s: completed_at (%d) < in_progress_at (%d)", b.ID, b.CompletedAt, b.InProgressAt)
		}

	case openai.BatchStatusCancelled:
		if b.CancelledAt == 0 {
			t.Errorf("batch %s: cancelled_at should be > 0", b.ID)
		}
		if b.CancelledAt < b.CreatedAt {
			t.Errorf("batch %s: cancelled_at (%d) < created_at (%d)", b.ID, b.CancelledAt, b.CreatedAt)
		}
		if b.CancellingAt != 0 && b.CancellingAt < b.CreatedAt {
			t.Errorf("batch %s: cancelling_at (%d) < created_at (%d)", b.ID, b.CancellingAt, b.CreatedAt)
		}
		if b.CancellingAt != 0 && b.CancelledAt < b.CancellingAt {
			t.Errorf("batch %s: cancelled_at (%d) < cancelling_at (%d)", b.ID, b.CancelledAt, b.CancellingAt)
		}
		if b.InProgressAt != 0 && b.RequestCounts.Failed == 0 {
			t.Errorf("batch %s: expected failed count > 0 for cancelled batch that was in progress", b.ID)
		}

	case openai.BatchStatusFailed:
		if b.FailedAt == 0 {
			t.Errorf("batch %s: failed_at should be > 0", b.ID)
		}
		if b.FailedAt < b.CreatedAt {
			t.Errorf("batch %s: failed_at (%d) < created_at (%d)", b.ID, b.FailedAt, b.CreatedAt)
		}

	case openai.BatchStatusExpired:
		if b.ExpiredAt == 0 {
			t.Errorf("batch %s: expired_at should be > 0", b.ID)
		}
		if b.ExpiredAt < b.CreatedAt {
			t.Errorf("batch %s: expired_at (%d) < created_at (%d)", b.ID, b.ExpiredAt, b.CreatedAt)
		}
	}
}

// batchResults holds downloaded output/error file bodies and derived line counts.
type batchResults struct {
	OutputLines int
	ErrorLines  int
	OutputBody  string
	ErrorBody   string
}

// batchResultLine represents a single line in the batch output or error JSONL file.
type batchResultLine struct {
	ID       string `json:"id"`
	CustomID string `json:"custom_id"`
	Response *struct {
		StatusCode int            `json:"status_code"`
		RequestID  string         `json:"request_id"`
		Body       map[string]any `json:"body"`
	} `json:"response"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// fetchBatchResults downloads the output and error files for a batch
// and returns their contents. It also verifies Content-Disposition headers.
func fetchBatchResults(t *testing.T, batch *openai.Batch) batchResults {
	t.Helper()

	var result batchResults
	client := newClient()

	if batch.OutputFileID != "" {
		resp, err := client.Files.Content(context.Background(), batch.OutputFileID)
		if err != nil {
			t.Fatalf("download output file failed: %v", err)
		}
		wantCD := fmt.Sprintf(`attachment; filename=%q`, fmt.Sprintf("batch_output_%s.jsonl", batch.ID))
		if cd := resp.Header.Get("Content-Disposition"); cd != wantCD {
			t.Errorf("output file Content-Disposition mismatch\ngot:  %s\nwant: %s", cd, wantCD)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		result.OutputBody = strings.TrimSpace(string(body))
		if result.OutputBody != "" {
			result.OutputLines = len(strings.Split(result.OutputBody, "\n"))
		}
	}

	if batch.ErrorFileID != "" {
		resp, err := client.Files.Content(context.Background(), batch.ErrorFileID)
		if err != nil {
			t.Fatalf("download error file failed: %v", err)
		}
		wantCD := fmt.Sprintf(`attachment; filename=%q`, fmt.Sprintf("batch_error_%s.jsonl", batch.ID))
		if cd := resp.Header.Get("Content-Disposition"); cd != wantCD {
			t.Errorf("error file Content-Disposition mismatch\ngot:  %s\nwant: %s", cd, wantCD)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		result.ErrorBody = strings.TrimSpace(string(body))
		if result.ErrorBody != "" {
			result.ErrorLines = len(strings.Split(result.ErrorBody, "\n"))
		}
	}

	t.Logf("file lines: output=%d, error=%d (batch total=%d)",
		result.OutputLines, result.ErrorLines, batch.RequestCounts.Total)

	return result
}

// validateBatchResults checks all universal invariants on the batch results:
//   - input lines == Total, output lines == Completed, error lines == Failed
//   - every non-empty input custom_id appears in either the output or error file
//   - output lines have valid response structure (status_code=200, choices, model)
//   - error lines have valid error structure (non-empty code and message)
//   - no duplicate custom_ids within output or error files
func validateBatchResults(t *testing.T, batch *openai.Batch, result batchResults) {
	t.Helper()

	if batch.RequestCounts.Total == 0 {
		// Only batch cancelled before the processor parsed the input file can legitimately have Total==0.
		if batch.Status != openai.BatchStatusCancelled {
			t.Errorf("batch %s: Total==0 but status is %q (only cancelled batches can have zero requests)",
				batch.ID, batch.Status)
		}
		return
	}

	// --- Validate output file ---
	outputCustomIDs := make(map[string]bool)
	for i, line := range strings.Split(result.OutputBody, "\n") {
		line = strings.TrimSpace(line)
		t.Logf("output line %d: %s", i+1, line)

		if line == "" {
			continue
		}

		var out batchResultLine
		if err := json.Unmarshal([]byte(line), &out); err != nil {
			t.Errorf("output line %d: invalid JSON: %v", i+1, err)
			continue
		}
		if out.ID == "" {
			t.Errorf("output line %d: missing id", i+1)
		}
		if out.CustomID == "" {
			t.Errorf("output line %d: missing custom_id", i+1)
			continue
		}
		if outputCustomIDs[out.CustomID] {
			t.Errorf("output line %d: duplicate custom_id %q", i+1, out.CustomID)
		}
		outputCustomIDs[out.CustomID] = true

		if out.Response == nil {
			t.Errorf("output line %d (custom_id=%s): response is null", i+1, out.CustomID)
			continue
		}
		if out.Response.StatusCode != 200 {
			t.Errorf("output line %d (custom_id=%s): status_code = %d, want 200",
				i+1, out.CustomID, out.Response.StatusCode)
		}
		if _, ok := out.Response.Body["choices"]; !ok {
			t.Errorf("output line %d (custom_id=%s): response body missing 'choices'", i+1, out.CustomID)
		}
		if _, ok := out.Response.Body["model"]; !ok {
			t.Errorf("output line %d (custom_id=%s): response body missing 'model'", i+1, out.CustomID)
		}
		if usage, ok := out.Response.Body["usage"]; !ok {
			t.Errorf("output line %d (custom_id=%s): response body missing 'usage'", i+1, out.CustomID)
		} else if usageMap, ok := usage.(map[string]any); !ok {
			t.Errorf("output line %d (custom_id=%s): usage is not an object", i+1, out.CustomID)
		} else {
			for _, key := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
				if _, ok := usageMap[key]; !ok {
					t.Errorf("output line %d (custom_id=%s): usage missing '%s'", i+1, out.CustomID, key)
				}
			}
		}
	}

	// --- Validate error file ---
	errorCustomIDs := make(map[string]bool)
	for i, line := range strings.Split(result.ErrorBody, "\n") {
		line = strings.TrimSpace(line)
		t.Logf("error line %d: %s", i+1, line)

		if line == "" {
			continue
		}

		var out batchResultLine
		if err := json.Unmarshal([]byte(line), &out); err != nil {
			t.Errorf("error line %d: invalid JSON: %v", i+1, err)
			continue
		}
		if out.CustomID == "" {
			t.Errorf("error line %d: missing custom_id", i+1)
			continue
		}
		if errorCustomIDs[out.CustomID] {
			t.Errorf("error line %d: duplicate custom_id %q", i+1, out.CustomID)
		}
		errorCustomIDs[out.CustomID] = true

		if out.Error == nil {
			t.Errorf("error line %d (custom_id=%s): error is null", i+1, out.CustomID)
		} else {
			if out.Error.Code == "" {
				t.Errorf("error line %d (custom_id=%s): error code is empty", i+1, out.CustomID)
			}
			if out.Error.Message == "" {
				t.Errorf("error line %d (custom_id=%s): error message is empty", i+1, out.CustomID)
			}
		}
	}

	// --- Download input file and validate custom_id coverage ---
	client := newClient()
	resp, err := client.Files.Content(context.Background(), batch.InputFileID)
	if err != nil {
		t.Fatalf("download input file failed: %v", err)
	}
	inputBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	inputBody := strings.TrimSpace(string(inputBytes))

	var inputLines int
	if inputBody != "" {
		inputLines = len(strings.Split(inputBody, "\n"))
	}
	if int64(inputLines) != batch.RequestCounts.Total {
		t.Errorf("input lines (%d) != batch total (%d)", inputLines, batch.RequestCounts.Total)
	}
	for i, line := range strings.Split(inputBody, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var req struct {
			CustomID string `json:"custom_id"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Errorf("input line %d: invalid JSON: %v", i+1, err)
			continue
		}
		if req.CustomID != "" && !outputCustomIDs[req.CustomID] && !errorCustomIDs[req.CustomID] {
			t.Errorf("input custom_id %q not found in output or error file", req.CustomID)
		}
	}

	// --- Line count invariants ---
	total := result.OutputLines + result.ErrorLines
	if int64(total) != batch.RequestCounts.Total {
		t.Errorf("output lines (%d) + error lines (%d) = %d, but total requests = %d",
			result.OutputLines, result.ErrorLines, total, batch.RequestCounts.Total)
	}
	if result.OutputLines != int(batch.RequestCounts.Completed) {
		t.Errorf("output lines (%d) != completed count (%d)",
			result.OutputLines, batch.RequestCounts.Completed)
	}
	if result.ErrorLines != int(batch.RequestCounts.Failed) {
		t.Errorf("error lines (%d) != failed count (%d)",
			result.ErrorLines, batch.RequestCounts.Failed)
	}
}

// ── Generic helpers ──────────────────────────────────────────────────────

// assertSliceEqual verifies that want and got contain the same elements
// (order-independent, no duplicates allowed in got).
func assertSliceEqual(t *testing.T, want, got []string) {
	t.Helper()

	seen := make(map[string]bool, len(got))
	for _, v := range got {
		if seen[v] {
			t.Errorf("duplicate element: %s", v)
		}
		seen[v] = true
	}
	for _, v := range want {
		if !seen[v] {
			t.Errorf("missing element: %s", v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("length mismatch: got %d, want %d", len(got), len(want))
	}
}

func waitForReady(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp, err := http.Get(url + "/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("not ready after %v: %v (%s)", timeout, err, url)
			}
			t.Fatalf("not ready after %v (status %d) (%s)", timeout, resp.StatusCode, url)
		}
		time.Sleep(time.Second)
	}
}
