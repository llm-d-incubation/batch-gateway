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

// AIMD e2e tests verify that the adaptive concurrency controller reacts to
// backpressure from downstream inference endpoints and that per-endpoint
// isolation holds.
//
// Coverage:
//   - Decrease: 429s from sim-model-429 drive the AIMD limit below perEndpoint.
//   - Isolation: sim-model (0% failure) stays at perEndpoint while
//     sim-model-429 decreases independently.
//   - Recovery: a dedicated sim-model-aimd starts at 100% failure (driving
//     limit to min), then is patched to 0% failure; subsequent requests
//     drive the limit back toward perEndpoint.

package e2e_test

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

const (
	aimdPerEndpoint = 10
	aimdMin         = 5
)

func testAIMD(t *testing.T) {
	t.Run("DecreaseAndIsolation", doTestAIMDDecreaseAndIsolation)
	t.Run("Recovery", doTestAIMDRecovery)
}

// doTestAIMDDecreaseAndIsolation submits a multi-model batch targeting both
// sim-model-429 (50% failure injection) and sim-model (0% failure). After
// completion it scrapes processor metrics and asserts:
//   - The 429 endpoint's AIMD concurrency limit dropped below perEndpoint.
//   - The healthy endpoint's AIMD concurrency limit stayed at perEndpoint.
//   - The 429 endpoint accumulated AIMD decrease signals.
//
// With 50% failure rate and maxRetries=10, all requests eventually succeed,
// but intermediate 429 responses trigger AIMD multiplicative decreases.
// Two consecutive 429s are enough to hit the floor (10→5 with backoffFactor=0.5,
// min=5), and the probability of avoiding two consecutive 429s across 10
// requests is negligible ((1−0.5²)^9 ≈ 7.5%).
func doTestAIMDDecreaseAndIsolation(t *testing.T) {
	const (
		// 10 requests at 50% failure rate ensures at least two consecutive 429s
		// with >99% probability, which is enough to drive AIMD from perEndpoint
		// (10) down to min (5) via two multiplicative decreases (10→5).
		num429Requests    = 10
		numNormalRequests = 2
	)

	lines := make([]string, 0, num429Requests+numNormalRequests)
	for i := range num429Requests {
		lines = append(lines, fmt.Sprintf(
			`{"custom_id":"aimd-429-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"AIMD test %d"}]}}`,
			i+1, testModel429, i+1,
		))
	}
	for i := range numNormalRequests {
		lines = append(lines, fmt.Sprintf(
			`{"custom_id":"aimd-ok-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"AIMD baseline %d"}]}}`,
			i+1, testModel, i+1,
		))
	}

	fileID := mustCreateFile(t, fmt.Sprintf("aimd-e2e-%s.jsonl", testRunID), strings.Join(lines, "\n"))
	batchID := mustCreateBatch(t, fileID)

	batch, _ := waitForBatchStatus(t, batchID, 5*time.Minute, openai.BatchStatusCompleted)

	// All requests should complete: sim-model-429 has maxRetries=10 in
	// dev-deploy, so each request retries on 429 until it gets a 200.
	// The probability of exhausting 10 retries at 50% failure is (0.5)^10 < 0.1%.
	// AIMD is triggered by intermediate 429 responses during retries,
	// not by the final outcome.
	totalRequests := int64(num429Requests + numNormalRequests)
	if batch.RequestCounts.Completed != totalRequests {
		t.Fatalf("expected %d completed, got %d (failed=%d)",
			totalRequests, batch.RequestCounts.Completed, batch.RequestCounts.Failed)
	}

	metrics := scrapeProcessorMetrics(t)

	aimdLimits := parseGaugeByEndpoint(t, metrics, "batch_processor_aimd_concurrency_limit")
	aimdDecreases := parseCounterByEndpoint(t, metrics, "batch_processor_aimd_decreases_total")

	var (
		found429Endpoint    bool
		foundNormalEndpoint bool
	)
	for endpoint, limit := range aimdLimits {
		t.Logf("aimd_concurrency_limit{endpoint=%q} = %.0f", endpoint, limit)

		if strings.Contains(endpoint, "vllm-sim-429") {
			found429Endpoint = true
			if limit >= float64(aimdPerEndpoint) {
				t.Errorf("429 endpoint limit = %.0f, want < %d (AIMD should have decreased)", limit, aimdPerEndpoint)
			}

			if count, ok := aimdDecreases[endpoint]; ok {
				t.Logf("aimd_decreases_total{endpoint=%q} = %.0f", endpoint, count)
				if count == 0 {
					t.Errorf("expected AIMD decrease signals > 0 for 429 endpoint")
				}
			} else {
				t.Errorf("no aimd_decreases_total found for 429 endpoint %q", endpoint)
			}
		}

		if isHealthySimEndpoint(endpoint) {
			foundNormalEndpoint = true
			if limit != float64(aimdPerEndpoint) {
				t.Errorf("healthy endpoint limit = %.0f, want %d (should be unaffected)", limit, aimdPerEndpoint)
			}
		}
	}

	if !found429Endpoint {
		t.Error("no AIMD metric found for an endpoint containing 'vllm-sim-429'")
	}
	if !foundNormalEndpoint {
		t.Error("no AIMD metric found for a healthy (non-429) endpoint")
	}
}

