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
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
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
	})
}

// ── Header propagation and 429 retry (no GIE required) ─────────────────

// doTestInferenceObjectiveHeader verifies that the processor is configured with
// the expected inferenceObjective for testModel, and that a batch targeting this
// model completes successfully. The header itself (x-gateway-inference-objective)
// is set by mergeInferenceHeaders, which is unit-tested in executor_test.go.
//
// TODO: when llm-d-inference-sim releases --log-http support (post-v0.8.3),
// add --log-http to the simulator args in dev-deploy.sh, then assert the header
// value directly from simulator logs via getSimulatorLogsSince.
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

	t.Logf("inferenceObjective=%q configured and batch completed (header propagation unit-tested)", expectedObjective)
}

// doTestSLOHeader submits a batch with a short completion_window and verifies
// the batch completes before the SLO deadline. The x-slo-ttft-ms header is set
// by mergeInferenceHeaders based on the remaining SLO budget, which is
// unit-tested in executor_test.go.
//
// TODO: when llm-d-inference-sim releases --log-http support (post-v0.8.3),
// assert x-slo-ttft-ms header value directly from simulator logs.
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

	t.Logf("batch completed within 10m SLO window (x-slo-ttft-ms header propagation unit-tested)")
}

// doTestRetryOn429 submits a batch targeting a simulator with 30% failure
// injection (rate_limit → HTTP 429). The processor should retry failed
// requests and eventually complete the batch.
func doTestRetryOn429(t *testing.T) {
	t.Helper()

	jsonl := strings.Join([]string{
		fmt.Sprintf(`{"custom_id":"r429-1","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello 1"}]}}`, testModel429),
		fmt.Sprintf(`{"custom_id":"r429-2","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello 2"}]}}`, testModel429),
		fmt.Sprintf(`{"custom_id":"r429-3","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello 3"}]}}`, testModel429),
		fmt.Sprintf(`{"custom_id":"r429-4","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello 4"}]}}`, testModel429),
		fmt.Sprintf(`{"custom_id":"r429-5","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello 5"}]}}`, testModel429),
	}, "\n")

	fileID := mustCreateFile(t, fmt.Sprintf("test-retry-429-%s.jsonl", testRunID), jsonl)
	batchID := mustCreateBatch(t, fileID)

	finalBatch, _ := waitForBatchStatus(t, batchID, 5*time.Minute,
		openai.BatchStatusCompleted, openai.BatchStatusFailed)

	if finalBatch.Status != openai.BatchStatusCompleted {
		t.Fatalf("expected batch to complete despite 429s, got status %s", finalBatch.Status)
	}
	if finalBatch.RequestCounts.Completed != 5 {
		t.Errorf("expected 5 completed (after retries), got %d", finalBatch.RequestCounts.Completed)
	}
	if finalBatch.RequestCounts.Failed != 0 {
		t.Errorf("expected 0 failed (retries should succeed), got %d", finalBatch.RequestCounts.Failed)
	}

	t.Logf("429 retry test: completed=%d failed=%d total=%d",
		finalBatch.RequestCounts.Completed, finalBatch.RequestCounts.Failed, finalBatch.RequestCounts.Total)

	assertNoRequestErrors(t, testModel429)
}

// doTestRetryExhaustion submits a batch targeting a simulator with 100%
// failure injection. maxRetries is set to 1 via Helm, so the processor
// exhausts retries quickly and the batch must end in "failed" status.
func doTestRetryExhaustion(t *testing.T) {
	t.Helper()

	jsonl := strings.Join([]string{
		fmt.Sprintf(`{"custom_id":"fail-1","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello 1"}]}}`, testModelAlwaysFail),
		fmt.Sprintf(`{"custom_id":"fail-2","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello 2"}]}}`, testModelAlwaysFail),
	}, "\n")

	fileID := mustCreateFile(t, fmt.Sprintf("test-retry-exhaust-%s.jsonl", testRunID), jsonl)
	batchID := mustCreateBatch(t, fileID)

	finalBatch, _ := waitForBatchStatus(t, batchID, 3*time.Minute,
		openai.BatchStatusFailed, openai.BatchStatusCompleted)

	if finalBatch.Status != openai.BatchStatusFailed {
		t.Fatalf("expected batch to fail after retry exhaustion, got status %s", finalBatch.Status)
	}
	if finalBatch.RequestCounts.Failed == 0 {
		t.Errorf("expected failed request count > 0, got %d", finalBatch.RequestCounts.Failed)
	}

	t.Logf("retry exhaustion: completed=%d failed=%d total=%d",
		finalBatch.RequestCounts.Completed, finalBatch.RequestCounts.Failed, finalBatch.RequestCounts.Total)

	assertRequestErrors(t, testModelAlwaysFail)
}

