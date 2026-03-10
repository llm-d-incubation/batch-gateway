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
	"crypto/tls"
	"encoding/json"
	"errors"
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

var (
	apiserverURL    = getEnvOrDefault("TEST_APISERVER_URL", "https://localhost:8000")
	apiserverObsURL = getEnvOrDefault("TEST_APISERVER_OBS_URL", "http://localhost:8081")
	processorObsURL = getEnvOrDefault("TEST_PROCESSOR_OBS_URL", "http://localhost:9090")
	jaegerURL       = getEnvOrDefault("TEST_JAEGER_URL", "http://localhost:16686")
	tenantHeader    = getEnvOrDefault("TEST_TENANT_HEADER", "X-MaaS-Username")
	tenantID        = getEnvOrDefault("TEST_TENANT_ID", "default")
	namespace       = getEnvOrDefault("TEST_NAMESPACE", "default")
	helmRelease     = getEnvOrDefault("TEST_HELM_RELEASE", "batch-gateway")

	testRunID = fmt.Sprintf("%d", time.Now().UnixNano())

	// httpClient is used for direct HTTP calls; skips TLS verification for self-signed certs.
	httpClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // dev/test only
		},
		Timeout: 10 * time.Second,
	}

	// kubectlAvailable is set once at TestE2E startup; when false,
	// verifications that require kubectl (e.g. log grepping) are skipped.
	kubectlAvailable bool

	// testPassThroughHeaders lists the headers configured in the apiserver's
	// pass_through_headers setting (set via dev-deploy.sh).
	testPassThroughHeaders = map[string]string{
		"X-E2E-Pass-Through-1": "test-value-1",
		"X-E2E-Pass-Through-2": "test-value-2",
	}
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

// ── Helpers ────────────────────────────────────────────────────────────

func newClient() *openai.Client {
	return newClientForTenant(tenantID)
}

