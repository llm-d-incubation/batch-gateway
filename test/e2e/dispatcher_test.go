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
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	asyncapi "github.com/llm-d-incubation/llm-d-async/api"
	"github.com/llm-d-incubation/llm-d-async/producer"
	"github.com/openai/openai-go/v3"
	"github.com/redis/go-redis/v9"
)

var (
	testRedisURL          = getEnvOrDefault("TEST_REDIS_URL", "redis://localhost:6399")
	testSimURL            = getEnvOrDefault("TEST_SIM_URL", "http://localhost:8099")
	dispatcherPool        = "sim-pool"
	dispatcherReqQueue    = "llm-d-async:requests:" + dispatcherPool
	dispatcherResultQueue = "llm-d-async:results:" + dispatcherPool

	gatePool              = "sim-pool-gate"
	gateReqQueue          = "llm-d-async:requests:" + gatePool
	gateResultQueue       = "llm-d-async:results:" + gatePool
	dispatchGateBudgetKey = "dispatch-gate-budget"

	scrapePool        = "sim-pool-scrape"
	scrapeReqQueue    = "llm-d-async:requests:" + scrapePool
	scrapeResultQueue = "llm-d-async:results:" + scrapePool

	promPool        = "sim-pool-prom"
	promReqQueue    = "llm-d-async:requests:" + promPool
	promResultQueue = "llm-d-async:results:" + promPool

	injectPool        = "sim-pool-inject"
	injectReqQueue    = "llm-d-async:requests:" + injectPool
	injectResultQueue = "llm-d-async:results:" + injectPool
	injectModel       = "sim-model-inject"
)

func newDispatcherRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	opts, err := redis.ParseURL(testRedisURL)
	if err != nil {
		t.Fatalf("Failed to parse TEST_REDIS_URL %q: %v", testRedisURL, err)
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("Failed to connect to Redis at %s: %v", testRedisURL, err)
	}
	return client
}

func newDispatcherProducer(t *testing.T, rdb *redis.Client, poolName string) *producer.RedisSortedSetProducer {
	t.Helper()
	p, err := producer.NewRedisSortedSetProducer(
		producer.RedisSortedSetConfig{
			RequestQueueName: "llm-d-async:requests:" + poolName,
			ResultQueueName:  "llm-d-async:results:" + poolName,
		},
		producer.WithRedisClient(rdb),
	)
	if err != nil {
		t.Fatalf("Failed to create producer for pool %s: %v", poolName, err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// detectDispatcherDeployed checks whether at least one async-processor
// deployment exists in the test namespace.
func detectDispatcherDeployed(t *testing.T) bool {
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
		if strings.Contains(line, "async-processor") {
			return true
		}
	}
	return false
}

func TestDispatcher(t *testing.T) {
	if !detectDispatcherDeployed(t) {
		t.Skip("skipping: dispatcher not deployed")
	}
	rdb := newDispatcherRedisClient(t)
	defer rdb.Close()

	waitForServerUp(t, testApiserverURL, 30*time.Second)

	if resp, err := http.Get(testApiserverObsURL + "/ready"); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			waitForReady(t, testApiserverObsURL, 30*time.Second)
		}
	}

	t.Cleanup(func() {
		ctx := context.Background()
		rdb.Del(ctx, dispatcherReqQueue, dispatcherResultQueue)
		rdb.Del(ctx, gateReqQueue, gateResultQueue)
		rdb.Del(ctx, scrapeReqQueue, scrapeResultQueue)
		rdb.Del(ctx, promReqQueue, promResultQueue)
	})

	t.Run("BatchThroughDispatcher", func(t *testing.T) {
		testDispatcherBatchRoundTrip(t, rdb)
	})
	t.Run("MultiRequestBatch", func(t *testing.T) {
		testDispatcherMultiRequestBatch(t, rdb)
	})
	t.Run("HTTPErrorStatusPreserved", func(t *testing.T) {
		testDispatcherHTTPErrorStatusPreserved(t, rdb)
	})
	t.Run("BatchCancel", func(t *testing.T) {
		doTestBatchCancel(t)
	})
	t.Run("DispatchGate", func(t *testing.T) {
		testDispatcherRedisGate(t, rdb)
	})
	t.Run("EndpointScrapeGate", func(t *testing.T) {
		testDispatcherEndpointScrapeGate(t, rdb)
	})
	t.Run("PrometheusGate", func(t *testing.T) {
		testDispatcherPrometheusGate(t, rdb)
	})
}

