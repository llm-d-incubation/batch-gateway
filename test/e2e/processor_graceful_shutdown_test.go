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

// testProcessorGracefulShutdown covers the processor shutting down under Kubernetes
// SIGTERM (pod delete or rollout restart), re-enqueuing the in-flight job, and a
// replacement pod finishing the batch. Not recoverStaleJobs (workdir scan on startup).
func testProcessorGracefulShutdown(t *testing.T) {
	t.Run("PodDeletedMidJob", doTestPodDeletedMidJob)
	t.Run("RollingRestartReEnqueue", doTestRollingRestartReEnqueue)
}

// doTestPodDeletedMidJob submits a batch with long-running requests (max_tokens=200
// on testModel; dev-deploy's default sim-model uses ~50ms TTFT and ~100ms
// inter-token latency), deletes the processor pod mid-execution, and verifies the
// batch reaches a terminal state after a replacement pod comes up.
//
// Pod deletion usually delivers SIGTERM first; the processor then cancels work and
// re-enqueues the job (see Processor.handleJobError in job_runner.go for the
// context.Canceled path). A new pod dequeues and reprocesses the job from scratch
// (no checkpoint/resume). This is not the same as recoverStaleJobs (workdir scan):
// that path needs leftover on-disk job state inside the same pod volume.
func doTestPodDeletedMidJob(t *testing.T) {
	t.Helper()

	if !testKubectlAvailable {
		t.Skip("kubectl not available, skipping processor pod-delete graceful-shutdown test")
	}

	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"custom_id":"pod-del-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":200,"messages":[{"role":"user","content":"slow %d"}]}}`, i, testModel, i))
	}
	fileID := mustCreateFile(t, fmt.Sprintf("test-pod-delete-graceful-%s.jsonl", testRunID), strings.Join(lines, "\n"))
	batchID := mustCreateBatch(t, fileID)

	// Wait for in_progress so the processor has picked up the job.
	_, _ = waitForBatchStatus(t, batchID, 2*time.Minute, openai.BatchStatusInProgress)
	time.Sleep(2 * time.Second)

	// Delete the processor pod. The API typically SIGTERMs the container first (even
	// with --grace-period=0 / --force the window may be short), which triggers the
	// processor's shutdown path and often re-enqueues the in-flight job — unlike an
	// immediate SIGKILL-only "hard crash".
	t.Log("deleting processor pod...")
	out, err := exec.Command("kubectl", "delete", "pod",
		"-l", fmt.Sprintf("app.kubernetes.io/instance=%s,app.kubernetes.io/component=processor", testHelmRelease),
		"-n", testNamespace,
		"--grace-period=0", "--force",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl delete pod failed: %v\n%s", err, out)
	}
	t.Logf("processor pod delete issued: %s", strings.TrimSpace(string(out)))

	// Wait for the new pod to become ready.
	waitForReady(t, testProcessorObsURL, 2*time.Minute)
	t.Log("new processor pod is ready")

	// Expect a terminal status after pod replacement. completed is the usual outcome
	// when SIGTERM allows re-enqueue and the new pod finishes the job. failed is still
	// allowed (e.g. re-enqueue failed, SIGKILL raced shutdown, or later processing error).
	// The key assertion is that the job does NOT stay stuck in in_progress.
	finalBatch, _ := waitForBatchStatus(t, batchID, 5*time.Minute,
		openai.BatchStatusCompleted, openai.BatchStatusFailed)

	t.Logf("pod delete (graceful shutdown path): batch %s reached %s (completed=%d, failed=%d, total=%d)",
		batchID, finalBatch.Status,
		finalBatch.RequestCounts.Completed,
		finalBatch.RequestCounts.Failed,
		finalBatch.RequestCounts.Total)
}

// doTestRollingRestartReEnqueue submits a batch with the same slow-request pattern
// as doTestPodDeletedMidJob, triggers a rolling restart of the processor deployment,
// and verifies the batch eventually completes. Exercises the SIGTERM ->
// context.Canceled -> re-enqueue path.
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

	// Graceful shutdown (SIGTERM) triggers context.Canceled in handleJobError,
	// which re-enqueues the job via detached context. The new pod picks it up
	// from the queue and completes it. Stricter than PodDeletedMidJob: we expect completed.
	// failed is accepted in waitForBatchStatus to avoid a fatal on edge cases
	// (e.g. re-enqueue itself failed), but we assert completed below.
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