func newClientForTenant(tenant string) *openai.Client {
	c := openai.NewClient(
		option.WithBaseURL(apiserverURL+"/v1/"),
		option.WithAPIKey("unused"),
		option.WithHeader(tenantHeader, tenant),
		option.WithHTTPClient(httpClient),
	)
	return &c
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

func mustCreateBatch(t *testing.T, fileID string, opts ...option.RequestOption) string {
	t.Helper()

	batch, err := newClient().Batches.New(context.Background(),
		openai.BatchNewParams{
			InputFileID:      fileID,
			Endpoint:         openai.BatchNewParamsEndpointV1ChatCompletions,
			CompletionWindow: openai.BatchNewParamsCompletionWindow24h,
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
	return batch.ID
}

// waitForBatchCompletion polls a batch by ID until it reaches a terminal state
// and returns the final batch object. Fatals if the batch does not complete
// within 5 minutes or the test deadline (whichever comes first).
func waitForBatchCompletion(t *testing.T, batchID string) *openai.Batch {
	t.Helper()

	client := newClient()

	const (
		pollInterval = 5 * time.Second
		maxWait      = 5 * time.Minute
	)

	var finalBatch *openai.Batch
	deadline := time.Now().Add(maxWait)
	if d, ok := t.Deadline(); ok && d.Before(deadline) {
		deadline = d.Add(-5 * time.Second)
	}
	for time.Now().Before(deadline) {
		b, err := client.Batches.Get(context.Background(), batchID)
		if err != nil {
			t.Fatalf("retrieve batch failed: %v", err)
		}
		finalBatch = b

		t.Logf("batch %s status: %s (completed=%d, failed=%d)",
			batchID, b.Status,
			b.RequestCounts.Completed, b.RequestCounts.Failed)

		switch b.Status {
		case openai.BatchStatusCompleted, openai.BatchStatusFailed,
			openai.BatchStatusExpired, openai.BatchStatusCancelled:
			return finalBatch
		}
		time.Sleep(pollInterval)
	}

	t.Fatalf("batch %s did not reach a final state within %v (last status: %q)",
		batchID, maxWait, finalBatch.Status)
	return nil // unreachable
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
	if !kubectlAvailable {
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
		"-l", fmt.Sprintf("app.kubernetes.io/instance=%s,app.kubernetes.io/component=processor", helmRelease),
		"-n", namespace,
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

// ── Multi-tenant subtests ─────────────────────────────────────────────────────

// doTestMultiTenantIsolation verifies that resources created by tenant A
// are not visible to tenant B.
func doTestMultiTenantIsolation(t *testing.T) {
	t.Helper()

	ctx := context.Background()
	tenantA := fmt.Sprintf("tenant-a-%s", testRunID)
	tenantB := fmt.Sprintf("tenant-b-%s", testRunID)
	clientA := newClientForTenant(tenantA)
	clientB := newClientForTenant(tenantB)

	// ── Files isolation ──────────────────────────────────────────────────

	// Tenant A creates a file
	filenameA := fmt.Sprintf("tenant-a-file-%s.jsonl", testRunID)
	fileA, err := clientA.Files.New(ctx, openai.FileNewParams{
		File:    openai.File(strings.NewReader(testJSONL), filenameA, "application/jsonl"),
		Purpose: openai.FilePurposeBatch,
	})
	if err != nil {
		t.Fatalf("tenant A: create file failed: %v", err)
	}
	t.Logf("tenant A created file: %s", fileA.ID)

	// Tenant B should not see tenant A's file
	_, err = clientB.Files.Get(ctx, fileA.ID)
	if err == nil {
		t.Error("tenant B was able to retrieve tenant A's file; expected 404")
	} else {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 when tenant B accesses tenant A's file, got %d", apiErr.StatusCode)
		}
	}

	// Tenant B's file list should not contain tenant A's file
	pageB, err := clientB.Files.List(ctx, openai.FileListParams{})
	if err != nil {
		t.Fatalf("tenant B: list files failed: %v", err)
	}
	for _, f := range pageB.Data {
		if f.ID == fileA.ID {
			t.Error("tenant B's file list contains tenant A's file")
		}
	}

	// ── Batches isolation ────────────────────────────────────────────────

	// Tenant A creates a batch
	batchA, err := clientA.Batches.New(ctx, openai.BatchNewParams{
		InputFileID:      fileA.ID,
		Endpoint:         openai.BatchNewParamsEndpointV1ChatCompletions,
		CompletionWindow: openai.BatchNewParamsCompletionWindow24h,
	})
	if err != nil {
		t.Fatalf("tenant A: create batch failed: %v", err)
	}
	t.Logf("tenant A created batch: %s", batchA.ID)

	// Tenant B should not see tenant A's batch
	_, err = clientB.Batches.Get(ctx, batchA.ID)
	if err == nil {
		t.Error("tenant B was able to retrieve tenant A's batch; expected 404")
	} else {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 when tenant B accesses tenant A's batch, got %d", apiErr.StatusCode)
		}
	}

	// Tenant B's batch list should not contain tenant A's batch
	batchPageB, err := clientB.Batches.List(ctx, openai.BatchListParams{})
	if err != nil {
		t.Fatalf("tenant B: list batches failed: %v", err)
	}
	for _, b := range batchPageB.Data {
		if b.ID == batchA.ID {
			t.Error("tenant B's batch list contains tenant A's batch")
		}
	}

	// Tenant B should not be able to cancel tenant A's batch
	_, err = clientB.Batches.Cancel(ctx, batchA.ID)
	if err == nil {
		t.Error("tenant B was able to cancel tenant A's batch; expected 404")
	} else {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 when tenant B cancels tenant A's batch, got %d", apiErr.StatusCode)
		}
	}
}

// ── Observability subtests ────────────────────────────────────────────────────

// doTestOtelTraces verifies that traces are exported to Jaeger after a batch
// lifecycle. It queries the Jaeger query API for traces from the batch-gateway
// service.
func doTestOtelTraces(t *testing.T) {
	t.Helper()

	// Check Jaeger is reachable
	jaegerClient := &http.Client{Timeout: 5 * time.Second}
	checkResp, err := jaegerClient.Get(jaegerURL + "/")
	if err != nil {
		t.Skipf("Jaeger not reachable at %s, skipping OTel trace verification: %v", jaegerURL, err)
	}
	checkResp.Body.Close()

	// Run a quick batch to generate traces
	fileID := mustCreateFile(t, fmt.Sprintf("test-otel-%s.jsonl", testRunID), testJSONL)
	batchID := mustCreateBatch(t, fileID)
	finalBatch := waitForBatchCompletion(t, batchID)
	if finalBatch.Status != openai.BatchStatusCompleted {
		t.Fatalf("expected batch status %q, got %q", openai.BatchStatusCompleted, finalBatch.Status)
	}

	// Give Jaeger a moment to index the traces
	time.Sleep(3 * time.Second)

	// Query Jaeger for traces from the batch-gateway service
	jaegerQueryURL := fmt.Sprintf("%s/api/traces?service=batch-gateway&limit=1", jaegerURL)
	resp, err := jaegerClient.Get(jaegerQueryURL)
	if err != nil {
		t.Fatalf("failed to query Jaeger API: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Jaeger API returned status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode Jaeger response: %v", err)
	}

	if len(result.Data) == 0 {
		t.Fatal("expected at least 1 trace from Jaeger, got 0")
	}

	t.Logf("Jaeger returned %d trace(s) for service batch-gateway", len(result.Data))
}

// doTestObservabilityEndpoints verifies that the observability endpoints
// (/health, /ready, /metrics) are reachable over plain HTTP at the given base URL.
func doTestObservabilityEndpoints(t *testing.T, obsURL string) {
	t.Helper()

	for _, endpoint := range []string{"/health", "/ready", "/metrics"} {
		t.Run(endpoint, func(t *testing.T) {
			resp, err := http.Get(obsURL + endpoint)
			if err != nil {
				t.Fatalf("GET %s failed: %v", endpoint, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("expected 200 from %s, got %d", endpoint, resp.StatusCode)
			}
		})
	}
}

// ── Setup ────────────────────────────────────────────────────────────────

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

// ── Entry point ──────────────────────────────────────────────────────────

func TestE2E(t *testing.T) {
	if out, err := exec.Command("kubectl", "cluster-info").CombinedOutput(); err != nil {
		t.Logf("kubectl not available, some checks will be skipped: %v\n%s", err, out)
	} else {
		kubectlAvailable = true
	}

	waitForReady(t, apiserverObsURL, 30*time.Second)

	t.Run("Files", func(t *testing.T) {
		t.Run("Lifecycle", doTestFileLifecycle)
	})

	t.Run("Batches", func(t *testing.T) {
		t.Run("Lifecycle", doTestBatchLifecycle)
		t.Run("Cancel", doTestBatchCancel)
		t.Run("PassThroughHeaders", doTestPassThroughHeaders)
	})

	t.Run("MultiTenant", func(t *testing.T) {
		t.Run("Isolation", doTestMultiTenantIsolation)
	})

	t.Run("Observability", func(t *testing.T) {
		t.Run("APIServer", func(t *testing.T) { doTestObservabilityEndpoints(t, apiserverObsURL) })
		t.Run("Processor", func(t *testing.T) { doTestObservabilityEndpoints(t, processorObsURL) })
		t.Run("OtelTraces", doTestOtelTraces)
	})
}