// testDispatcherHTTPErrorStatusPreserved verifies that an HTTP error status
// from the async ResultMessage (e.g. 403 with an empty body) is preserved in
// the batch output file instead of being collapsed into parse_error.
//
// Uses the consumer-less "sim-pool-inject" pool: no async-processor subscribes
// to it, so the request stays in the queue deterministically until the test
// removes it and injects a synthetic ResultMessage.
func testDispatcherHTTPErrorStatusPreserved(t *testing.T, rdb *redis.Client) {
	ctx := context.Background()

	rdb.Del(ctx, injectReqQueue, injectResultQueue)
	defer rdb.Del(ctx, injectReqQueue, injectResultQueue)

	jsonl := fmt.Sprintf(
		`{"custom_id":"dreq-403","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"expect 403"}]}}`,
		injectModel)
	fileID := mustCreateFile(t, fmt.Sprintf("dispatcher-http-error-%s.jsonl", testRunID), jsonl)
	batchID := mustCreateBatch(t, fileID)
	t.Logf("Created batch %s; waiting for request in %s", batchID, injectReqQueue)

	reqID := waitAndStealQueuedRequestID(t, rdb, injectReqQueue, 30*time.Second)
	t.Logf("Got request %s; injecting StatusCode=403 result", reqID)

	resultBytes, err := json.Marshal(asyncapi.ResultMessage{
		ID:         reqID,
		StatusCode: http.StatusForbidden,
		Payload:    "",
	})
	if err != nil {
		t.Fatalf("marshal ResultMessage: %v", err)
	}
	if err := rdb.LPush(ctx, injectResultQueue, string(resultBytes)).Err(); err != nil {
		t.Fatalf("LPUSH result: %v", err)
	}

	finalBatch := waitForRetryExhaustion(t, batchID, 2*time.Minute)

	if finalBatch.Status != openai.BatchStatusCompleted {
		t.Errorf("expected batch status %q, got %q", openai.BatchStatusCompleted, finalBatch.Status)
	}
	if finalBatch.RequestCounts.Completed != 0 {
		t.Errorf("completed = %d, want 0", finalBatch.RequestCounts.Completed)
	}
	if finalBatch.RequestCounts.Failed != 1 {
		t.Errorf("failed = %d, want 1", finalBatch.RequestCounts.Failed)
	}
	if finalBatch.OutputFileID == "" {
		t.Fatal("expected output_file_id with HTTP 403 response")
	}
	if finalBatch.ErrorFileID != "" {
		t.Errorf("expected empty error_file_id (HTTP errors go to output), got %q", finalBatch.ErrorFileID)
	}

	output := fetchOutputFile(t, finalBatch)
	var found403 bool
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rl batchResultLine
		if err := json.Unmarshal([]byte(line), &rl); err != nil {
			t.Fatalf("invalid output line: %v\n%s", err, line)
		}
		if rl.Error != nil {
			t.Fatalf("expected HTTP response in output, got error file entry code=%q message=%q",
				rl.Error.Code, rl.Error.Message)
		}
		if rl.Response == nil {
			t.Fatal("expected response object in output line")
		}
		if rl.Response.StatusCode == http.StatusForbidden {
			found403 = true
		} else {
			t.Errorf("status_code = %d, want 403", rl.Response.StatusCode)
		}
	}
	if !found403 {
		t.Errorf("expected status_code 403 in output file, got:\n%s", output)
	}
	t.Logf("Batch %s preserved HTTP 403 in output (no parse_error)", batchID)
}

