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

// Flow control tests verify that the batch-gateway processor correctly sends
// inference headers and handles downstream 429 responses.
//
// Tests are split into two groups:
//
//   - HeaderAndRetry: run without GIE. They verify batch-gateway's
//     own responsibilities — sending the right headers and retrying on 429 —
//     against plain vLLM simulator instances.
//
//   - GIE: require a full GIE/EPP deployment (ENABLE_GIE=true).
//     They verify that requests route through EPP and that per-model
//     InferenceObjectives are respected.

package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

func testFlowControl(t *testing.T) {
	t.Run("HeaderAndRetry", func(t *testing.T) {
		if !testKubectlAvailable {
			t.Skip("kubectl not available")
		}
		t.Run("InferenceObjectiveHeader", doTestInferenceObjectiveHeader)
		t.Run("SLOHeader", doTestSLOHeader)
		t.Run("RetryOn429", doTestRetryOn429)
		t.Run("RetryExhaustion", doTestRetryExhaustion)
	})

	t.Run("GIE", func(t *testing.T) {
		if !testKubectlAvailable {
			t.Skip("kubectl not available")
		}
		if !detectGIEDeployed(t) {
			t.Skip("GIE EPP not deployed (deploy with ENABLE_GIE=true)")
		}
		t.Run("HeaderPropagation", doTestGIEHeaderPropagation)
		t.Run("BatchCompletionThroughEPP", doTestBatchCompletionThroughEPP)
		t.Run("SheddingUnderSaturation", doTestSheddingUnderSaturation)
		t.Run("PriorityBandInteraction", doTestPriorityBandInteraction)
		t.Run("MixedLoadWithMetrics", doTestMixedLoadWithMetrics)
	})
}

// ── Header propagation and 429 retry (no GIE required) ─────────────────

// doTestInferenceObjectiveHeader verifies that the processor ConfigMap has the
// expected inferenceObjective for testModel, and that a batch targeting this
// model completes successfully. It does not inspect the request header on the
// wire.
func doTestInferenceObjectiveHeader(t *testing.T) {
	t.Helper()

	expectedObjective := resolveExpectedObjective(t, testModel)
	configuredObjective := getProcessorConfigObjective(t, testModel)
	if configuredObjective != expectedObjective {
		t.Errorf("processor ConfigMap inferenceObjective for %q = %q, want %q",
			testModel, configuredObjective, expectedObjective)
	}

	fileID := mustCreateFile(t, fmt.Sprintf("test-fc-obj-header-%s.jsonl", testRunID), testJSONL)
	batchID := mustCreateBatch(t, fileID)

	batch, _ := waitForBatchStatus(t, batchID, 3*time.Minute, openai.BatchStatusCompleted)
	if batch.RequestCounts.Completed != 2 {
		t.Fatalf("expected 2 completed, got %d", batch.RequestCounts.Completed)
	}

	t.Logf("inferenceObjective=%q configured and batch completed", expectedObjective)
}

// doTestSLOHeader submits a batch with a short completion_window and verifies
// the batch completes before that window expires. It does not read
// x-slo-ttft-ms on the live request. Remaining-ms formatting from a stored
// deadline is covered by TestMergeHeaders in source_planfile_test.go.
func doTestSLOHeader(t *testing.T) {
	t.Helper()

	fileID := mustCreateFile(t, fmt.Sprintf("test-fc-slo-header-%s.jsonl", testRunID), testJSONL)

	client := newClient()
	batch, err := client.Batches.New(t.Context(), openai.BatchNewParams{
		InputFileID:      fileID,
		Endpoint:         openai.BatchNewParamsEndpointV1ChatCompletions,
		CompletionWindow: openai.BatchNewParamsCompletionWindow("10m"),
	})
	if err != nil {
		t.Fatalf("create batch failed: %v", err)
	}

	finalBatch, _ := waitForBatchStatus(t, batch.ID, 3*time.Minute, openai.BatchStatusCompleted)
	if finalBatch.Status != openai.BatchStatusCompleted {
		t.Fatalf("expected batch to complete within SLO window, got status %s", finalBatch.Status)
	}
	if finalBatch.RequestCounts.Completed != 2 {
		t.Fatalf("expected 2 completed, got %d", finalBatch.RequestCounts.Completed)
	}

	t.Logf("batch completed within 10m SLO window")
}

