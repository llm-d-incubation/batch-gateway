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
//   - Decrease: 429s from sim-model-429 drive the AIMD limit below the configured perEndpoint limit.
//   - Isolation: sim-model (0% failure) stays at the configured perEndpoint limit while
//     sim-model-429 decreases independently.
//   - Recovery: a dedicated sim-model-aimd starts at 100% failure (driving
//     limit to min), then is flipped to 0% failure via /admin/config;
//     subsequent requests drive the limit back toward the configured perEndpoint limit.

package e2e_test

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"gopkg.in/yaml.v3"
)

const (
	aimdMin = 5
)

func testAIMD(t *testing.T) {
	t.Cleanup(func() { deleteE2ECurlPod(t) })

	t.Run("DecreaseAndIsolation", doTestAIMDDecreaseAndIsolation)
	t.Run("Recovery", doTestAIMDRecovery)
}

// doTestAIMDDecreaseAndIsolation submits a multi-model batch targeting both
// sim-model-429 (50% failure injection) and sim-model (0% failure). After
// completion it scrapes processor metrics and asserts:
//   - The 429 endpoint's AIMD concurrency limit dropped below the configured perEndpoint limit.
//   - The healthy endpoint's AIMD concurrency limit stayed at the configured perEndpoint limit.
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
		// with >99% probability, which is enough to drive AIMD from the
		// configured perEndpoint limit down to min (5) via multiplicative decreases.
		num429Requests    = 10
		numNormalRequests = 2
	)

	// Snapshot AIMD state before the test. Counters and gauges persist across
	// runs on a long-lived processor, so we assert on deltas rather than
	// absolute values.
	metricsBefore := scrapeProcessorMetrics(t)
	decreasesBefore := parseCounterByEndpoint(t, metricsBefore, "batch_processor_aimd_decreases_total")
	limitsBefore := parseGaugeByEndpoint(t, metricsBefore, "batch_processor_aimd_concurrency_limit")

	var decreaseBefore429 float64
	for endpoint, count := range decreasesBefore {
		if strings.Contains(endpoint, testSimService429) {
			decreaseBefore429 = count
			break
		}
	}
	var limitBeforeHealthy float64
	for endpoint, limit := range limitsBefore {
		if isHealthySimEndpoint(endpoint) {
			limitBeforeHealthy = limit
			break
		}
	}

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
	expectedPerEndpoint := getProcessorPerEndpointConcurrency(t)

	aimdLimits := parseGaugeByEndpoint(t, metrics, "batch_processor_aimd_concurrency_limit")
	aimdDecreases := parseCounterByEndpoint(t, metrics, "batch_processor_aimd_decreases_total")

	var (
		found429Endpoint    bool
		foundNormalEndpoint bool
	)
	for endpoint, limit := range aimdLimits {
		t.Logf("aimd_concurrency_limit{endpoint=%q} = %.0f", endpoint, limit)

		if strings.Contains(endpoint, testSimService429) {
			found429Endpoint = true
			if limit >= float64(expectedPerEndpoint) {
				t.Errorf("429 endpoint limit = %.0f, want < %d (AIMD should have decreased)", limit, expectedPerEndpoint)
			}

			if count, ok := aimdDecreases[endpoint]; ok {
				delta := count - decreaseBefore429
				t.Logf("aimd_decreases_total{endpoint=%q} = %.0f (delta=%.0f)", endpoint, count, delta)
				if delta == 0 {
					t.Errorf("expected AIMD decrease delta > 0 for 429 endpoint (before=%.0f, after=%.0f)",
						decreaseBefore429, count)
				}
			} else {
				t.Errorf("no aimd_decreases_total found for 429 endpoint %q", endpoint)
			}
		}

		if isHealthySimEndpoint(endpoint) {
			foundNormalEndpoint = true
			if limitBeforeHealthy == 0 {
				if limit != float64(expectedPerEndpoint) {
					t.Errorf("healthy endpoint limit = %.0f, want %d (first run: should start at max)",
						limit, expectedPerEndpoint)
				}
			} else if limit < limitBeforeHealthy {
				t.Errorf("healthy endpoint limit decreased during test "+
					"(before=%.0f, after=%.0f); should be unaffected by 429 traffic",
					limitBeforeHealthy, limit)
			}
		}
	}

	if !found429Endpoint {
		t.Errorf("no AIMD metric found for an endpoint containing %q", testSimService429)
	}
	if !foundNormalEndpoint {
		t.Error("no AIMD metric found for a healthy (non-429) endpoint")
	}
}

