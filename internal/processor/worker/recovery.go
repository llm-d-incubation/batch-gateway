/*
Copyright 2026 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/batch_utils"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

const (
	recoveryActionFinalized = "finalized"
	recoveryActionCancelled = "cancelled"
	recoveryActionFailed    = "failed"
	recoveryActionExpired   = "expired"
	recoveryActionReEnqueue = "re_enqueued"
	recoveryActionCleanedUp = "cleaned_up"
	recoveryActionError     = "error"

	recoveryUnknownStatus openai.BatchStatus = "unknown"
)

// recoverStaleJobs scans the workdir for leftover job directories from a previous
// container execution and performs phase-aware recovery for each discovered job.
//
// This handles container-level crashes (OOM kill, process panic) where K8s restarts
// the container within the same pod and emptyDir survives. Pod-level failures
// (node eviction, pod deletion) destroy emptyDir and are out of scope.
//
// Runs once at startup before the polling loop. Individual job recovery failures
// do not prevent the processor from starting.
func (p *Processor) recoverStaleJobs(ctx context.Context) {
	logger := logr.FromContextOrDiscard(ctx)

	dirs, err := p.discoverStaleJobDirs()
	if err != nil {
		logger.Error(err, "Startup recovery: failed to scan workdir")
		return
	}

	if len(dirs) == 0 {
		logger.V(logging.DEBUG).Info("Startup recovery: no stale job directories found")
		return
	}

	logger.V(logging.INFO).Info("Startup recovery: found stale job directories", "count", len(dirs))

	var grp errgroup.Group
	grp.SetLimit(p.cfg.RecoveryMaxConcurrency)

	for _, dir := range dirs {
		jobID := filepath.Base(dir)
		grp.Go(func() error {
			jlogger := logger.WithValues("jobId", jobID)
			jctx := logr.NewContext(ctx, jlogger)
			if err := p.recoverJob(jctx, jobID); err != nil {
				jlogger.Error(err, "Startup recovery: failed to recover job")
			}
			return nil // individual failures shouldn't block other recoveries
		})
	}
	_ = grp.Wait()
}

// discoverStaleJobDirs returns paths to job directories left over from a previous execution.
// The workdir layout is <WorkDir>/<tenantHash>/jobs/<jobID>/.
func (p *Processor) discoverStaleJobDirs() ([]string, error) {
	pattern := filepath.Join(p.cfg.WorkDir, "*", jobsDirName, "*")
	return filepath.Glob(pattern)
}

// recoverJob performs phase-aware recovery for a single job based on its DB status.
func (p *Processor) recoverJob(ctx context.Context, jobID string) error {
	logger := logr.FromContextOrDiscard(ctx)

	dbItem, err := p.poller.fetchJobItemByID(ctx, jobID)
	// DB unreachable — can't read status or mark as failed. Leave workdir on disk so the
	// next container restart retries. If the pod is evicted (emptyDir destroyed), this job
	// becomes an orphan that only an external entity can detect.
	if err != nil {
		logger.Error(err, "Startup recovery: DB lookup failed, skipping (will retry on next restart)")
		metrics.RecordStartupRecovery(string(recoveryUnknownStatus), recoveryActionError)
		return err
	}

	// Job not in DB — can't update status, but we can clean up the stale directory.
	// tenantID is unknown (directory uses SHA256 hash), so we glob for the jobID.
	if dbItem == nil {
		logger.Info("Startup recovery: job not found in DB, cleaning up stale directory")
		metrics.RecordStartupRecovery(string(recoveryUnknownStatus), recoveryActionCleanedUp)
		p.cleanupStaleJobDir(ctx, jobID)
		return nil
	}

	jobInfo, err := batch_utils.FromDBItemToJobInfoObject(dbItem)
	if err != nil {
		logger.Error(err, "Startup recovery: failed to convert DB item")
		return p.recoverWithFailed(ctx, dbItem, err, nil, nil)
	}

	status := jobInfo.BatchJob.Status
	statusStr := string(status)
	logger.V(logging.INFO).Info("Startup recovery: recovering job", "status", statusStr)

	switch status {
	case openai.BatchStatusFinalizing:
		return p.recoverFinalizing(ctx, dbItem, jobInfo)

	case openai.BatchStatusCancelling:
		return p.recoverCancelling(ctx, dbItem, jobInfo)

	case openai.BatchStatusInProgress:
		return p.recoverInProgress(ctx, dbItem, jobInfo)

	case openai.BatchStatusValidating:
		return p.recoverValidating(ctx, dbItem, jobInfo)

	default:
		if status.IsFinal() {
			logger.V(logging.INFO).Info("Startup recovery: job already terminal, cleaning up")
			metrics.RecordStartupRecovery(statusStr, recoveryActionCleanedUp)
			p.cleanupJobArtifacts(ctx, dbItem.ID, dbItem.TenantID)
			return nil
		}
		logger.Info("Startup recovery: unexpected status, marking as failed", "status", statusStr)
		return p.recoverWithFailed(ctx, dbItem, nil, nil, jobInfo)
	}
}

// recoverFinalizing completes a job that crashed during the upload phase.
// Output files should be complete on disk since execution finished before finalizing.
// We upload directly (instead of calling finalizeJob) so that file IDs are available
// for the fallback path if the status update fails.
func (p *Processor) recoverFinalizing(ctx context.Context, dbItem *db.BatchItem, jobInfo *batch_types.JobInfo) error {
	logger := logr.FromContextOrDiscard(ctx)
	counts := p.extractRequestCounts(dbItem)

	outputFileID, errorFileID := p.uploadPartialResults(ctx, jobInfo, dbItem)

	if err := p.updater.UpdateCompletedStatus(ctx, dbItem, counts, outputFileID, errorFileID); err != nil {
		logger.Error(err, "Startup recovery: finalization failed, marking as failed")
		return p.recoverWithFailed(ctx, dbItem, err, &recoveryFallback{
			counts: counts, outputFileID: outputFileID, errorFileID: errorFileID,
		}, jobInfo)
	}

	p.cleanupJobArtifacts(ctx, dbItem.ID, dbItem.TenantID)
	recordE2ELatency(jobInfo, metrics.E2EStatusCompleted)
	metrics.RecordStartupRecovery("finalizing", recoveryActionFinalized)
	logger.V(logging.INFO).Info("Startup recovery: finalized successfully")
	return nil
}

// recoverCancelling completes a cancellation that was interrupted by crash.
func (p *Processor) recoverCancelling(ctx context.Context, dbItem *db.BatchItem, jobInfo *batch_types.JobInfo) error {
	logger := logr.FromContextOrDiscard(ctx)
	counts := p.extractRequestCounts(dbItem)

	outputFileID, errorFileID := p.uploadPartialResults(ctx, jobInfo, dbItem)

	if err := p.updater.UpdateCancelledStatus(ctx, dbItem, counts, outputFileID, errorFileID); err != nil {
		logger.Error(err, "Startup recovery: failed to update cancelled status, marking as failed")
		return p.recoverWithFailed(ctx, dbItem, err, &recoveryFallback{
			counts: counts, outputFileID: outputFileID, errorFileID: errorFileID,
		}, jobInfo)
	}

	p.cleanupJobArtifacts(ctx, dbItem.ID, dbItem.TenantID)
	recordE2ELatency(jobInfo, metrics.E2EStatusCancelled)
	if counts != nil {
		metrics.RecordCancellation(metrics.CancelPhaseInProgress)
	} else {
		metrics.RecordCancellation(metrics.CancelPhaseQueued)
	}
	metrics.RecordStartupRecovery("cancelling", recoveryActionCancelled)
	logger.V(logging.INFO).Info("Startup recovery: cancelled successfully")
	return nil
}

// recoverInProgress handles a job that crashed during inference execution.
// If the output file exists and has non-zero size, inference made meaningful progress
// — upload partial results and mark as failed.
// If output is empty or absent, inference barely started — re-enqueue for retry.
func (p *Processor) recoverInProgress(ctx context.Context, dbItem *db.BatchItem, jobInfo *batch_types.JobInfo) error {
	logger := logr.FromContextOrDiscard(ctx)

	hasOutput, err := p.outputFileHasContent(dbItem.ID, dbItem.TenantID)
	if err != nil {
		logger.Error(err, "Startup recovery: failed to check output file, treating as empty")
	}

	if hasOutput {
		return p.recoverInProgressWithPartial(ctx, dbItem, jobInfo)
	}
	return p.recoverInProgressReEnqueue(ctx, dbItem, jobInfo)
}

func (p *Processor) recoverInProgressWithPartial(ctx context.Context, dbItem *db.BatchItem, jobInfo *batch_types.JobInfo) error {
	logger := logr.FromContextOrDiscard(ctx)
	counts := p.extractRequestCounts(dbItem)

	outputFileID, errorFileID := p.uploadPartialResults(ctx, jobInfo, dbItem)

	if err := p.updater.UpdateFailedStatus(ctx, dbItem, counts, outputFileID, errorFileID); err != nil {
		logger.Error(err, "Startup recovery: failed to update failed status")
		return p.recoverWithFailed(ctx, dbItem, err, &recoveryFallback{
			counts: counts, outputFileID: outputFileID, errorFileID: errorFileID,
		}, jobInfo)
	}

	p.cleanupJobArtifacts(ctx, dbItem.ID, dbItem.TenantID)
	recordE2ELatency(jobInfo, metrics.E2EStatusFailed)
	metrics.RecordStartupRecovery("in_progress", recoveryActionFailed)
	logger.V(logging.INFO).Info("Startup recovery: marked as failed with partial output")
	return nil
}

func (p *Processor) recoverInProgressReEnqueue(ctx context.Context, dbItem *db.BatchItem, jobInfo *batch_types.JobInfo) error {
	logger := logr.FromContextOrDiscard(ctx)

	slo, err := p.extractRecoverySLO(dbItem, jobInfo)
	if err != nil {
		logger.Error(err, "Startup recovery: failed to recover SLO for re-enqueue")
		return p.recoverWithFailed(ctx, dbItem, err, nil, jobInfo)
	}
	if time.Now().After(*slo) {
		expired, err := p.recoverExpired(ctx, dbItem, "in_progress")
		if expired {
			recordE2ELatency(jobInfo, metrics.E2EStatusExpired)
		} else {
			recordE2ELatency(jobInfo, metrics.E2EStatusFailed)
		}
		return err
	}

	if err := p.updater.UpdatePersistentStatus(ctx, dbItem, openai.BatchStatusValidating, nil, slo); err != nil {
		logger.Error(err, "Startup recovery: failed to reset status to validating")
		return p.recoverWithFailed(ctx, dbItem, err, nil, jobInfo)
	}

	task, err := p.buildRecoveryTask(dbItem, slo)
	if err != nil {
		logger.Error(err, "Startup recovery: failed to build recovery task")
		return p.recoverWithFailed(ctx, dbItem, err, nil, jobInfo)
	}
	if err := p.poller.enqueueOne(ctx, task); err != nil {
		logger.Error(err, "Startup recovery: failed to re-enqueue job")
		return p.recoverWithFailed(ctx, dbItem, err, nil, jobInfo)
	}

	p.cleanupJobArtifacts(ctx, dbItem.ID, dbItem.TenantID)
	metrics.RecordStartupRecovery("in_progress", recoveryActionReEnqueue)
	logger.V(logging.INFO).Info("Startup recovery: re-enqueued for retry (no partial output)")
	return nil
}

// recoverValidating re-enqueues a job that crashed before inference started.
func (p *Processor) recoverValidating(ctx context.Context, dbItem *db.BatchItem, jobInfo *batch_types.JobInfo) error {
	logger := logr.FromContextOrDiscard(ctx)

	slo, err := p.extractRecoverySLO(dbItem, jobInfo)
	if err != nil {
		logger.Error(err, "Startup recovery: failed to recover SLO for re-enqueue")
		return p.recoverWithFailed(ctx, dbItem, err, nil, jobInfo)
	}
	if time.Now().After(*slo) {
		expired, err := p.recoverExpired(ctx, dbItem, "validating")
		if expired {
			recordE2ELatency(jobInfo, metrics.E2EStatusExpired)
		} else {
			recordE2ELatency(jobInfo, metrics.E2EStatusFailed)
		}
		return err
	}

	task, err := p.buildRecoveryTask(dbItem, slo)
	if err != nil {
		logger.Error(err, "Startup recovery: failed to build recovery task")
		return p.recoverWithFailed(ctx, dbItem, err, nil, jobInfo)
	}

	if err := p.poller.enqueueOne(ctx, task); err != nil {
		logger.Error(err, "Startup recovery: failed to re-enqueue validating job")
		return p.recoverWithFailed(ctx, dbItem, err, nil, jobInfo)
	}

	p.cleanupJobArtifacts(ctx, dbItem.ID, dbItem.TenantID)
	metrics.RecordStartupRecovery("validating", recoveryActionReEnqueue)
	logger.V(logging.INFO).Info("Startup recovery: re-enqueued validating job")
	return nil
}

// recoveryFallback holds partial results that were already uploaded before the primary
// recovery action failed. Passing them to recoverWithFailed preserves the file references
// and request counts in the terminal failed status, avoiding orphaned uploads.
type recoveryFallback struct {
	counts       *openai.BatchRequestCounts
	outputFileID string
	errorFileID  string
}

// recoverWithFailed is the fallback: mark the job as failed so it doesn't stay stuck.
// Used when the primary recovery action fails and DB is reachable.
// If fb is non-nil, its counts and file IDs are preserved in the failed status.
// Records E2E latency as failed when jobInfo is available (nil-safe).
func (p *Processor) recoverWithFailed(ctx context.Context, dbItem *db.BatchItem, cause error, fb *recoveryFallback, jobInfo *batch_types.JobInfo) error {
	logger := logr.FromContextOrDiscard(ctx)

	var counts *openai.BatchRequestCounts
	var outputFileID, errorFileID string
	if fb != nil {
		counts = fb.counts
		outputFileID = fb.outputFileID
		errorFileID = fb.errorFileID
	}

	if err := p.updater.UpdateFailedStatus(ctx, dbItem, counts, outputFileID, errorFileID); err != nil {
		logger.Error(err, "Startup recovery: failed to mark job as failed (job will remain stuck)")
		return err
	}

	p.cleanupJobArtifacts(ctx, dbItem.ID, dbItem.TenantID)
	recordE2ELatency(jobInfo, metrics.E2EStatusFailed)
	metrics.RecordStartupRecovery(string(p.getJobStatus(dbItem)), recoveryActionFailed)
	logger.Info("Startup recovery: marked as failed (recovery action failed)", "cause", cause)
	return nil
}

// recoverExpired transitions a job to expired status. Returns (true, nil) on success,
// (false, nil) if the expired update failed but recoverWithFailed succeeded (job is now failed),
// or (false, err) if both expired and failed updates failed.
func (p *Processor) recoverExpired(ctx context.Context, dbItem *db.BatchItem, previousStatus string) (expired bool, err error) {
	logger := logr.FromContextOrDiscard(ctx)

	if err := p.updater.UpdatePersistentStatus(ctx, dbItem, openai.BatchStatusExpired, nil, nil); err != nil {
		logger.Error(err, "Startup recovery: failed to update expired status")
		return false, p.recoverWithFailed(ctx, dbItem, err, nil, nil)
	}

	p.cleanupJobArtifacts(ctx, dbItem.ID, dbItem.TenantID)
	metrics.RecordStartupRecovery(previousStatus, recoveryActionExpired)
	logger.V(logging.INFO).Info("Startup recovery: marked job as expired because SLO already passed")
	return true, nil
}

// outputFileHasContent checks whether the output.jsonl file exists and has content.
func (p *Processor) outputFileHasContent(jobID, tenantID string) (bool, error) {
	outputPath, err := p.jobOutputFilePath(jobID, tenantID)
	if err != nil {
		return false, err
	}
	stat, err := os.Stat(outputPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return stat.Size() > 0, nil
}

// extractRequestCounts parses RequestCounts from the DB status JSON.
func (p *Processor) extractRequestCounts(dbItem *db.BatchItem) *openai.BatchRequestCounts {
	if len(dbItem.Status) == 0 {
		return nil
	}
	var info openai.BatchStatusInfo
	if err := json.Unmarshal(dbItem.Status, &info); err != nil {
		return nil
	}
	if info.RequestCounts.Total == 0 {
		return nil
	}
	return &info.RequestCounts
}

// extractRecoverySLO recovers the exact SLO deadline for queue re-enqueue.
// Prefer the stored microsecond tag so later CancelBatch can reconstruct the same queue score.
func (p *Processor) extractRecoverySLO(dbItem *db.BatchItem, jobInfo *batch_types.JobInfo) (*time.Time, error) {
	if dbItem != nil {
		if sloStr, ok := dbItem.Tags[batch_types.TagSLO]; ok {
			sloMicro, err := strconv.ParseInt(sloStr, 10, 64)
			if err == nil {
				slo := time.UnixMicro(sloMicro).UTC()
				return &slo, nil
			}
		}
	}

	if jobInfo.BatchJob.ExpiresAt != nil {
		slo := time.Unix(*jobInfo.BatchJob.ExpiresAt, 0).UTC()
		return &slo, nil
	}

	return nil, fmt.Errorf("missing recovery SLO for job %s", dbItem.ID)
}

// buildRecoveryTask constructs a BatchJobPriority for re-enqueue.
func (p *Processor) buildRecoveryTask(dbItem *db.BatchItem, slo *time.Time) (*db.BatchJobPriority, error) {
	if slo == nil || slo.IsZero() {
		return nil, fmt.Errorf("missing recovery SLO for job %s", dbItem.ID)
	}

	task := &db.BatchJobPriority{
		ID:  dbItem.ID,
		SLO: slo.UTC(),
	}
	return task, nil
}

// getJobStatus parses the status from a BatchItem's Status JSON.
func (p *Processor) getJobStatus(dbItem *db.BatchItem) openai.BatchStatus {
	if len(dbItem.Status) == 0 {
		return recoveryUnknownStatus
	}
	var info openai.BatchStatusInfo
	if err := json.Unmarshal(dbItem.Status, &info); err != nil {
		return recoveryUnknownStatus
	}
	return info.Status
}

// cleanupStaleJobDir removes a job directory when tenantID is unknown (job not in DB).
// Scans all tenant hash directories under workdir to find the matching jobID.
func (p *Processor) cleanupStaleJobDir(ctx context.Context, jobID string) {
	logger := logr.FromContextOrDiscard(ctx)
	pattern := filepath.Join(p.cfg.WorkDir, "*", jobsDirName, jobID)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		logger.Error(err, "Failed to glob for stale job directory")
		return
	}
	for _, dir := range matches {
		if err := os.RemoveAll(dir); err != nil {
			logger.Error(err, "Failed to remove stale job directory", "path", dir)
		} else {
			logger.V(logging.INFO).Info("Removed stale job directory", "path", dir)
		}
	}
}
