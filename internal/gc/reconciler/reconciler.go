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

// Package reconciler detects and recovers orphaned batch jobs that are stuck
// in non-terminal states because their processor crashed or lost connectivity.
package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/batch_utils"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
)

const pageSize = 100

// Result contains the outcome of a single reconciliation cycle.
type Result struct {
	Cancelled    int
	Expired      int
	ReEnqueued   int
	Failed       int
	StaleCleanup int
	Conflicts    int
	Errors       int
	Duration     time.Duration
}

// Reconciler periodically scans for orphaned batch jobs and recovers them.
type Reconciler struct {
	batchDB         db.BatchDBClient
	queue           db.BatchPriorityQueueClient
	inflight        db.InFlightClient
	interval        time.Duration
	dryRun          bool
	onCycleComplete func(*Result)
}

// NewReconciler creates a new orphan reconciler.
// interval controls both the scan frequency and the staleness threshold for in-flight entries.
// onCycleComplete, if non-nil, is called after each cycle with the result.
func NewReconciler(
	batchDB db.BatchDBClient,
	queue db.BatchPriorityQueueClient,
	inflight db.InFlightClient,
	interval time.Duration,
	dryRun bool,
	onCycleComplete func(*Result),
) (*Reconciler, error) {
	if batchDB == nil {
		return nil, fmt.Errorf("batchDB client is required")
	}
	if queue == nil {
		return nil, fmt.Errorf("queue client is required")
	}
	if inflight == nil {
		return nil, fmt.Errorf("in-flight client is required")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("interval must be positive, got %v", interval)
	}
	return &Reconciler{
		batchDB:         batchDB,
		queue:           queue,
		inflight:        inflight,
		interval:        interval,
		dryRun:          dryRun,
		onCycleComplete: onCycleComplete,
	}, nil
}

// RunLoop runs the reconciler in a continuous loop at the configured interval.
// It blocks until the context is cancelled.
func (r *Reconciler) RunLoop(ctx context.Context) error {
	logger := logr.FromContextOrDiscard(ctx)
	logger.Info("Reconciler: starting loop", "interval", r.interval)

	r.run(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("Reconciler: loop stopped")
			return ctx.Err()
		case <-ticker.C:
			r.run(ctx)
		}
	}
}

// run executes a single reconciliation cycle.
func (r *Reconciler) run(ctx context.Context) {
	logger := logr.FromContextOrDiscard(ctx)
	start := time.Now()
	result := &Result{}

	defer func() {
		result.Duration = time.Since(start)
		logger.Info("Reconciler: cycle completed",
			"cancelled", result.Cancelled,
			"expired", result.Expired,
			"reEnqueued", result.ReEnqueued,
			"failed", result.Failed,
			"staleCleanup", result.StaleCleanup,
			"conflicts", result.Conflicts,
			"errors", result.Errors,
			"duration", result.Duration,
		)
		r.notifyCycle(result)
	}()

	jobs, err := r.fetchNonTerminalJobs(ctx)
	if err != nil {
		logger.Error(err, "Reconciler: failed to fetch non-terminal jobs")
		result.Errors++
		return
	}

	inflightEntries, err := r.inflight.InFlightGetAll(ctx)
	if err != nil {
		logger.Error(err, "Reconciler: failed to get in-flight entries")
		result.Errors++
		return
	}

	nonTerminalIDs := make(map[string]bool, len(jobs))

	if len(jobs) > 0 {
		queuedIDs, err := r.queue.PQGetIDs(ctx)
		if err != nil {
			logger.Error(err, "Reconciler: failed to get queued job IDs")
			result.Errors++
			return
		}

		now := time.Now()
		stalenessThreshold := now.Add(-r.interval)

		for _, job := range jobs {
			nonTerminalIDs[job.ID] = true

			if queuedIDs[job.ID] {
				continue
			}

			if entry, ok := inflightEntries[job.ID]; ok {
				lastSeen := time.Unix(entry.LastSeen, 0)
				if lastSeen.After(stalenessThreshold) {
					continue
				}
			}

			r.triageOrphan(ctx, job, result)
		}
	}

	r.cleanupStaleInflight(ctx, inflightEntries, nonTerminalIDs, result)
}