// getProcessorPerEndpointConcurrency reads the deployed processor ConfigMap and
// returns the configured concurrency.per_endpoint value.
func getProcessorPerEndpointConcurrency(t *testing.T) int {
	t.Helper()

	cmName := fmt.Sprintf("%s-processor-config", testHelmRelease)
	configYAML := kubectlGetConfigMap(t, cmName)

	var root struct {
		Concurrency struct {
			PerEndpoint *int `yaml:"per_endpoint"`
		} `yaml:"concurrency"`
	}
	if err := yaml.Unmarshal([]byte(configYAML), &root); err != nil {
		t.Fatalf("parse processor config.yaml: %v", err)
	}
	if root.Concurrency.PerEndpoint == nil {
		t.Fatalf("concurrency.per_endpoint missing in config:\n%s", configYAML)
	}

	return *root.Concurrency.PerEndpoint
}

// isHealthySimEndpoint returns true if the endpoint label refers to the
// healthy primary model used by this test.
//
// In non-GIE mode the processor talks directly to the simulator service
// (vllm-sim.<ns>...), while in GIE mode it routes the same model through the
// per-model EPP service (epp-<model>-epp.<ns>...).
func isHealthySimEndpoint(endpoint string) bool {
	return strings.Contains(endpoint, "vllm-sim.") ||
		strings.Contains(endpoint, fmt.Sprintf("epp-%s-epp.", testModel))
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
//  2. Flips the simulator to 0% failure rate via /admin/config (no rollout needed).
//  3. Submits more requests and verifies the limit eventually increases above
//     its phase-1 baseline.
//
// t.Cleanup restores the simulator to 100% failure for subsequent runs.
//
// Because AIMD increases by +1 per successful window, full recovery to the
// configured perEndpoint limit requires many requests. We only assert that the
// limit eventually increases beyond the phase-1 floor, since the exported gauge
// can lag the recovery batch completion by a scrape interval.
func doTestAIMDRecovery(t *testing.T) {
	if !testKubectlAvailable {
		t.Skip("kubectl not available, skipping AIMD recovery test")
	}

	const (
		numFailRequests  = 10
		numRecovRequests = 30
	)

	t.Cleanup(func() {
		t.Log("cleanup: restoring vllm-sim-aimd to 100% failure rate")
		setSimAdminConfig(t, testSimServiceAIMD, `{"failure-injection-rate": 100, "failure-types": ["rate_limit"]}`)
	})

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

	var (
		aimdEndpoint  string
		baselineLimit float64
	)
	for endpoint, limit := range aimdLimits {
		if strings.Contains(endpoint, testSimServiceAIMD) {
			aimdEndpoint = endpoint
			baselineLimit = limit
			t.Logf("phase 1: aimd_concurrency_limit{endpoint=%q} = %.0f", endpoint, limit)
			if limit > float64(aimdMin) {
				t.Fatalf("expected AIMD limit <= %d after 100%% failure, got %.0f", aimdMin, limit)
			}
			break
		}
	}
	if aimdEndpoint == "" {
		t.Fatalf("no AIMD metric found for endpoint containing %q", testSimServiceAIMD)
	}

	// Phase 2: Flip simulator to 0% failure via /admin/config.
	// Unlike kubectl patch + rollout, this takes effect immediately without
	// restarting the pod (~1s vs ~90s).
	t.Log("phase 2: setting vllm-sim-aimd to 0% failure rate via /admin/config")
	setSimAdminConfig(t, testSimServiceAIMD, `{"failure-injection-rate": 0}`)

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

	limit, increases := waitForAIMDLimitIncrease(t, aimdEndpoint, baselineLimit, 15*time.Second, 500*time.Millisecond)
	t.Logf("phase 3: aimd_concurrency_limit{endpoint=%q} = %.0f", aimdEndpoint, limit)
	t.Logf("aimd_increases_total{endpoint=%q} = %.0f", aimdEndpoint, increases)
}

func waitForAIMDLimitIncrease(t *testing.T, endpoint string, baselineLimit float64, timeout, interval time.Duration) (float64, float64) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	lastLimit := baselineLimit
	var lastIncreases float64

	for {
		metrics := scrapeProcessorMetrics(t)
		aimdLimits := parseGaugeByEndpoint(t, metrics, "batch_processor_aimd_concurrency_limit")
		if limit, ok := aimdLimits[endpoint]; ok {
			lastLimit = limit
		}

		aimdIncreases := parseCounterByEndpoint(t, metrics, "batch_processor_aimd_increases_total")
		if increases, ok := aimdIncreases[endpoint]; ok {
			lastIncreases = increases
		}

		if lastLimit > baselineLimit {
			return lastLimit, lastIncreases
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected AIMD limit to increase above %.0f after recovery, last limit=%.0f, increases_total=%.0f",
				baselineLimit, lastLimit, lastIncreases)
		}

		time.Sleep(interval)
	}
}