// waitAndStealQueuedRequestID waits until a request appears in the Redis sorted
// set queue, removes it, and returns the request ID from the InternalRequest envelope.
func waitAndStealQueuedRequestID(t *testing.T, rdb *redis.Client, queue string, timeout time.Duration) string {
	t.Helper()

	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		members, err := rdb.ZRange(ctx, queue, 0, 0).Result()
		if err != nil {
			t.Fatalf("ZRange %s: %v", queue, err)
		}
		if len(members) == 0 {
			time.Sleep(200 * time.Millisecond)
			continue
		}

		member := members[0]
		removed, err := rdb.ZRem(ctx, queue, member).Result()
		if err != nil {
			t.Fatalf("ZRem %s: %v", queue, err)
		}
		if removed == 0 {
			// Another consumer won the race; retry.
			continue
		}

		var ir asyncapi.InternalRequest
		if err := json.Unmarshal([]byte(member), &ir); err != nil {
			t.Fatalf("unmarshal InternalRequest: %v\n%s", err, member)
		}
		if ir.PublicRequest == nil {
			t.Fatalf("InternalRequest missing PublicRequest: %s", member)
		}
		reqID := ir.PublicRequest.ReqID()
		if reqID == "" {
			t.Fatalf("empty request ID in queued message: %s", member)
		}
		return reqID
	}
	t.Fatalf("no request appeared in %s within %v", queue, timeout)
	return ""
}

func testDispatcherBatchRoundTrip(t *testing.T, rdb *redis.Client) {
	jsonl := fmt.Sprintf(
		`{"custom_id":"dreq-1","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello dispatcher"}]}}`,
		testModel)

	fileID := mustCreateFile(t, fmt.Sprintf("dispatcher-single-%s.jsonl", testRunID), jsonl)
	batchID := mustCreateBatch(t, fileID)
	t.Logf("Created batch %s with file %s", batchID, fileID)

	batch, results := waitForBatchStatus(t, batchID, 120*time.Second, openai.BatchStatusCompleted)

	if batch.RequestCounts.Total != 1 {
		t.Errorf("Expected 1 total request, got %d", batch.RequestCounts.Total)
	}
	if batch.RequestCounts.Completed != 1 {
		t.Errorf("Expected 1 completed request, got %d", batch.RequestCounts.Completed)
	}
	if results == nil {
		t.Fatal("Expected non-nil results")
	}
	if results.OutputLines != 1 {
		t.Errorf("Expected 1 output line, got %d", results.OutputLines)
	}

	t.Logf("Batch %s completed via dispatcher", batchID)
}

func testDispatcherMultiRequestBatch(t *testing.T, rdb *redis.Client) {
	jsonl := strings.Join([]string{
		fmt.Sprintf(`{"custom_id":"dreq-m1","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello 1"}]}}`, testModel),
		fmt.Sprintf(`{"custom_id":"dreq-m2","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello 2"}]}}`, testModel),
		fmt.Sprintf(`{"custom_id":"dreq-m3","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello 3"}]}}`, testModel),
	}, "\n")

	fileID := mustCreateFile(t, fmt.Sprintf("dispatcher-multi-%s.jsonl", testRunID), jsonl)
	batchID := mustCreateBatch(t, fileID)
	t.Logf("Created batch %s with 3 requests", batchID)

	batch, results := waitForBatchStatus(t, batchID, 120*time.Second, openai.BatchStatusCompleted)

	if batch.RequestCounts.Total != 3 {
		t.Errorf("Expected 3 total requests, got %d", batch.RequestCounts.Total)
	}
	if batch.RequestCounts.Completed != 3 {
		t.Errorf("Expected 3 completed requests, got %d", batch.RequestCounts.Completed)
	}
	if results == nil {
		t.Fatal("Expected non-nil results")
	}
	if results.OutputLines != 3 {
		t.Errorf("Expected 3 output lines, got %d", results.OutputLines)
	}

	t.Logf("All 3 requests completed via dispatcher")
}