// ──  GIE integration tests (require ENABLE_GIE=true) ───────────────────
//
// Current coverage: EPP routing smoke tests (header propagation, multi-model completion).
//
// Not yet covered (requires EPP-side observability improvements):
//   - Priority band interaction: verify interactive requests are served while batch
//     requests are shed under saturation. EPP does not expose per-request scheduling
//     decisions in logs or metrics.
//   - SLO-deadline ordering: verify batches with shorter completion_window are
//     dispatched first. Requires EPP queue-depth observability.
//   - Shedding under saturation: verify batch requests receive 429 when the
//     inference pool is saturated. Requires controllable load generation and
//     EPP saturation metrics.
//   - Mixed load with metrics: verify batch completion alongside interactive
//     traffic with retry/shedding counters. Requires EPP to export scheduling metrics.

// detectGIEDeployed checks whether at least one EPP deployment exists
// in the test namespace.
func detectGIEDeployed(t *testing.T) bool {
	t.Helper()

	out, err := exec.Command("kubectl", "get", "deployments",
		"-n", testNamespace,
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
// EPP with x-gateway-inference-objective configured.
//
// Verification: submit a batch, wait for completion, then check EPP logs for
// evidence that it received and routed the requests.
func doTestGIEHeaderPropagation(t *testing.T) {
	t.Helper()

	eppDeployment := fmt.Sprintf("%s-%s-epp", getEnvOrDefault("GIE_EPP_RELEASE", "epp"), testModel)
	sinceTime := time.Now().UTC().Format(time.RFC3339)

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
}

// doTestBatchCompletionThroughEPP verifies multi-model batch completion through
// separate EPP instances, confirming each EPP received requests.
func doTestBatchCompletionThroughEPP(t *testing.T) {
	t.Helper()

	sinceTime := time.Now().UTC().Format(time.RFC3339)
	eppPrefix := getEnvOrDefault("GIE_EPP_RELEASE", "epp")

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

	for _, model := range []string{testModel, testModelB} {
		eppDeployment := fmt.Sprintf("%s-%s-epp", eppPrefix, model)
		eppLogs := getEPPLogsSince(t, eppDeployment, sinceTime)

		if !strings.Contains(eppLogs, "EPP received request") {
			t.Errorf("EPP %s did not receive requests since %s;\nlog sample:\n%s",
				eppDeployment, sinceTime, truncateLog(eppLogs, 500))
		}
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// getSimulatorLogsSince fetches vllm-sim container logs for the simulator
// serving the given model, filtered to entries after sinceTime (RFC3339).
func getSimulatorLogsSince(t *testing.T, model, sinceTime string) string {
	t.Helper()

	simDeploymentLabel := ""
	switch model {
	case testModel:
		simDeploymentLabel = getEnvOrDefault("VLLM_SIM_NAME", "vllm-sim")
	case testModelB:
		simDeploymentLabel = getEnvOrDefault("VLLM_SIM_B_NAME", "vllm-sim-b")
	case testModel429:
		simDeploymentLabel = getEnvOrDefault("VLLM_SIM_429_NAME", "vllm-sim-429")
	case testModelAlwaysFail:
		simDeploymentLabel = getEnvOrDefault("VLLM_SIM_ALWAYS_FAIL_NAME", "vllm-sim-always-fail")
	default:
		t.Fatalf("unknown model %q for simulator log lookup", model)
	}

	out, err := exec.Command("kubectl", "logs",
		fmt.Sprintf("deployment/%s", simDeploymentLabel),
		"-n", testNamespace,
		fmt.Sprintf("--since-time=%s", sinceTime),
	).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl logs for simulator %s failed: %v\n%s", simDeploymentLabel, err, out)
	}
	return string(out)
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

	resp, err := http.Get(testProcessorObsURL + "/metrics")
	if err != nil {
		t.Fatalf("failed to scrape processor metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read processor metrics body: %v", err)
	}
	return string(body)
}

// assertNoRequestErrors verifies that request_errors_by_model_total for the
// given model is either absent or zero. When the HTTP client retries 429s
// transparently and all retries succeed, the executor never records an error.
func assertNoRequestErrors(t *testing.T, model string) {
	t.Helper()

	metrics := scrapeProcessorMetrics(t)

	pattern := regexp.MustCompile(fmt.Sprintf(`request_errors_by_model_total\{model=%q\}\s+(\d+)`, model))
	match := pattern.FindStringSubmatch(metrics)
	if match == nil {
		t.Logf("request_errors_by_model_total{model=%q} not found in metrics (expected: retries succeeded transparently)", model)
		return
	}
	if match[1] != "0" {
		t.Errorf("expected request_errors_by_model_total{model=%q} = 0 (retries should succeed), got %s", model, match[1])
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