func (r *Reconciler) notifyCycle(result *Result) {
	if r.onCycleComplete != nil {
		r.onCycleComplete(result)
	}
}

// fetchNonTerminalJobs retrieves all non-terminal batch jobs via paginated queries.
func (r *Reconciler) fetchNonTerminalJobs(ctx context.Context) ([]*db.BatchItem, error) {
	query := &db.BatchQuery{NonTerminal: true}
	var allJobs []*db.BatchItem
	cursor := 0

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		jobs, nextCursor, expectMore, err := r.batchDB.DBGet(ctx, query, false, cursor, pageSize)
		if err != nil {
			return nil, fmt.Errorf("failed to query non-terminal jobs: %w", err)
		}
		allJobs = append(allJobs, jobs...)

		if !expectMore {
			break
		}
		cursor = nextCursor
	}

	return allJobs, nil
}

// triageOrphan determines the correct recovery action for an orphaned job
// based on its current status and SLO.
func (r *Reconciler) triageOrphan(ctx context.Context, job *db.BatchItem, result *Result) {
	logger := logr.FromContextOrDiscard(ctx).WithValues("jobId", job.ID)

	var statusInfo openai.BatchStatusInfo
	if err := json.Unmarshal(job.Status, &statusInfo); err != nil {
		logger.Error(err, "Reconciler: failed to unmarshal job status")
		result.Errors++
		return
	}

	sloExpired := isSLOExpired(job)

	var ok bool

	switch statusInfo.Status {
	case openai.BatchStatusCancelling:
		ok = r.transitionOrphan(ctx, job, &statusInfo, openai.BatchStatusCancelled, result, logger)

	case openai.BatchStatusValidating:
		if sloExpired {
			ok = r.transitionOrphan(ctx, job, &statusInfo, openai.BatchStatusExpired, result, logger)
		} else {
			ok = r.reEnqueueOrphan(ctx, job, result, logger)
		}

	case openai.BatchStatusInProgress, openai.BatchStatusFinalizing:
		if sloExpired {
			ok = r.transitionOrphan(ctx, job, &statusInfo, openai.BatchStatusExpired, result, logger)
		} else {
			ok = r.transitionOrphan(ctx, job, &statusInfo, openai.BatchStatusFailed, result, logger)
		}

	default:
		logger.Info("Reconciler: orphan in unexpected status, skipping", "status", statusInfo.Status)
		result.Errors++
		return
	}

	if ok && !r.dryRun {
		if err := r.inflight.InFlightDelete(ctx, job.ID); err != nil {
			logger.Error(err, "Reconciler: failed to delete in-flight entry for orphan")
			result.Errors++
		}
	}
}

// transitionOrphan performs a CAS status transition on the orphaned job.
// Returns true if the transition succeeded (or dry-run logged it).
func (r *Reconciler) transitionOrphan(
	ctx context.Context,
	job *db.BatchItem,
	currentStatus *openai.BatchStatusInfo,
	newStatus openai.BatchStatus,
	result *Result,
	logger logr.Logger,
) bool {
	updatedStatus, err := batch_utils.BuildUpdatedStatusInfo(currentStatus, newStatus, nil, nil)
	if err != nil {
		logger.Error(err, "Reconciler: failed to build updated status", "newStatus", newStatus)
		result.Errors++
		return false
	}

	updatedBytes, err := json.Marshal(updatedStatus)
	if err != nil {
		logger.Error(err, "Reconciler: failed to marshal updated status")
		result.Errors++
		return false
	}

	if !r.dryRun {
		updateItem := &db.BatchItem{
			BaseIndexes:  db.BaseIndexes{ID: job.ID},
			BaseContents: db.BaseContents{Status: updatedBytes},
		}
		if err := r.batchDB.DBUpdate(ctx, updateItem, job.Status); err != nil {
			if errors.Is(err, db.ErrConflict) {
				logger.Info("Reconciler: CAS conflict during orphan transition (another actor won the race)", "newStatus", newStatus)
				result.Conflicts++
			} else {
				logger.Error(err, "Reconciler: failed to transition orphan", "newStatus", newStatus)
				result.Errors++
			}
			return false
		}
		logger.Info("Reconciler: orphan transitioned", "from", currentStatus.Status, "to", newStatus)
	} else {
		logger.Info("Reconciler: dry-run: would transition orphan", "from", currentStatus.Status, "to", newStatus)
	}

	switch newStatus {
	case openai.BatchStatusCancelled:
		result.Cancelled++
	case openai.BatchStatusExpired:
		result.Expired++
	case openai.BatchStatusFailed:
		result.Failed++
	}
	return true
}

