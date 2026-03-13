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
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

var (
	testApiserverURL      = getEnvOrDefault("TEST_APISERVER_URL", "https://localhost:8000")
	testApiserverObsURL   = getEnvOrDefault("TEST_APISERVER_OBS_URL", "http://localhost:8081")
	testProcessorObsURL   = getEnvOrDefault("TEST_PROCESSOR_OBS_URL", "http://localhost:9090")
	testJaegerURL         = getEnvOrDefault("TEST_JAEGER_URL", "http://localhost:16686")
	testTenantHeader      = getEnvOrDefault("TEST_TENANT_HEADER", "X-MaaS-Username")
	testTenantID          = getEnvOrDefault("TEST_TENANT_ID", "default")
	testNamespace         = getEnvOrDefault("TEST_NAMESPACE", "default")
	testHelmRelease       = getEnvOrDefault("TEST_HELM_RELEASE", "batch-gateway")
	testPostgresqlRelease = getEnvOrDefault("TEST_POSTGRESQL_RELEASE", "postgresql")

	testRunID = fmt.Sprintf("%d", time.Now().UnixNano())

	// testModel is the model name used in batch input; configurable via TEST_MODEL env var.
	testModel = getEnvOrDefault("TEST_MODEL", "sim-model")

	// testJSONL is a valid batch input file with two requests.
	// max_tokens is kept small to finish quickly with the simulator's 500ms inter-token-latency.
	testJSONL = strings.Join([]string{
		fmt.Sprintf(`{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello"}]}}`, testModel),
		fmt.Sprintf(`{"custom_id":"req-2","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"World"}]}}`, testModel),
	}, "\n")

	// testHTTPClient is used for direct HTTP calls; skips TLS verification for self-signed certs.
	testHTTPClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // dev/test only
		},
		Timeout: 10 * time.Second,
	}

	// testKubectlAvailable is set once at TestE2E startup; when false,
	// verifications that require kubectl (e.g. log grepping) are skipped.
	testKubectlAvailable bool

	// testPassThroughHeaders lists the headers configured in the apiserver's
	// pass_through_headers setting (set via dev-deploy.sh).
	testPassThroughHeaders = map[string]string{
		"X-E2E-Pass-Through-1": "test-value-1",
		"X-E2E-Pass-Through-2": "test-value-2",
	}

	// testBatchMetadata is attached to every batch created via mustCreateBatch
	// so that metadata round-tripping is verified as part of the lifecycle test.
	testBatchMetadata = shared.Metadata{
		"env":    "e2e-test",
		"run_id": testRunID,
	}
)

// ── Helpers ────────────────────────────────────────────────────────────

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

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
func waitForBatchStatus(t *testing.T, batchID string, timeout time.Duration, targets ...openai.BatchStatus) *openai.Batch {
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

		if targetSet[b.Status] {
			return b
		}
		if terminalBatchStatuses[b.Status] {
			t.Fatalf("batch %s reached terminal status %q, will never become %v",
				batchID, b.Status, targets)
		}
		time.Sleep(pollInterval)
	}

	t.Fatalf("batch %s did not reach status %v within %v (last status: %q)",
		batchID, targets, timeout, lastBatch.Status)
	return nil // unreachable
}

// waitForBatchCompletion polls a batch by ID until it reaches a terminal state.
func waitForBatchCompletion(t *testing.T, batchID string) *openai.Batch {
	t.Helper()
	return waitForBatchStatus(t, batchID, 5*time.Minute,
		openai.BatchStatusCompleted, openai.BatchStatusFailed,
		openai.BatchStatusExpired, openai.BatchStatusCancelled,
	)
}

func validateAndLogJSONL(t *testing.T, label string, content string) {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(content), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			t.Errorf("%s: line %d is not valid JSON: %q", label, i+1, line)
		}
		t.Logf("%s line %d: %s", label, i+1, line)
	}
}

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
		testKubectlAvailable = true
	}

	waitForReady(t, testApiserverObsURL, 30*time.Second)

	t.Run("Files", testFiles)
	t.Run("Batches", testBatches)
	t.Run("Concurrent", testConcurrent)
	t.Run("MultiTenant", testMultiTenant)
	t.Run("GarbageCollection", testGarbageCollection)
	t.Run("Observability", testObservability)
}
