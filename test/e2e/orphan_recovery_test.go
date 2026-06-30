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
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

// testOrphanRecovery covers the GC reconciler's orphan detection and recovery
// after a processor hard crash (SIGKILL via --grace-period=0).
//
// Unlike testProcessorGracefulShutdown which tests the SIGTERM -> re-enqueue
// path within the processor itself, this test validates the external safety net:
// the GC reconciler detects jobs stranded in non-terminal states when the
// processor had no chance to clean up.
//
// Requires:
//   - batch-gc running with reconciler enabled and a short interval (60s in dev-deploy)
//   - kubectl available
func testOrphanRecovery(t *testing.T) {
	t.Run("HardCrashInProgress", doTestHardCrashOrphanRecovery)
	t.Run("CancellingOrphan", doTestCancellingOrphanRecovery)
}

// doTestHardCrashOrphanRecovery submits a batch with long-running requests,
// force-kills the processor pod (--grace-period=0, immediate SIGKILL) while the
// batch is in_progress, and verifies the GC reconciler transitions the orphaned
// job to failed.
//
// Timeline:
//  1. Submit batch → wait for in_progress
//  2. kubectl delete pod --grace-period=0 (SIGKILL, no re-enqueue possible)
//  3. Wait for replacement processor pod to become ready
//  4. Reconciler detects orphan (staleness threshold = reconciler interval)
//  5. in_progress + unexpired SLO → reconciler transitions to failed
func doTestHardCrashOrphanRecovery(t *testing.T) {
	t.Helper()

	if !testKubectlAvailable {
		t.Skip("kubectl not available, skipping orphan recovery test")
	}

	// Use enough slow requests to guarantee the batch is still in_progress
	// when the force-kill arrives. Each request takes ~20s (sim-model:
	// 50ms TTFT + 200 tokens * 100ms inter-token). The processor handles
	// requests concurrently, so we need more requests than the concurrency
	// limit to ensure work is still queued at kill time.
	var lines []string
	for i := 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"custom_id":"orphan-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":200,"messages":[{"role":"user","content":"slow %d"}]}}`, i, testModel, i))
	}
	fileID := mustCreateFile(t, fmt.Sprintf("test-orphan-recovery-%s.jsonl", testRunID), strings.Join(lines, "\n"))
	batchID := mustCreateBatch(t, fileID)

	_, _ = waitForBatchStatus(t, batchID, 2*time.Minute, openai.BatchStatusInProgress)
	time.Sleep(2 * time.Second)

	// Force-kill the processor pod with --grace-period=0 (immediate SIGKILL).
	// Unlike the graceful shutdown test (kubectl delete pod without
	// --grace-period=0), the processor gets no chance to catch SIGTERM and
	// re-enqueue in-flight jobs. The job remains in_progress with no
	// processor working on it.
	t.Log("force-killing processor pod (--grace-period=0)...")
	out, err := exec.Command("kubectl", "delete", "pod",
		"-l", fmt.Sprintf("app.kubernetes.io/instance=%s,app.kubernetes.io/component=processor", testHelmRelease),
		"-n", testNamespace,
		"--grace-period=0", "--force",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl delete pod --grace-period=0 failed: %v\n%s", err, out)
	}
	t.Logf("processor pod force-killed: %s", strings.TrimSpace(string(out)))

	// Wait for the replacement pod via kubectl wait instead of port-forward.
	// After --grace-period=0, the service endpoints may be stale for a few
	// seconds, causing port-forward-based waitForProcessorReady to hang.
	t.Log("waiting for replacement processor pod...")
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer waitCancel()
	waitOut, waitErr := exec.CommandContext(waitCtx, "kubectl", "wait",
		"--for=condition=Ready",
		"pod",
		"-l", fmt.Sprintf("app.kubernetes.io/instance=%s,app.kubernetes.io/component=processor", testHelmRelease),
		"-n", testNamespace,
		"--timeout=120s",
	).CombinedOutput()
	if waitErr != nil {
		t.Fatalf("waiting for replacement processor pod: %v\n%s", waitErr, waitOut)
	}
	t.Logf("replacement processor pod ready: %s", strings.TrimSpace(string(waitOut)))

	// Wait for the reconciler to detect the orphan and transition it to failed.
	// With reconciler interval=60s (dev-deploy):
	//   - Staleness threshold: 60s after last heartbeat
	//   - Next cycle: up to 60s after staleness
	//   - Total: ~2m + buffer
	// The reconciler runs in batch-gc, not the processor, so killing the
	// processor does not affect reconciler cycles.
	//
	// Use waitForOrphanTerminal instead of waitForBatchStatus because the
	// reconciler preserves whatever request counts existed at crash time
	// (Completed+Failed != Total) and does not upload output/error files,
	// which validateTerminalBatch/validateBatchResults would reject.
	finalBatch := waitForOrphanTerminal(t, batchID, 5*time.Minute, openai.BatchStatusFailed)

	t.Logf("orphan recovery: batch %s reached %s", batchID, finalBatch.Status)
}

// doTestCancellingOrphanRecovery submits a batch, waits for it to reach
// in_progress, cancels it (status becomes cancelling), then immediately
// force-kills the processor pod so it cannot complete the cancellation.
// The GC reconciler should detect the orphaned cancelling job and transition
// it to cancelled.
//
// Timeline:
//  1. Submit batch → wait for in_progress
//  2. Wait for at least 1 request to complete (deterministic timing)
//  3. Cancel batch → verify cancelling status
//  4. Immediately kubectl delete pod --grace-period=0 (SIGKILL)
//  5. Wait for replacement processor pod
//  6. Reconciler detects orphan → cancelling transitions to cancelled
func doTestCancellingOrphanRecovery(t *testing.T) {
	t.Helper()

	if !testKubectlAvailable {
		t.Skip("kubectl not available, skipping cancelling orphan recovery test")
	}

	// Mix fast and slow requests (same pattern as doTestBatchCancel):
	//   - Fast requests (max_tokens=1): complete in ~150ms, giving us a
	//     deterministic signal to proceed with cancel.
	//   - Slow requests (max_tokens=200): take ~20s each, ensuring
	//     work is still in-flight when we cancel and kill.
	var lines []string
	for i := 1; i <= 5; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"custom_id":"cancel-orphan-fast-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":1,"messages":[{"role":"user","content":"Hi %d"}]}}`, i, testModel, i))
	}
	for i := 1; i <= 20; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"custom_id":"cancel-orphan-slow-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":200,"messages":[{"role":"user","content":"slow %d"}]}}`, i, testModel, i))
	}
	fileID := mustCreateFile(t, fmt.Sprintf("test-cancelling-orphan-%s.jsonl", testRunID), strings.Join(lines, "\n"))
	batchID := mustCreateBatch(t, fileID)

	_, _ = waitForBatchStatus(t, batchID, 2*time.Minute, openai.BatchStatusInProgress)

	// Wait for at least one fast request to complete before cancelling,
	// ensuring the batch is fully in_progress and processing.
	waitForCompletedRequests(t, batchID, 1, 2*time.Minute)

	// Cancel the batch — apiserver writes cancelling to DB first, then
	// sends the cancel event to the processor.
	client := newClient()
	batch, err := client.Batches.Cancel(context.Background(), batchID)
	if err != nil {
		t.Fatalf("cancel batch failed: %v", err)
	}
	if batch.Status != openai.BatchStatusCancelling {
		t.Fatalf("expected cancelling after cancel call, got %s", batch.Status)
	}
	t.Logf("batch %s is now cancelling", batchID)

	// Immediately force-kill the processor pod so it cannot complete the
	// cancel handling (handleCancelled → partial upload → cancelled).
	t.Log("force-killing processor pod (--grace-period=0)...")
	out, err := exec.Command("kubectl", "delete", "pod",
		"-l", fmt.Sprintf("app.kubernetes.io/instance=%s,app.kubernetes.io/component=processor", testHelmRelease),
		"-n", testNamespace,
		"--grace-period=0", "--force",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl delete pod --grace-period=0 failed: %v\n%s", err, out)
	}
	t.Logf("processor pod force-killed: %s", strings.TrimSpace(string(out)))

	// Wait for the replacement pod.
	t.Log("waiting for replacement processor pod...")
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer waitCancel()
	waitOut, waitErr := exec.CommandContext(waitCtx, "kubectl", "wait",
		"--for=condition=Ready",
		"pod",
		"-l", fmt.Sprintf("app.kubernetes.io/instance=%s,app.kubernetes.io/component=processor", testHelmRelease),
		"-n", testNamespace,
		"--timeout=120s",
	).CombinedOutput()
	if waitErr != nil {
		t.Fatalf("waiting for replacement processor pod: %v\n%s", waitErr, waitOut)
	}
	t.Logf("replacement processor pod ready: %s", strings.TrimSpace(string(waitOut)))

	// Wait for the reconciler to detect the orphan and transition
	// cancelling → cancelled. Use waitForOrphanTerminal because the
	// reconciler does not upload partial results or update request counts.
	finalBatch := waitForOrphanTerminal(t, batchID, 5*time.Minute, openai.BatchStatusCancelled)

	t.Logf("cancelling orphan recovery: batch %s reached %s (cancelled_at=%d)",
		batchID, finalBatch.Status, finalBatch.CancelledAt)

	if finalBatch.CancelledAt == 0 {
		t.Error("expected cancelled_at to be set")
	}
}