// doTestRetryOn429 verifies the full retry-to-success path:
//  1. Set sim-model-429 to 100% failure via /admin/config.
//  2. Submit a batch — all initial attempts receive 429.
//  3. Flip the simulator to 0% failure — subsequent retries succeed.
//  4. Assert all requests completed with no failures.
//  5. Assert AIMD decrease signals were recorded (proves 429s were observed).
//
// This exercises the end-to-end contract: transient 429 → retry → success.
// Requires llm-d-inference-sim >= v0.9.1 for the /admin/config endpoint.
func doTestRetryOn429(t *testing.T) {
	t.Helper()

	if !testKubectlAvailable {
		t.Skip("kubectl not available")
	}

	const numRequests = 4

	t.Cleanup(func() { deleteE2ECurlPod(t) })
	t.Cleanup(func() {
		t.Log("cleanup: restoring sim-model-429 to 50% failure rate")
		if err := trySetSimAdminConfig(t, testSimService429, `{"failure-injection-rate": 50, "failure-types": ["rate_limit"]}`); err != nil {
			t.Errorf("cleanup: failed to restore %s to 50%% failure rate: %v", testSimService429, err)
		}
	})

	// Snapshot cumulative counters before the test. These are process-wide
	// counters that persist across test runs on the same processor. We verify
	// deltas (not absolute values) after batch completion.
	metricsBefore := scrapeProcessorMetrics(t)
	decreasesBefore := parseCounterByEndpoint(t, metricsBefore, "batch_processor_aimd_decreases_total")
	var beforeCount float64
	for endpoint, count := range decreasesBefore {
		if strings.Contains(endpoint, testSimService429) {
			beforeCount = count
			break
		}
	}
	errorsBefore := getRequestErrors(t, testModel429)

	// Phase 1: Set 100% failure — all initial attempts will get 429.
	t.Log("phase 1: setting sim-model-429 to 100% failure rate")
	setSimAdminConfig(t, testSimService429, `{"failure-injection-rate": 100, "failure-types": ["rate_limit"]}`)

	// Phase 2: Submit batch.
	t.Log("phase 2: submitting batch")
	lines := make([]string, 0, numRequests)
	for i := range numRequests {
		lines = append(lines, fmt.Sprintf(
			`{"custom_id":"retry429-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Retry test %d"}]}}`,
			i+1, testModel429, i+1,
		))
	}

	fileID := mustCreateFile(t, fmt.Sprintf("test-retry429-%s.jsonl", testRunID), strings.Join(lines, "\n"))
	batchID := mustCreateBatch(t, fileID)

	_, _ = waitForBatchStatus(t, batchID, 2*time.Minute, openai.BatchStatusInProgress)

	// Phase 3: Wait until the processor has dispatched at least one request
	// to the sim. model_inflight_requests{model="sim-model-429"} > 0 proves
	// dispatch happened. Because failure rate is 100%, the first response is
	// a 429, and the resty client enters its retry loop (Generate blocks
	// through all retries). The gauge stays elevated for the entire retry
	// chain (minutes), making it a stable — not transient — signal here.
	t.Log("phase 3: waiting for model_inflight_requests > 0")
	waitForModelInflight(t, testModel429, 30*time.Second)

	// Phase 4: Flip to 0% failure — the next retry attempt will succeed.
	t.Log("phase 4: setting sim-model-429 to 0% failure rate")
	setSimAdminConfig(t, testSimService429, `{"failure-injection-rate": 0}`)

	// Phase 5: Wait for batch to complete.
	batch, _ := waitForBatchStatus(t, batchID, 3*time.Minute, openai.BatchStatusCompleted)

	if batch.RequestCounts.Completed != int64(numRequests) {
		t.Fatalf("expected %d completed, got %d (failed=%d)",
			numRequests, batch.RequestCounts.Completed, batch.RequestCounts.Failed)
	}
	if batch.RequestCounts.Failed != 0 {
		t.Errorf("expected 0 failed, got %d", batch.RequestCounts.Failed)
	}

	// Phase 6: Verify AIMD capacity_retry signals were recorded. Each request
	// hit at least one 429 before succeeding, so hadCapacityRetry=true triggers
	// an AIMD decrease on completion.
	metricsAfter := scrapeProcessorMetrics(t)
	decreasesAfter := parseCounterByEndpoint(t, metricsAfter, "batch_processor_aimd_decreases_total")
	var afterCount float64
	for endpoint, count := range decreasesAfter {
		if strings.Contains(endpoint, testSimService429) {
			afterCount = count
			break
		}
	}

	if afterCount <= beforeCount {
		t.Errorf("expected AIMD decreases to increase after retry-on-429 "+
			"(before=%.0f, after=%.0f); capacity_retry signals not recorded",
			beforeCount, afterCount)
	} else {
		t.Logf("retry-on-429: all %d requests completed after retry "+
			"(aimd_decreases: %.0f → %.0f)", numRequests, beforeCount, afterCount)
	}

	assertNoNewRequestErrors(t, testModel429, errorsBefore)
}

// doTestRetryExhaustion submits a batch targeting a simulator with 100%
// failure injection. maxRetries is set to 1 via Helm, so the processor
// exhausts retries quickly. The processor records 429 responses in the
// output file and marks the batch completed with RequestCounts.Failed > 0.
//
// This test polls manually instead of using waitForBatchStatus because
// validateBatchResults enforces status_code=200 on all output lines,
// which is not valid here — 429 responses in the output file are expected.
func doTestRetryExhaustion(t *testing.T) {
	t.Helper()

	jsonl := strings.Join([]string{
		fmt.Sprintf(`{"custom_id":"fail-1","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello 1"}]}}`, testModelAlwaysFail),
		fmt.Sprintf(`{"custom_id":"fail-2","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello 2"}]}}`, testModelAlwaysFail),
	}, "\n")

	fileID := mustCreateFile(t, fmt.Sprintf("test-retry-exhaust-%s.jsonl", testRunID), jsonl)
	batchID := mustCreateBatch(t, fileID)

	finalBatch := waitForRetryExhaustion(t, batchID, 3*time.Minute)

	// The processor records 429 responses as output and marks the batch
	// as completed (all requests were processed, even if none succeeded).
	if finalBatch.Status != openai.BatchStatusCompleted {
		t.Errorf("expected batch status %q (processor finished processing), got %q",
			openai.BatchStatusCompleted, finalBatch.Status)
	}
	if finalBatch.RequestCounts.Completed != 0 {
		t.Errorf("expected 0 successfully completed requests, got %d", finalBatch.RequestCounts.Completed)
	}
	if finalBatch.RequestCounts.Failed != 2 {
		t.Errorf("expected 2 failed requests, got %d", finalBatch.RequestCounts.Failed)
	}

	t.Logf("retry exhaustion: status=%s completed=%d failed=%d total=%d",
		finalBatch.Status, finalBatch.RequestCounts.Completed, finalBatch.RequestCounts.Failed, finalBatch.RequestCounts.Total)

	if finalBatch.OutputFileID == "" {
		t.Fatal("expected output file with 429 responses, but OutputFileID is empty")
	}
	result := fetchOutputFile(t, finalBatch)
	var found429 int
	for _, line := range strings.Split(result, "\n") {
		var rl batchResultLine
		if err := json.Unmarshal([]byte(line), &rl); err != nil {
			continue
		}
		if rl.Response != nil && rl.Response.StatusCode == http.StatusTooManyRequests {
			found429++
		}
	}
	if found429 == 0 {
		t.Errorf("expected at least one 429 response in output file, found none")
	}
	t.Logf("output file contains %d response(s) with status 429", found429)

	assertRequestErrors(t, testModelAlwaysFail)
}

