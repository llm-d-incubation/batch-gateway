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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

func testObservability(t *testing.T) {
	t.Run("APIServer", func(t *testing.T) { doTestObservabilityEndpoints(t, testApiserverObsURL) })
	t.Run("Processor", func(t *testing.T) { doTestObservabilityEndpoints(t, testProcessorObsURL) })
	t.Run("OtelTraces", doTestOtelTraces)
}

// doTestOtelTraces verifies that traces are exported to Jaeger after a batch
// lifecycle. It queries the Jaeger query API for traces from the batch-gateway
// service.
func doTestOtelTraces(t *testing.T) {
	t.Helper()

	// Check Jaeger is reachable
	jaegerClient := &http.Client{Timeout: 5 * time.Second}
	checkResp, err := jaegerClient.Get(testJaegerURL + "/")
	if err != nil {
		t.Skipf("Jaeger not reachable at %s, skipping OTel trace verification: %v", testJaegerURL, err)
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
	jaegerQueryURL := fmt.Sprintf("%s/api/traces?service=batch-gateway&limit=1", testJaegerURL)
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
		t.Run(strings.TrimPrefix(endpoint, "/"), func(t *testing.T) {
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