func testDispatcherRedisGate(t *testing.T, rdb *redis.Client) {
	ctx := context.Background()

	p := newDispatcherProducer(t, rdb, gatePool)

	// Close the gate before submitting any request
	rdb.Set(ctx, dispatchGateBudgetKey, "0.0", 0)
	defer rdb.Del(ctx, dispatchGateBudgetKey)
	t.Log("Gate closed (budget=0.0)")

	// Wait for the dispatcher to see the closed gate (poll interval is 500ms)
	time.Sleep(2 * time.Second)

	// Enqueue a request via the producer (bypass the processor
	// so we can observe the gate independently of BRPOP timeouts)
	reqID := fmt.Sprintf("gate-test-%s", testRunID)
	err := p.SubmitRequest(ctx, &asyncapi.RequestMessage{
		ID:       reqID,
		Created:  time.Now().Unix(),
		Deadline: time.Now().Add(5 * time.Minute).Unix(),
		Payload:  map[string]any{"model": testModel, "prompt": "Hello gate", "max_tokens": 5},
		Endpoint: "/v1/completions",
	})
	if err != nil {
		t.Fatalf("Failed to submit request: %v", err)
	}
	t.Logf("Enqueued request %s while gate is closed", reqID)

	// Verify request stays in the queue (gate blocks dispatch)
	time.Sleep(3 * time.Second)
	queueDepth, _ := rdb.ZCard(ctx, gateReqQueue).Result()
	if queueDepth == 0 {
		t.Fatal("Expected request in dispatcher queue while gate is closed, but queue is empty")
	}
	t.Logf("Confirmed: request stuck in queue (depth=%d, gate closed)", queueDepth)

	// Open the gate
	rdb.Set(ctx, dispatchGateBudgetKey, "1.0", 0)
	t.Log("Gate opened (budget=1.0)")

	// Wait for the dispatcher to process the request
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("Timed out waiting for dispatcher to drain queue after gate opened")
		default:
		}
		depth, _ := rdb.ZCard(ctx, gateReqQueue).Result()
		if depth == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Poll results via the producer until we find ours
	pollCtx, pollCancel := context.WithTimeout(ctx, 10*time.Second)
	defer pollCancel()
	for {
		result, err := p.GetResult(pollCtx)
		if err != nil {
			t.Fatalf("Failed to get result for %s: %v", reqID, err)
		}
		if result.ID == reqID {
			t.Logf("Request %s completed after gate opened", reqID)
			return
		}
		t.Logf("Skipped stale result %s", result.ID)
	}
}

func testDispatcherEndpointScrapeGate(t *testing.T, rdb *redis.Client) {
	ctx := context.Background()

	p := newDispatcherProducer(t, rdb, scrapePool)

	// Saturate the sim — gate should close
	// (endpoint-scrape gate: vllm:num_requests_waiting / max_count_per_pod >= 1 → budget 0)
	setSimWaitingRequests(t, 10)
	defer setSimWaitingRequests(t, 0)
	t.Log("Sim saturated (waiting-requests=10, gate should close)")

	// Give the scrape gate time to poll the new metric value
	time.Sleep(3 * time.Second)

	// Enqueue a request — it should stay in the queue (gate closed)
	reqID := fmt.Sprintf("scrape-gate-%s", testRunID)
	err := p.SubmitRequest(ctx, &asyncapi.RequestMessage{
		ID:       reqID,
		Created:  time.Now().Unix(),
		Deadline: time.Now().Add(5 * time.Minute).Unix(),
		Payload:  map[string]any{"model": testModel, "prompt": "Hello scrape gate", "max_tokens": 5},
		Endpoint: "/v1/completions",
	})
	if err != nil {
		t.Fatalf("Failed to submit request: %v", err)
	}
	t.Logf("Enqueued request %s while gate is closed", reqID)

	// Verify no result arrives (gate closed)
	time.Sleep(3 * time.Second)
	queueDepth, _ := rdb.ZCard(ctx, scrapeReqQueue).Result()
	if queueDepth == 0 {
		t.Fatal("Expected request in queue while gate is closed, but queue is empty")
	}
	t.Logf("Confirmed: request stuck in queue (depth=%d, gate closed)", queueDepth)

	// Clear saturation — gate should open
	setSimWaitingRequests(t, 0)
	t.Log("Sim idle (waiting-requests=0, gate should open)")

	// Wait for the scrape gate to pick up the updated metrics and dispatcher to drain
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("Timed out waiting for dispatcher to drain queue after gate opened")
		default:
		}
		depth, _ := rdb.ZCard(ctx, scrapeReqQueue).Result()
		if depth == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Poll result
	pollCtx, pollCancel := context.WithTimeout(ctx, 10*time.Second)
	defer pollCancel()
	for {
		result, err := p.GetResult(pollCtx)
		if err != nil {
			t.Fatalf("Failed to get result for %s: %v", reqID, err)
		}
		if result.ID == reqID {
			t.Logf("Request %s completed after endpoint-scrape gate opened", reqID)
			return
		}
		t.Logf("Skipped stale result %s", result.ID)
	}
}