// ──  GIE integration tests (require ENABLE_GIE=true) ───────────────────
//
// Coverage:
//   - EPP routing smoke tests (header propagation, multi-model completion)
//   - Shedding under saturation (DroppedOnSaturation via fake-metrics saturation)
//   - Priority band interaction (interactive dispatched, batch shed under saturation)
//   - Mixed load with metrics (saturation/recovery cycle with EPP + processor metrics)
//
// Not covered: while two sheddable requests are queued, EPP does not dispatch
// the earlier x-slo-ttft-ms deadline first. Observed on GIE EPP v1.5.0 (FCFS
// even with slo-deadline-ordering-policy). Unblock by deploying llm-d-router
// for Kind e2e, not by bumping GIE_VERSION (v1.6.0 dropped standalone EPP).
// When adding the test: saturate, curl long then short SLO on the same
// objective, unsaturate only after both "Item enqueued", assert short
// "Item dispatched" first. Do not use batch CompletedAt.

// detectGIEDeployed checks whether at least one EPP deployment exists.
// It searches testEPPNamespace (TEST_EPP_NAMESPACE) if set, otherwise
// testNamespace. Set TEST_GIE_DEPLOYED=true to force-enable GIE tests
// when the EPP naming convention differs (e.g. RHOAI).
func detectGIEDeployed(t *testing.T) bool {
	t.Helper()

	if strings.EqualFold(getEnvOrDefault("TEST_GIE_DEPLOYED", ""), "true") {
		return true
	}

	ns := testEPPNamespace
	if ns == "" {
		ns = testNamespace
	}

	out, err := exec.Command("kubectl", "get", "deployments",
		"-n", ns,
		"-o", "name",
	).CombinedOutput()
	if err != nil {
		t.Logf("kubectl get deployments failed: %v", err)
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "epp-") {
			return true
		}
	}
	return false
}

// doTestGIEHeaderPropagation verifies that the processor sends requests through
// EPP with x-gateway-inference-objective configured and that the requests
// pass through the flow control dispatch path.
//
// Verification: submit a batch, wait for completion, then check EPP logs for
// evidence that it received, routed, and dispatched the requests via flow control.
func doTestGIEHeaderPropagation(t *testing.T) {
	t.Helper()

	eppDeployment := fmt.Sprintf("%s-%s-epp", getEnvOrDefault("GIE_EPP_RELEASE", "epp"), testModel)
	sinceTime := time.Now().UTC().Format(time.RFC3339Nano)

	fileID := mustCreateFile(t, fmt.Sprintf("flow-control-headers-%s.jsonl", testRunID), testJSONL)
	batchID := mustCreateBatch(t, fileID)

	batch, _ := waitForBatchStatus(t, batchID, 60*time.Second, openai.BatchStatusCompleted)

	if batch.RequestCounts.Completed != 2 {
		t.Fatalf("expected 2 completed requests, got %d", batch.RequestCounts.Completed)
	}
	if batch.RequestCounts.Failed != 0 {
		t.Fatalf("expected 0 failed requests, got %d", batch.RequestCounts.Failed)
	}

	eppLogs := getEPPLogsSince(t, eppDeployment, sinceTime)

	received := strings.Count(eppLogs, "EPP received request")
	if received < 2 {
		t.Errorf("expected EPP to receive >= 2 requests since %s, got %d;\nlog sample:\n%s",
			sinceTime, received, truncateLog(eppLogs, 1000))
	}

	routed := strings.Count(eppLogs, "EPP sent request body response(s) to proxy")
	if routed < 2 {
		t.Errorf("expected EPP to route >= 2 responses since %s, got %d", sinceTime, routed)
	}

	dispatched := strings.Count(eppLogs, "Item dispatched.")
	if dispatched < 2 {
		t.Errorf("expected flow control to dispatch >= 2 items since %s, got %d;\nlog sample:\n%s",
			sinceTime, dispatched, truncateLog(eppLogs, 1000))
	}
}

