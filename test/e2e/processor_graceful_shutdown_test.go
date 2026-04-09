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

// testProcessorGracefulShutdown covers the processor's SIGTERM -> context.Canceled
// -> re-enqueue path using two different Kubernetes triggers:
//   - PodDeleteMidJob: kubectl delete pod (standard termination grace period)
//   - RollingRestartReEnqueue: kubectl rollout restart
//
// Both deliver SIGTERM and wait terminationGracePeriodSeconds (60s) before
// SIGKILL, giving the processor ample time to re-enqueue in-flight jobs.
// Neither test covers recoverStaleJobs (workdir scan on startup) or true
// hard-crash (SIGKILL-only) scenarios.
func testProcessorGracefulShutdown(t *testing.T) {
	t.Run("PodDeleteMidJob", doTestPodDeleteMidJob)
	t.Run("RollingRestartReEnqueue", doTestRollingRestartReEnqueue)
}

// doTestPodDeleteMidJob submits a batch with long-running requests
// (max_tokens=200 on testModel; dev-deploy's default sim-model uses ~50ms TTFT
// and ~100ms inter-token latency), deletes the processor pod mid-execution, and
// verifies the batch completes after a replacement pod comes up.
//
// kubectl delete pod sends SIGTERM and respects the pod's
// terminationGracePeriodSeconds (60s). The processor catches SIGTERM via
// interrupt.ContextWithSignal, cancels the polling context, and in-flight
// workers re-enqueue the job via a detached context (see Processor.handleJobError
// in job_runner.go). A new pod dequeues and reprocesses the job from scratch
// (no checkpoint/resume).
func doTestPodDeleteMidJob(t *testing.T) {
	t.Helper()

	if !testKubectlAvailable {
		t.Skip("kubectl not available, skipping processor pod-delete test")
	}

	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"custom_id":"pod-del-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":200,"messages":[{"role":"user","content":"slow %d"}]}}`, i, testModel, i))
	}
	fileID := mustCreateFile(t, fmt.Sprintf("test-pod-delete-%s.jsonl", testRunID), strings.Join(lines, "\n"))
	batchID := mustCreateBatch(t, fileID)

	// Wait for in_progress so the processor has picked up the job.
	_, _ = waitForBatchStatus(t, batchID, 2*time.Minute, openai.BatchStatusInProgress)
	time.Sleep(2 * time.Second)

	// Delete the processor pod. Kubelet delivers SIGTERM and waits
	// terminationGracePeriodSeconds (60s) before SIGKILL.
	t.Log("deleting processor pod...")
	out, err := exec.Command("kubectl", "delete", "pod",
		"-l", fmt.Sprintf("app.kubernetes.io/instance=%s,app.kubernetes.io/component=processor", testHelmRelease),
		"-n", testNamespace,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl delete pod failed: %v\n%s", err, out)
	}
	t.Logf("processor pod delete issued: %s", strings.TrimSpace(string(out)))

	// Wait for the new pod to become ready.
	waitForReady(t, testProcessorObsURL, 2*time.Minute)
	t.Log("new processor pod is ready")

	// The re-enqueue path should succeed reliably with the full grace period.
	// failed is accepted in waitForBatchStatus to avoid a fatal on unlikely
	// edge cases (e.g. transient Redis error), but we expect completed.
	finalBatch, _ := waitForBatchStatus(t, batchID, 5*time.Minute,
		openai.BatchStatusCompleted, openai.BatchStatusFailed)

	t.Logf("pod delete: batch %s reached %s (completed=%d, failed=%d, total=%d)",
		batchID, finalBatch.Status,
		finalBatch.RequestCounts.Completed,
		finalBatch.RequestCounts.Failed,
		finalBatch.RequestCounts.Total)

	if finalBatch.Status != openai.BatchStatusCompleted {
		t.Errorf("expected batch to complete after pod delete, got %s", finalBatch.Status)
	}
}

// doTestRollingRestartReEnqueue submits a batch with the same slow-request
// pattern as doTestPodDeleteMidJob, triggers a rolling restart of the processor
// deployment, and verifies the batch eventually completes. Same SIGTERM ->
// re-enqueue path, different trigger.
func doTestRollingRestartReEnqueue(t *testing.T) {
	t.Helper()

	if !testKubectlAvailable {
		t.Skip("kubectl not available, skipping rolling restart test")
	}

	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"custom_id":"restart-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":200,"messages":[{"role":"user","content":"slow %d"}]}}`, i, testModel, i))
	}
	fileID := mustCreateFile(t, fmt.Sprintf("test-rolling-restart-%s.jsonl", testRunID), strings.Join(lines, "\n"))
	batchID := mustCreateBatch(t, fileID)

	_, _ = waitForBatchStatus(t, batchID, 2*time.Minute, openai.BatchStatusInProgress)
	time.Sleep(2 * time.Second)

	// Trigger a rolling restart (graceful shutdown via SIGTERM).
	deployment := fmt.Sprintf("%s-processor", testHelmRelease)
	t.Logf("triggering rolling restart of %s...", deployment)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", "rollout", "restart",
		fmt.Sprintf("deployment/%s", deployment),
		"-n", testNamespace,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl rollout restart failed: %v\n%s", err, out)
	}
	t.Logf("rollout restart triggered: %s", strings.TrimSpace(string(out)))

	// Wait for rollout to complete.
	waitForRollout(t, deployment)
	waitForReady(t, testProcessorObsURL, 2*time.Minute)
	t.Log("processor rollout complete and ready")

	// Same re-enqueue path as PodDeleteMidJob. completed is expected; failed
	// is accepted in waitForBatchStatus to avoid a fatal on unlikely edge
	// cases, but we assert completed below.
	finalBatch, _ := waitForBatchStatus(t, batchID, 5*time.Minute,
		openai.BatchStatusCompleted, openai.BatchStatusFailed)

	t.Logf("rolling restart: batch %s reached %s (completed=%d, failed=%d, total=%d)",
		batchID, finalBatch.Status,
		finalBatch.RequestCounts.Completed,
		finalBatch.RequestCounts.Failed,
		finalBatch.RequestCounts.Total)

	if finalBatch.Status != openai.BatchStatusCompleted {
		t.Errorf("expected batch to complete after rolling restart, got %s", finalBatch.Status)
	}
}
