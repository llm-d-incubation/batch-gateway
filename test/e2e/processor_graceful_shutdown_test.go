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

// testProcessorGracefulShutdown covers the processor's behavior on SIGTERM:
// the processor exits without re-enqueueing, and the GC reconciler detects
// the orphaned job and transitions it to a terminal state.
//
//   - PodDeleteMidJob: kubectl delete pod (standard termination grace period)
//   - RollingRestartOrphan: kubectl rollout restart
//
// Both deliver SIGTERM. The processor catches SIGTERM via
// interrupt.ContextWithSignal, cancels the polling context, and the
// errShutdown handler leaves the job in its current state. The GC
// reconciler then detects the stranded job and marks it failed.
func testProcessorGracefulShutdown(t *testing.T) {
	t.Run("PodDeleteMidJob", doTestPodDeleteMidJob)
	t.Run("RollingRestartOrphan", doTestRollingRestartReEnqueue)
}

// doTestPodDeleteMidJob submits a batch with long-running requests
// (max_tokens=200 on testModel; dev-deploy's default sim-model uses ~50ms TTFT
// and ~100ms inter-token latency), deletes the processor pod mid-execution, and
// verifies the GC reconciler transitions the orphaned job to failed.
//
// kubectl delete pod sends SIGTERM and respects the pod's
// terminationGracePeriodSeconds (60s). The processor catches SIGTERM via
// interrupt.ContextWithSignal, cancels the polling context, and the
// errShutdown handler exits without re-enqueueing. The job stays in_progress
// in the DB with no queue entry. The GC reconciler detects this orphan on
// its next cycle and transitions the job to failed.
func doTestPodDeleteMidJob(t *testing.T) {
	t.Helper()

	if !testKubectlAvailable {
		t.Skip("kubectl not available, skipping processor pod-delete test")
	}

	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"custom_id":"pod-del-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":200,"messages":[{"role":"user","content":"slow %d"}]}}`, i, testSimModel, i))
	}
	fileID := mustCreateFile(t, fmt.Sprintf("test-pod-delete-%s.jsonl", testRunID), strings.Join(lines, "\n"))
	batchID := mustCreateBatch(t, fileID)

	_, _ = waitForBatchStatus(t, batchID, 2*time.Minute, openai.BatchStatusInProgress)
	time.Sleep(2 * time.Second)

	t.Log("deleting processor pod...")
	out, err := exec.Command("kubectl", "delete", "pod",
		"-l", fmt.Sprintf("app.kubernetes.io/instance=%s,app.kubernetes.io/component=processor", testHelmRelease),
		"-n", testNamespace,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl delete pod failed: %v\n%s", err, out)
	}
	t.Logf("processor pod delete issued: %s", strings.TrimSpace(string(out)))

	waitForProcessorReady(t, 2*time.Minute)
	t.Log("new processor pod is ready")

	// The orphan reconciler detects the stranded in_progress job (not in
	// queue, stale or missing in-flight entry) and transitions it to failed.
	// Use waitForOrphanTerminal because the reconciler's transition preserves
	// whatever request counts existed at crash time and does not upload files.
	finalBatch := waitForOrphanTerminal(t, batchID, 5*time.Minute, openai.BatchStatusFailed)

	t.Logf("pod delete: batch %s reached %s (completed=%d, failed=%d, total=%d)",
		batchID, finalBatch.Status,
		finalBatch.RequestCounts.Completed,
		finalBatch.RequestCounts.Failed,
		finalBatch.RequestCounts.Total)
}

// doTestRollingRestartReEnqueue submits a batch, triggers a rolling restart of
// the processor deployment, and verifies the GC reconciler transitions the
// orphaned job to failed. Same SIGTERM -> orphan -> reconciler path as
// PodDeleteMidJob, different trigger.
//
// Rolling restart delivers SIGTERM only after the new pod is Ready (~12s).
// To guarantee requests are still in-flight when SIGTERM arrives, we set the
// simulator's inter-token latency to 30s via /admin/config. This makes even a
// single generated token take longer than the new-pod startup delay, eliminating
// the race between request completion and SIGTERM delivery.
func doTestRollingRestartReEnqueue(t *testing.T) {
	t.Helper()

	if !testKubectlAvailable {
		t.Skip("kubectl not available, skipping rolling restart test")
	}

	// Slow down the simulator so requests cannot complete before SIGTERM arrives.
	setSimAdminConfig(t, testSimService, `{"inter-token-latency":"30s"}`)
	t.Cleanup(func() {
		if err := trySetSimAdminConfig(t, testSimService, `{"inter-token-latency":"100ms"}`); err != nil {
			t.Errorf("cleanup: failed to restore %s inter-token-latency: %v", testSimService, err)
		}
	})

	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"custom_id":"restart-%d","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":200,"messages":[{"role":"user","content":"slow %d"}]}}`, i, testSimModel, i))
	}
	fileID := mustCreateFile(t, fmt.Sprintf("test-rolling-restart-%s.jsonl", testRunID), strings.Join(lines, "\n"))
	batchID := mustCreateBatch(t, fileID)

	_, _ = waitForBatchStatus(t, batchID, 2*time.Minute, openai.BatchStatusInProgress)
	time.Sleep(2 * time.Second)

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

	waitForRollout(t, deployment)
	waitForProcessorReady(t, 2*time.Minute)
	t.Log("processor rollout complete and ready")

	// Same reconciler path as PodDeleteMidJob: the orphaned job is detected
	// and transitioned to failed.
	finalBatch := waitForOrphanTerminal(t, batchID, 5*time.Minute, openai.BatchStatusFailed)

	t.Logf("rolling restart: batch %s reached %s (completed=%d, failed=%d, total=%d)",
		batchID, finalBatch.Status,
		finalBatch.RequestCounts.Completed,
		finalBatch.RequestCounts.Failed,
		finalBatch.RequestCounts.Total)
}