// doTestBatchCompletionThroughEPP verifies multi-model batch completion through
// separate EPP instances by checking each EPP's dispatched-request metric.
// We prefer metrics here because log sampling via --since-time proved flaky.
func doTestBatchCompletionThroughEPP(t *testing.T) {
	t.Helper()

	t.Cleanup(func() {
		deleteE2ECurlPod(t)
	})

	eppPrefix := getEnvOrDefault("GIE_EPP_RELEASE", "epp")
	eppDeployments := []string{
		fmt.Sprintf("%s-%s-epp", eppPrefix, testModel),
		fmt.Sprintf("%s-%s-epp", eppPrefix, testModelB),
	}
	beforeCounts := make(map[string]float64, len(eppDeployments))
	for _, deployment := range eppDeployments {
		beforeCounts[deployment] = getEPPDispatchedCount(t, deployment)
	}

	multiModelJSONL := strings.Join([]string{
		fmt.Sprintf(`{"custom_id":"fc-req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello from model A"}]}}`, testModel),
		fmt.Sprintf(`{"custom_id":"fc-req-2","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello from model B"}]}}`, testModelB),
	}, "\n")

	fileID := mustCreateFile(t, fmt.Sprintf("flow-control-epp-%s.jsonl", testRunID), multiModelJSONL)
	batchID := mustCreateBatch(t, fileID)

	batch, result := waitForBatchStatus(t, batchID, 60*time.Second, openai.BatchStatusCompleted)

	if batch.RequestCounts.Completed != 2 {
		t.Errorf("expected 2 completed, got %d", batch.RequestCounts.Completed)
	}
	if batch.RequestCounts.Failed != 0 {
		t.Errorf("expected 0 failed, got %d", batch.RequestCounts.Failed)
	}

	validateTerminalBatch(t, batch)
	validateBatchResults(t, batch, *result)

	for _, deployment := range eppDeployments {
		assertEPPDispatchedDelta(t, deployment, beforeCounts[deployment], 1, 15*time.Second)
	}
}

// ── GIE flow control scenario tests ─────────────────────────────────────────

// doTestSheddingUnderSaturation verifies that batch requests (priority -1) fail
// when the inference pool is saturated. The test saturates the pool by injecting
// high waiting-requests via /fake_metrics, then submits a batch. Under saturation,
// requests queue until defaultRequestTTL (30s) expires and are evicted (EvictedTTL).
// After unsaturation, a new batch completes successfully.
func doTestSheddingUnderSaturation(t *testing.T) {
	t.Helper()

	t.Cleanup(func() {
		deleteE2ECurlPod(t)
	})

	eppDeployment := fmt.Sprintf("%s-%s-epp", getEnvOrDefault("GIE_EPP_RELEASE", "epp"), testModel)

	// Record baseline metrics before saturation.
	beforeNonDispatch := getEPPNonDispatchCount(t, eppDeployment)
	beforeErrors := getRequestErrors(t, testModel)

	// Saturate: set waiting-requests above queueDepthThreshold (5).
	t.Log("saturating pool: setting waiting-requests=10 on sim")
	setSimFakeMetrics(t, testSimService, `{"waiting-requests": 10}`)
	t.Cleanup(func() {
		// Best-effort restore; inline reset later is the primary path.
		_ = trySetSimFakeMetrics(t, testSimService, `{"waiting-requests": 0}`)
	})

	// Wait for EPP to detect saturation.
	waitForEPPSaturation(t, eppDeployment, true, 30*time.Second)

	// Submit a batch — processor sends x-gateway-inference-objective: batch-sheddable-sim-model
	// (priority -1). EPP should shed these requests.
	fileID := mustCreateFile(t, fmt.Sprintf("fc-shedding-%s.jsonl", testRunID), testJSONL)
	batchID := mustCreateBatch(t, fileID)

	// Wait for the batch to reach a terminal state. Under full saturation
	// the processor retries up to maxRetries then the batch fails.
	batch := waitForRetryExhaustion(t, batchID, 5*time.Minute)
	if batch.RequestCounts.Failed == 0 {
		t.Errorf("expected at least 1 failed request under saturation, got 0 failed (completed=%d)",
			batch.RequestCounts.Completed)
	}

	// Assert EPP non-dispatch outcomes increased (EvictedTTL or DroppedOnSaturation).
	afterNonDispatch := getEPPNonDispatchCount(t, eppDeployment)
	if afterNonDispatch <= beforeNonDispatch {
		t.Errorf("EPP non-dispatch outcomes did not increase: before=%.0f after=%.0f", beforeNonDispatch, afterNonDispatch)
	} else {
		t.Logf("EPP non-dispatch outcomes: %.0f -> %.0f (delta=%.0f)", beforeNonDispatch, afterNonDispatch, afterNonDispatch-beforeNonDispatch)
	}

	// Assert processor recorded request errors.
	afterErrors := getRequestErrors(t, testModel)
	if afterErrors <= beforeErrors {
		t.Errorf("request_errors_by_model_total{model=%q} did not increase: before=%d after=%d",
			testModel, beforeErrors, afterErrors)
	}

	// Unsaturate and verify recovery.
	t.Log("unsaturating pool: setting waiting-requests=0")
	setSimFakeMetrics(t, testSimService, `{"waiting-requests": 0}`)
	waitForEPPSaturation(t, eppDeployment, false, 30*time.Second)

	// Submit a new batch — should succeed now.
	fileID2 := mustCreateFile(t, fmt.Sprintf("fc-shedding-recovery-%s.jsonl", testRunID), testJSONL)
	batchID2 := mustCreateBatch(t, fileID2)
	batch2, _ := waitForBatchStatus(t, batchID2, 2*time.Minute, openai.BatchStatusCompleted)
	if batch2.RequestCounts.Completed != 2 {
		t.Errorf("expected 2 completed after unsaturation, got %d", batch2.RequestCounts.Completed)
	}
}