// reEnqueueOrphan re-enqueues an orphaned validating job with its original SLO.
// Returns true if the re-enqueue succeeded (or dry-run logged it).
func (r *Reconciler) reEnqueueOrphan(
	ctx context.Context,
	job *db.BatchItem,
	result *Result,
	logger logr.Logger,
) bool {
	slo, err := extractSLO(job)
	if err != nil {
		logger.Error(err, "Reconciler: cannot re-enqueue orphan with corrupt SLO")
		result.Errors++
		return false
	}
	if slo == nil {
		logger.Error(fmt.Errorf("missing SLO tag"), "Reconciler: cannot re-enqueue orphan without SLO")
		result.Errors++
		return false
	}

	if r.dryRun {
		logger.Info("Reconciler: dry-run: would re-enqueue orphan", "slo", slo)
		result.ReEnqueued++
		return true
	}

	task := &db.BatchJobPriority{
		ID:  job.ID,
		SLO: *slo,
	}
	if err := r.queue.PQEnqueue(ctx, task); err != nil {
		logger.Error(err, "Reconciler: failed to re-enqueue orphan")
		result.Errors++
		return false
	}

	logger.Info("Reconciler: orphan re-enqueued", "slo", slo)
	result.ReEnqueued++
	return true
}

// cleanupStaleInflight removes in-flight entries for jobs that are no longer
// in the non-terminal set (already completed, failed, or deleted from DB).
func (r *Reconciler) cleanupStaleInflight(
	ctx context.Context,
	inflightEntries map[string]*db.InFlightEntry,
	nonTerminalIDs map[string]bool,
	result *Result,
) {
	logger := logr.FromContextOrDiscard(ctx)

	for jobID := range inflightEntries {
		if nonTerminalIDs[jobID] {
			continue
		}
		if r.dryRun {
			logger.Info("Reconciler: dry-run: would clean up stale in-flight entry", "jobId", jobID)
			result.StaleCleanup++
			continue
		}
		if err := r.inflight.InFlightDelete(ctx, jobID); err != nil {
			logger.Error(err, "Reconciler: failed to clean up stale in-flight entry", "jobId", jobID)
			result.Errors++
			continue
		}
		logger.Info("Reconciler: cleaned up stale in-flight entry", "jobId", jobID)
		result.StaleCleanup++
	}
}

// isSLOExpired checks whether the job's SLO deadline has passed.
// Returns false if the SLO tag is missing or corrupt (caller should check extractSLO separately).
func isSLOExpired(job *db.BatchItem) bool {
	slo, _ := extractSLO(job)
	if slo == nil {
		return false
	}
	return time.Now().After(*slo)
}

// extractSLO parses the SLO tag from the job's tags.
// Returns (nil, nil) if the tag is missing, or (nil, error) if the tag value is corrupt.
func extractSLO(job *db.BatchItem) (*time.Time, error) {
	sloStr, ok := job.Tags[batch_types.TagSLO]
	if !ok {
		return nil, nil
	}
	sloMicro, err := strconv.ParseInt(sloStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("corrupt SLO tag %q: %w", sloStr, err)
	}
	slo := time.UnixMicro(sloMicro).UTC()
	return &slo, nil
}