func setSimWaitingRequests(t *testing.T, count int) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"waiting-requests": count})
	if err != nil {
		t.Fatalf("Failed to marshal fake_metrics body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, testSimURL+"/fake_metrics", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Failed to build fake_metrics request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to set fake_metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("fake_metrics returned %d", resp.StatusCode)
	}
}

func testDispatcherPrometheusGate(t *testing.T, rdb *redis.Client) {
	ctx := context.Background()

	p := newDispatcherProducer(t, rdb, promPool)

	// Saturate the sim — gate should close
	// (query: 1 - clamp_max(vllm:num_requests_waiting / 5, 1) → 0 when waiting ≥ 5)
	setSimWaitingRequests(t, 10)
	defer setSimWaitingRequests(t, 0)
	t.Log("Sim saturated (waiting-requests=10, gate should close)")

	// Give Prometheus time to scrape the new metric value
	time.Sleep(10 * time.Second)

	// Enqueue a request — it should stay in the queue (gate closed)
	reqID := fmt.Sprintf("prom-gate-%s", testRunID)
	err := p.SubmitRequest(ctx, &asyncapi.RequestMessage{
		ID:       reqID,
		Created:  time.Now().Unix(),
		Deadline: time.Now().Add(5 * time.Minute).Unix(),
		Payload:  map[string]any{"model": testModel, "prompt": "Hello prom gate", "max_tokens": 5},
		Endpoint: "/v1/completions",
	})
	if err != nil {
		t.Fatalf("Failed to submit request: %v", err)
	}
	t.Logf("Enqueued request %s while gate is closed", reqID)

	// Verify no result arrives (gate closed)
	time.Sleep(5 * time.Second)
	queueDepth, _ := rdb.ZCard(ctx, promReqQueue).Result()
	if queueDepth == 0 {
		t.Fatal("Expected request in queue while gate is closed, but queue is empty")
	}
	t.Logf("Confirmed: request stuck in queue (depth=%d, gate closed)", queueDepth)

	// Clear saturation — gate should open
	setSimWaitingRequests(t, 0)
	t.Log("Sim idle (waiting-requests=0, gate should open)")

	// Wait for Prometheus to scrape the updated metric and dispatcher to react
	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("Timed out waiting for dispatcher to drain queue after gate opened")
		default:
		}
		depth, _ := rdb.ZCard(ctx, promReqQueue).Result()
		if depth == 0 {
			break
		}
		time.Sleep(1 * time.Second)
	}

	// Poll result
	pollCtx, pollCancel := context.WithTimeout(ctx, 15*time.Second)
	defer pollCancel()
	for {
		result, err := p.GetResult(pollCtx)
		if err != nil {
			t.Fatalf("Failed to get result for %s: %v", reqID, err)
		}
		if result.ID == reqID {
			t.Logf("Request %s completed after Prometheus gate opened", reqID)
			return
		}
		t.Logf("Skipped stale result %s", result.ID)
	}
}