// doTestPriorityBandInteraction verifies priority-band routing through EPP:
// 1. Interactive (priority 100) dispatches under normal conditions.
// 2. Under saturation, both priority bands are blocked (pool-level gate).
// 3. Batch (priority -1) fails under sustained saturation.
// 4. After recovery, both bands dispatch again.
func doTestPriorityBandInteraction(t *testing.T) {
	t.Helper()

	t.Cleanup(func() {
		deleteE2ECurlPod(t)
	})

	eppDeployment := fmt.Sprintf("%s-%s-epp", getEnvOrDefault("GIE_EPP_RELEASE", "epp"), testModel)
	interactiveObjective := fmt.Sprintf("interactive-default-%s", testModel)
	eppURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:8081/v1/chat/completions", eppDeployment, testNamespace)
	interactiveBody := fmt.Sprintf(`{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"priority test"}]}`, testModel)

	ensureE2ECurlPod(t)

	// ── Phase 1: interactive dispatches under normal conditions ──
	t.Log("phase 1: verifying interactive dispatch under normal conditions")
	beforeDispatched := getEPPOutcomeCount(t, eppDeployment, "Dispatched")

	code := curlEPP(t, eppURL, interactiveObjective, interactiveBody)
	if code != "200" {
		t.Fatalf("interactive request (priority 100) returned %s under normal conditions, expected 200", code)
	}

	afterDispatched := getEPPOutcomeCount(t, eppDeployment, "Dispatched")
	if afterDispatched <= beforeDispatched {
		t.Errorf("EPP Dispatched did not increase for interactive: before=%.0f after=%.0f",
			beforeDispatched, afterDispatched)
	}
	t.Logf("interactive dispatched OK: Dispatched %.0f -> %.0f", beforeDispatched, afterDispatched)

	// ── Phase 2: saturate — verify interactive is ALSO blocked ──
	t.Log("phase 2: saturating pool, verifying interactive is blocked")
	setSimFakeMetrics(t, testSimService, `{"waiting-requests": 10}`)
	t.Cleanup(func() {
		// Best-effort restore; inline reset in phase 4 is the primary path.
		_ = trySetSimFakeMetrics(t, testSimService, `{"waiting-requests": 0}`)
	})

	waitForEPPSaturation(t, eppDeployment, true, 30*time.Second)

	code = curlEPP(t, eppURL, interactiveObjective, interactiveBody)
	switch {
	case code == "000":
		t.Fatalf("interactive request during saturation failed at transport level (curl error); cannot verify pool-level blocking")
	case code == "200":
		t.Errorf("interactive request (priority 100) returned 200 DURING saturation; expected non-200 (pool-level block)")
	default:
		t.Logf("interactive blocked under saturation as expected (HTTP %s)", code)
	}

	// ── Phase 3: batch (priority -1) also fails under saturation ──
	t.Log("phase 3: verifying batch fails under saturation")
	beforeNonDispatch := getEPPNonDispatchCount(t, eppDeployment)

	fileID := mustCreateFile(t, fmt.Sprintf("fc-priority-band-%s.jsonl", testRunID), testJSONL)
	batchID := mustCreateBatch(t, fileID)

	batch := waitForRetryExhaustion(t, batchID, 5*time.Minute)
	if batch.RequestCounts.Failed == 0 {
		t.Errorf("expected batch (priority -1) to fail under saturation, got 0 failed")
	}

	afterNonDispatch := getEPPNonDispatchCount(t, eppDeployment)
	if afterNonDispatch <= beforeNonDispatch {
		t.Errorf("EPP non-dispatch outcomes did not increase: before=%.0f after=%.0f",
			beforeNonDispatch, afterNonDispatch)
	}
	t.Logf("batch shed under saturation: non-dispatch %.0f -> %.0f", beforeNonDispatch, afterNonDispatch)

	// ── Phase 4: unsaturate — both bands recover ──
	t.Log("phase 4: unsaturating pool, verifying recovery")
	setSimFakeMetrics(t, testSimService, `{"waiting-requests": 0}`)
	waitForEPPSaturation(t, eppDeployment, false, 30*time.Second)

	code = curlEPP(t, eppURL, interactiveObjective, interactiveBody)
	if code != "200" {
		t.Errorf("interactive request after recovery returned %s, expected 200", code)
	}

	fileID2 := mustCreateFile(t, fmt.Sprintf("fc-priority-recovery-%s.jsonl", testRunID), testJSONL)
	batchID2 := mustCreateBatch(t, fileID2)
	batch2, _ := waitForBatchStatus(t, batchID2, 2*time.Minute, openai.BatchStatusCompleted)
	if batch2.RequestCounts.Completed != 2 {
		t.Errorf("expected 2 completed after recovery, got %d", batch2.RequestCounts.Completed)
	}

	t.Log("priority bands verified: both blocked under saturation, both recover after")
}