// isHealthySimEndpoint returns true if the endpoint label refers to the
// primary vllm-sim simulator, not any variant (vllm-sim-429, vllm-sim-b, etc.).
// Endpoint labels are gateway URLs like "http://vllm-sim.<ns>.svc.cluster.local:8000",
// so matching "vllm-sim." (with trailing dot) excludes all hyphenated variants.
func isHealthySimEndpoint(endpoint string) bool {
	return strings.Contains(endpoint, "vllm-sim.") &&
		!strings.Contains(endpoint, "vllm-sim-")
}

// parseGaugeByEndpoint extracts all {endpoint="..."} values for a gauge metric.
// Returns a map from endpoint label to the gauge value.
func parseGaugeByEndpoint(t *testing.T, metrics, metricName string) map[string]float64 {
	t.Helper()

	pattern := regexp.MustCompile(fmt.Sprintf(`%s\{[^}]*endpoint="([^"]+)"[^}]*\}\s+([0-9.e+-]+)`, regexp.QuoteMeta(metricName)))
	result := make(map[string]float64)
	for _, match := range pattern.FindAllStringSubmatch(metrics, -1) {
		val, err := strconv.ParseFloat(match[2], 64)
		if err != nil {
			t.Logf("failed to parse %s value %q: %v", metricName, match[2], err)
			continue
		}
		result[match[1]] = val
	}
	return result
}

// parseCounterByEndpoint sums counter values across signal labels for each endpoint.
// For counters like aimd_decreases_total{endpoint="...",signal="..."}, this
// returns the total across all signals per endpoint.
func parseCounterByEndpoint(t *testing.T, metrics, metricName string) map[string]float64 {
	t.Helper()

	pattern := regexp.MustCompile(fmt.Sprintf(`%s\{[^}]*endpoint="([^"]+)"[^}]*\}\s+([0-9.e+-]+)`, regexp.QuoteMeta(metricName)))
	result := make(map[string]float64)
	for _, match := range pattern.FindAllStringSubmatch(metrics, -1) {
		val, err := strconv.ParseFloat(match[2], 64)
		if err != nil {
			t.Logf("failed to parse %s value %q: %v", metricName, match[2], err)
			continue
		}
		result[match[1]] += val
	}
	return result
}