// doTestMixedLoadWithMetrics exercises a full saturation/recovery cycle and
// asserts both EPP-side and processor-side metrics are consistent.
// Phase 1: saturate pool, submit batch, observe shedding metrics.
// Phase 2: unsaturate, submit batch, observe successful dispatch metrics.
func doTestMixedLoadWithMetrics(t *testing.T) {
	t.Helper()

	t.Cleanup(func() {
		deleteE2ECurlPod(t)
	})

	eppDeployment := fmt.Sprintf("%s-%s-epp", getEnvOrDefault("GIE_EPP_RELEASE", "epp"), testModel)

	// ── Phase 1: Saturated ──
	t.Log("phase 1: saturating pool")
	setSimFakeMetrics(t, testSimService, `{"waiting-requests": 10}`)
	t.Cleanup(func() {
		// Best-effort restore; inline reset in phase 2 is the primary path.
		_ = trySetSimFakeMetrics(t, testSimService, `{"waiting-requests": 0}`)
	})
	waitForEPPSaturation(t, eppDeployment, true, 30*time.Second)

	beforeNonDispatch := getEPPNonDispatchCount(t, eppDeployment)
	beforeErrors := getRequestErrors(t, testModel)

	// Submit batch under saturation — expect non-dispatch outcome.
	fileID1 := mustCreateFile(t, fmt.Sprintf("fc-mixed-phase1-%s.jsonl", testRunID), testJSONL)
	batchID1 := mustCreateBatch(t, fileID1)
	batch1 := waitForRetryExhaustion(t, batchID1, 5*time.Minute)
	if batch1.RequestCounts.Failed == 0 {
		t.Errorf("phase 1: expected failures under saturation, got 0")
	}

	// ── Phase 2: Unsaturated ──
	t.Log("phase 2: unsaturating pool")
	setSimFakeMetrics(t, testSimService, `{"waiting-requests": 0}`)
	waitForEPPSaturation(t, eppDeployment, false, 30*time.Second)

	beforeDispatched := getEPPOutcomeCount(t, eppDeployment, "Dispatched")

	// Submit batch after recovery — expect success.
	fileID2 := mustCreateFile(t, fmt.Sprintf("fc-mixed-phase2-%s.jsonl", testRunID), testJSONL)
	batchID2 := mustCreateBatch(t, fileID2)
	batch2, _ := waitForBatchStatus(t, batchID2, 2*time.Minute, openai.BatchStatusCompleted)
	if batch2.RequestCounts.Completed != 2 {
		t.Errorf("phase 2: expected 2 completed, got %d", batch2.RequestCounts.Completed)
	}

	// ── Assertions: both EPP and processor metrics ──
	afterNonDispatch := getEPPNonDispatchCount(t, eppDeployment)
	afterDispatched := getEPPOutcomeCount(t, eppDeployment, "Dispatched")
	afterErrors := getRequestErrors(t, testModel)

	if afterNonDispatch <= beforeNonDispatch {
		t.Errorf("EPP non-dispatch outcomes did not increase in phase 1: before=%.0f after=%.0f",
			beforeNonDispatch, afterNonDispatch)
	}
	if afterDispatched <= beforeDispatched {
		t.Errorf("EPP Dispatched did not increase in phase 2: before=%.0f after=%.0f",
			beforeDispatched, afterDispatched)
	}
	if afterErrors <= beforeErrors {
		t.Errorf("processor request_errors did not increase during saturation: before=%d after=%d",
			beforeErrors, afterErrors)
	}

	t.Logf("mixed load metrics: non-dispatch %.0f->%.0f, Dispatched %.0f->%.0f, errors %d->%d",
		beforeNonDispatch, afterNonDispatch, beforeDispatched, afterDispatched, beforeErrors, afterErrors)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// curlEPP sends a chat completion request to EPP with the given objective header
// and returns the HTTP status code string. Does not fail the test on non-200.
// Timeout is 65s (EPP defaultRequestTTL 30s plus buffer) so saturation-path
// EvictedTTL responses are not cut off as transport errors.
func curlEPP(t *testing.T, url, objective, body string) string {
	t.Helper()

	ensureE2ECurlPod(t)
	out, err := exec.Command("kubectl", "exec",
		"-n", testNamespace,
		e2eCurlPod,
		"--",
		"curl", "-sS", "-X", "POST",
		"-H", "Content-Type: application/json",
		"-H", fmt.Sprintf("x-gateway-inference-objective: %s", objective),
		"-d", body,
		"-w", "\n%{http_code}",
		"--max-time", "65",
		url,
	).CombinedOutput()
	if err != nil {
		t.Logf("curl to %s failed: %v\n%s", url, err, out)
		return "000"
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return lines[len(lines)-1]
}

// getEPPLogsSince fetches EPP container logs from the given deployment,
// filtered to entries after sinceTime (RFC3339).
func getEPPLogsSince(t *testing.T, deployment, sinceTime string) string {
	t.Helper()

	out, err := exec.Command("kubectl", "logs",
		fmt.Sprintf("deployment/%s", deployment),
		"-n", testNamespace,
		"-c", "epp",
		fmt.Sprintf("--since-time=%s", sinceTime),
	).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl logs for %s failed: %v\n%s", deployment, err, out)
	}
	return string(out)
}

func truncateLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

var eppDispatchedCountPattern = regexp.MustCompile(`(?m)^inference_extension_flow_control_request_queue_duration_seconds_count\{([^}]*)\}\s+([0-9.e+-]+)$`)

func assertEPPDispatchedDelta(
	t *testing.T,
	deployment string,
	before float64,
	minDelta float64,
	timeout time.Duration,
) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastCount float64
	var lastSample string
	for time.Now().Before(deadline) {
		lastCount, lastSample = getEPPDispatchedCountAndSample(t, deployment)
		if lastCount-before >= minDelta {
			t.Logf("EPP %s dispatched count advanced from %.0f to %.0f", deployment, before, lastCount)
			return
		}
		time.Sleep(1 * time.Second)
	}

	t.Errorf("EPP %s did not dispatch >= %.0f request(s); before=%.0f after=%.0f\nmetric sample:\n%s",
		deployment, minDelta, before, lastCount, lastSample)
}