// doTestAIMDRecovery verifies that the AIMD concurrency limit recovers after
// backpressure subsides. It uses a dedicated simulator (vllm-sim-aimd) that
// starts at 100% failure rate. The test:
//  1. Submits requests to drive the AIMD limit to min (5).
//  2. Patches the simulator to 0% failure rate and waits for rollout.
//  3. Submits more requests and verifies the limit increased above min.
//
// t.Cleanup restores the simulator to 100% failure for subsequent runs.
//
// Because AIMD increases by +1 per successful window, full recovery to
// perEndpoint (10) requires many requests. We only assert limit > min to
// avoid flakiness from timing variance.
func doTestAIMDRecovery(t *testing.T) {
	if !testKubectlAvailable {
		t.Skip("kubectl not available, skipping AIMD recovery test")
	}

	const (
		numFailRequests   = 10
		numRecovRequests  = 30
		aimdSimDeployment = "vllm-sim-aimd"
	)

	// Phase 1: Drive AIMD limit to min with 100% failure rate.
	// The simulator starts with 100% failure, so all requests get 429s
	// (retried up to maxRetries=10). This triggers multiplicative decreases.
	// We use waitForRetryExhaustion (not waitForBatchStatus) because 100%
	// failure means output lines have status 429, which validateBatchResults
	// would reject.
	t.Log("phase 1: submitting requests to drive AIMD limit to min...")
	lines := make([]string, 0, numFailRequests)
	for i := range numFailRequests {
		lines = append(lines, fmt.Sprintf(
			`{"custom_id":"aimd-recov-fail-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"AIMD fail %d"}]}}`,
			i+1, testModelAIMD, i+1,
		))
	}

	fileID := mustCreateFile(t, fmt.Sprintf("aimd-recov-fail-%s.jsonl", testRunID), strings.Join(lines, "\n"))
	batchID := mustCreateBatch(t, fileID)

	batch := waitForRetryExhaustion(t, batchID, 5*time.Minute)
	if batch.Status != openai.BatchStatusCompleted {
		t.Fatalf("expected batch status %q, got %q", openai.BatchStatusCompleted, batch.Status)
	}

	t.Logf("phase 1 batch: completed=%d, failed=%d",
		batch.RequestCounts.Completed, batch.RequestCounts.Failed)

	metrics := scrapeProcessorMetrics(t)
	aimdLimits := parseGaugeByEndpoint(t, metrics, "batch_processor_aimd_concurrency_limit")

	var aimdEndpoint string
	for endpoint, limit := range aimdLimits {
		if strings.Contains(endpoint, aimdSimDeployment) {
			aimdEndpoint = endpoint
			t.Logf("phase 1: aimd_concurrency_limit{endpoint=%q} = %.0f", endpoint, limit)
			if limit > float64(aimdMin) {
				t.Fatalf("expected AIMD limit <= %d after 100%% failure, got %.0f", aimdMin, limit)
			}
			break
		}
	}
	if aimdEndpoint == "" {
		t.Fatalf("no AIMD metric found for endpoint containing %q", aimdSimDeployment)
	}

	// Phase 2: Patch simulator to 0% failure and wait for rollout.
	t.Log("phase 2: patching simulator to 0% failure rate...")
	patchSimulatorFailureRate(t, aimdSimDeployment, testModelAIMD, 0)
	t.Cleanup(func() {
		t.Log("cleanup: restoring simulator to 100% failure rate")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Timing args (10ms/10ms) must match dev-deploy.sh's install_vllm_sim defaults.
		patch := fmt.Sprintf(
			`{"spec":{"template":{"spec":{"containers":[{"name":"vllm-sim","args":["--model","%s","--port","8000","--time-to-first-token=10ms","--inter-token-latency=10ms","--v=5","--failure-injection-rate=100","--failure-types=rate_limit"]}]}}}}`,
			testModelAIMD,
		)
		out, err := exec.CommandContext(ctx, "kubectl", "patch", "deployment", aimdSimDeployment,
			"-n", testNamespace,
			"--type=strategic",
			"-p", patch,
		).CombinedOutput()
		if err != nil {
			t.Errorf("cleanup: failed to restore %s to 100%% failure: %v\n%s\nMANUAL RESTORE REQUIRED: kubectl patch deployment %s -n %s ...",
				aimdSimDeployment, err, out, aimdSimDeployment, testNamespace)
			return
		}

		rollCtx, rollCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer rollCancel()
		out, err = exec.CommandContext(rollCtx, "kubectl", "rollout", "status",
			"deployment/"+aimdSimDeployment, "-n", testNamespace, "--timeout=90s",
		).CombinedOutput()
		if err != nil {
			t.Errorf("cleanup: rollout wait failed for %s: %v\n%s", aimdSimDeployment, err, out)
		}
	})
	waitForRollout(t, aimdSimDeployment)
	t.Log("phase 2: simulator rollout complete, now at 0% failure rate")

	// Phase 3: Submit requests that all succeed, triggering additive increases.
	// Each successful window adds +1 to the limit (from aimd.additiveIncrease=1).
	t.Log("phase 3: submitting requests to trigger AIMD recovery...")
	recovLines := make([]string, 0, numRecovRequests)
	for i := range numRecovRequests {
		recovLines = append(recovLines, fmt.Sprintf(
			`{"custom_id":"aimd-recov-ok-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"AIMD recover %d"}]}}`,
			i+1, testModelAIMD, i+1,
		))
	}

	recovFileID := mustCreateFile(t, fmt.Sprintf("aimd-recov-ok-%s.jsonl", testRunID), strings.Join(recovLines, "\n"))
	recovBatchID := mustCreateBatch(t, recovFileID)

	recovBatch, _ := waitForBatchStatus(t, recovBatchID, 5*time.Minute, openai.BatchStatusCompleted)
	if recovBatch.RequestCounts.Completed != int64(numRecovRequests) {
		t.Fatalf("expected %d completed in recovery batch, got %d (failed=%d)",
			numRecovRequests, recovBatch.RequestCounts.Completed, recovBatch.RequestCounts.Failed)
	}

	// Verify AIMD limit increased above min.
	metrics = scrapeProcessorMetrics(t)
	aimdLimits = parseGaugeByEndpoint(t, metrics, "batch_processor_aimd_concurrency_limit")

	if limit, ok := aimdLimits[aimdEndpoint]; ok {
		t.Logf("phase 3: aimd_concurrency_limit{endpoint=%q} = %.0f", aimdEndpoint, limit)
		if limit <= float64(aimdMin) {
			t.Errorf("expected AIMD limit > %d after recovery, got %.0f", aimdMin, limit)
		}
	} else {
		t.Errorf("no AIMD metric found for endpoint %q after recovery", aimdEndpoint)
	}

	// Verify increase counter registered.
	aimdIncreases := parseCounterByEndpoint(t, metrics, "batch_processor_aimd_increases_total")
	if count, ok := aimdIncreases[aimdEndpoint]; ok {
		t.Logf("aimd_increases_total{endpoint=%q} = %.0f", aimdEndpoint, count)
		if count == 0 {
			t.Errorf("expected AIMD increase signals > 0 for recovered endpoint")
		}
	} else {
		t.Errorf("no aimd_increases_total found for endpoint %q", aimdEndpoint)
	}
}

// patchSimulatorFailureRate patches the vllm-sim container args in the given
// deployment to set --failure-injection-rate to the specified value. A rate of
// 0 removes the failure injection flags entirely.
//
// The timing args (10ms/10ms) must match dev-deploy.sh's install_vllm_sim
// invocation for the AIMD simulator; if those defaults change, update here.
func patchSimulatorFailureRate(t *testing.T, deployment, model string, rate int) {
	t.Helper()

	var patch string
	if rate == 0 {
		patch = fmt.Sprintf(
			`{"spec":{"template":{"spec":{"containers":[{"name":"vllm-sim","args":["--model","%s","--port","8000","--time-to-first-token=10ms","--inter-token-latency=10ms","--v=5"]}]}}}}`,
			model,
		)
	} else {
		patch = fmt.Sprintf(
			`{"spec":{"template":{"spec":{"containers":[{"name":"vllm-sim","args":["--model","%s","--port","8000","--time-to-first-token=10ms","--inter-token-latency=10ms","--v=5","--failure-injection-rate=%d","--failure-types=rate_limit"]}]}}}}`,
			model, rate,
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "kubectl", "patch", "deployment", deployment,
		"-n", testNamespace,
		"--type=strategic",
		"-p", patch,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("failed to patch %s failure rate to %d%%: %v\n%s", deployment, rate, err, out)
	}
	t.Logf("patched %s: failure-injection-rate=%d%%", deployment, rate)
}