func getEPPDispatchedCount(t *testing.T, deployment string) float64 {
	t.Helper()

	count, _ := getEPPDispatchedCountAndSample(t, deployment)
	return count
}

func getEPPDispatchedCountAndSample(t *testing.T, deployment string) (float64, string) {
	t.Helper()

	metrics := scrapeEPPMetrics(t, deployment)
	matches := eppDispatchedCountPattern.FindAllStringSubmatch(metrics, -1)
	if len(matches) == 0 {
		return 0, truncateLog(metrics, 1000)
	}

	var total float64
	lines := make([]string, 0, len(matches))
	for _, match := range matches {
		labels := match[1]
		if !strings.Contains(labels, `outcome="Dispatched"`) {
			continue
		}
		value, err := strconv.ParseFloat(match[2], 64)
		if err != nil {
			t.Fatalf("failed to parse dispatched count for %s: %v", deployment, err)
		}
		total += value
		lines = append(lines,
			fmt.Sprintf("inference_extension_flow_control_request_queue_duration_seconds_count{%s} %s", labels, match[2]))
	}
	if len(lines) == 0 {
		return 0, truncateLog(metrics, 1000)
	}
	return total, strings.Join(lines, "\n")
}

func scrapeEPPMetrics(t *testing.T, deployment string) string {
	t.Helper()

	ensureE2ECurlPod(t)

	out, err := exec.Command("kubectl", "exec",
		"-n", testNamespace,
		e2eCurlPod,
		"--",
		"curl",
		"-sS",
		fmt.Sprintf("http://%s.%s.svc.cluster.local:9090/metrics", deployment, testNamespace),
	).CombinedOutput()
	if err != nil {
		t.Fatalf("failed to scrape metrics for %s: %v\n%s", deployment, err, out)
	}
	return string(out)
}

// ── EPP saturation helpers ──────────────────────────────────────────────

var eppOutcomeCountPattern = regexp.MustCompile(
	`(?m)^inference_extension_flow_control_request_queue_duration_seconds_count\{([^}]*)\}\s+([0-9.e+-]+)$`)

var eppPoolSaturationPattern = regexp.MustCompile(
	`(?m)^inference_extension_flow_control_pool_saturation\b[^\n]*\s+([0-9.e+-]+)$`)

// getEPPOutcomeCount parses EPP metrics and returns the total count for the
// given outcome label (e.g. "Dispatched", "EvictedTTL", "DroppedOnSaturation").
func getEPPOutcomeCount(t *testing.T, deployment, outcome string) float64 {
	t.Helper()

	metrics := scrapeEPPMetrics(t, deployment)
	matches := eppOutcomeCountPattern.FindAllStringSubmatch(metrics, -1)

	var total float64
	needle := fmt.Sprintf(`outcome="%s"`, outcome)
	for _, match := range matches {
		if !strings.Contains(match[1], needle) {
			continue
		}
		value, err := strconv.ParseFloat(match[2], 64)
		if err != nil {
			t.Fatalf("failed to parse outcome count for %s/%s: %v", deployment, outcome, err)
		}
		total += value
	}
	return total
}

// getEPPNonDispatchCount returns the combined count of all non-dispatch outcomes
// (EvictedTTL + DroppedOnSaturation). Under saturation, EPP may evict requests
// via TTL expiry or immediate drop depending on queue state and timing.
func getEPPNonDispatchCount(t *testing.T, deployment string) float64 {
	t.Helper()

	return getEPPOutcomeCount(t, deployment, "EvictedTTL") +
		getEPPOutcomeCount(t, deployment, "DroppedOnSaturation")
}

// waitForEPPSaturation polls the EPP flow_control_pool_saturation gauge until
// it reaches the expected state. Saturated means >= 1.0 (the value scales with
// utilization ratio, e.g. queue_depth/threshold, so it can exceed 1.0).
func waitForEPPSaturation(t *testing.T, deployment string, saturated bool, timeout time.Duration) {
	t.Helper()

	desc := "saturated"
	if !saturated {
		desc = "unsaturated"
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		metrics := scrapeEPPMetrics(t, deployment)
		match := eppPoolSaturationPattern.FindStringSubmatch(metrics)
		if match != nil {
			val, err := strconv.ParseFloat(match[1], 64)
			if err == nil {
				if saturated && val >= 1.0 {
					t.Logf("EPP %s pool_saturation = %g (%s)", deployment, val, desc)
					return
				}
				if !saturated && val < 1.0 {
					t.Logf("EPP %s pool_saturation = %g (%s)", deployment, val, desc)
					return
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("timed out waiting for EPP %s to become %s", deployment, desc)
}

// getProcessorConfigObjective reads the deployed processor ConfigMap and
// returns the inferenceObjective value for the given model.
func getProcessorConfigObjective(t *testing.T, model string) string {
	t.Helper()

	cmName := fmt.Sprintf("%s-processor-config", testHelmRelease)
	cm := kubectlGetConfigMap(t, cmName)

	pattern := regexp.MustCompile(fmt.Sprintf(`(?m)"?%s"?:\s*\n(?:.*\n)*?\s+inference_objective:\s*"?([^"\s]+)"?`, regexp.QuoteMeta(model)))
	match := pattern.FindStringSubmatch(cm)
	if match == nil {
		t.Logf("inferenceObjective not found for model %q in ConfigMap (may use global default)", model)
		return ""
	}
	return strings.TrimSpace(match[1])
}

// resolveExpectedObjective returns the inference objective value that the
// processor should set for the given model. In GIE mode the objective is
// per-model ("<prefix>-<model>"), otherwise it is the prefix alone.
// TEST_INFERENCE_OBJECTIVE overrides auto-detection.
func resolveExpectedObjective(t *testing.T, model string) string {
	t.Helper()

	if v := getEnvOrDefault("TEST_INFERENCE_OBJECTIVE", ""); v != "" {
		return v
	}
	prefix := getEnvOrDefault("GIE_OBJECTIVE_PREFIX", "batch-sheddable")
	if detectGIEDeployed(t) {
		return prefix + "-" + model
	}
	return prefix
}

// scrapeProcessorMetrics fetches the raw Prometheus text from the processor
// observability endpoint.
func scrapeProcessorMetrics(t *testing.T) string {
	t.Helper()

	_, body, err := readProcessorObsEndpoint(t, "/metrics")
	if err != nil {
		t.Fatalf("failed to scrape processor metrics: %v", err)
	}
	return string(body)
}

// waitForRetryExhaustion polls a batch until it reaches a terminal state.
// Unlike waitForBatchStatus, it skips validateBatchResults because retry
// exhaustion produces 429 responses in the output file which fail the
// standard status_code=200 check.
func waitForRetryExhaustion(t *testing.T, batchID string, timeout time.Duration) *openai.Batch {
	t.Helper()

	client := newClient()
	deadline := time.Now().Add(timeout)
	if d, ok := t.Deadline(); ok && d.Before(deadline) {
		deadline = d.Add(-5 * time.Second)
	}

	for time.Now().Before(deadline) {
		b, err := client.Batches.Get(t.Context(), batchID)
		if err != nil {
			t.Fatalf("retrieve batch failed: %v", err)
		}
		t.Logf("batch %s status: %s (completed=%d, failed=%d)",
			batchID, b.Status, b.RequestCounts.Completed, b.RequestCounts.Failed)

		if terminalBatchStatuses[b.Status] {
			return b
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("batch %s did not reach terminal status within %v", batchID, timeout)
	return nil
}

// getRequestErrors returns the current value of request_errors_by_model_total
// for the given model, or 0 if the metric is not present yet.
func getRequestErrors(t *testing.T, model string) int {
	t.Helper()

	metrics := scrapeProcessorMetrics(t)
	pattern := regexp.MustCompile(fmt.Sprintf(`request_errors_by_model_total\{model=%q\}\s+(\d+)`, model))
	match := pattern.FindStringSubmatch(metrics)
	if match == nil {
		return 0
	}
	val, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("failed to parse request_errors_by_model_total value %q: %v", match[1], err)
	}
	return val
}

// assertNoNewRequestErrors verifies that request_errors_by_model_total for the
// given model did not increase relative to the provided baseline. This is safe
// to use against long-lived processors where previous test runs may have
// already incremented the counter.
func assertNoNewRequestErrors(t *testing.T, model string, baseline int) {
	t.Helper()

	current := getRequestErrors(t, model)
	if current > baseline {
		t.Errorf("request_errors_by_model_total{model=%q} increased during test "+
			"(before=%d, after=%d); retries should have succeeded transparently",
			model, baseline, current)
	}
}

// assertRequestErrors verifies that request_errors_by_model_total for the
// given model is present and > 0. Used after retry exhaustion to confirm
// that the processor recorded the failures.
func assertRequestErrors(t *testing.T, model string) {
	t.Helper()

	metrics := scrapeProcessorMetrics(t)

	pattern := regexp.MustCompile(fmt.Sprintf(`request_errors_by_model_total\{model=%q\}\s+(\d+)`, model))
	match := pattern.FindStringSubmatch(metrics)
	if match == nil {
		t.Errorf("request_errors_by_model_total{model=%q} not found in metrics, expected > 0", model)
		return
	}
	if match[1] == "0" {
		t.Errorf("expected request_errors_by_model_total{model=%q} > 0, got 0", model)
	}
	t.Logf("request_errors_by_model_total{model=%q} = %s", model, match[1])
}

// getProcessorLogsSince fetches batch-gateway-processor container logs
// filtered to entries after sinceTime (RFC3339Nano).
func getProcessorLogsSince(t *testing.T, sinceTime string) string {
	t.Helper()

	deployment := fmt.Sprintf("%s-processor", testHelmRelease)
	out, err := exec.Command("kubectl", "logs",
		fmt.Sprintf("deployment/%s", deployment),
		"-n", testNamespace,
		fmt.Sprintf("--since-time=%s", sinceTime),
	).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl logs for %s failed: %v\n%s", deployment, err, out)
	}
	return string(out)
}
