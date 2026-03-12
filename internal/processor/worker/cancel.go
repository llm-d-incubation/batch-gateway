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
	"sync"
	"sync/atomic"

	"k8s.io/klog/v2"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

func (p *Processor) watchCancel(
	ctx context.Context,
	eventWatcher *db.BatchEventsChan,
	updater *StatusUpdater,
	jobItem *db.BatchItem,
	cancelRequested *atomic.Bool,
	cancellingOnce *sync.Once,
) {
	logger := klog.FromContext(ctx)
	for {
		select {
		case <-ctx.Done():
			logger.V(logging.DEBUG).Info("watchCancel: context done")
			return

		case event, ok := <-eventWatcher.Events:
			if !ok {
				logger.V(logging.DEBUG).Info("watchCancel: event channel closed")
				return
			}

			if event.Type == db.BatchEventCancel {
				logger.V(logging.INFO).Info("watchCancel: cancel event received")

				// signal
				cancelRequested.Store(true)

				// update status to cancelling
				cancellingOnce.Do(func() {
					err := updater.UpdatePersistentStatus(
						ctx,
						jobItem,
						openai.BatchStatusCancelling,
						nil,
						nil,
					)
					if err != nil {
						logger.V(logging.ERROR).Error(err, "Failed to update status to cancelling in DB")
					}
				})
			}
		}
	}
}

// handleCancelled finalizes a user-cancelled job.
// When called after executeJob (execution), requestCounts and jobInfo are non-nil and partial
// results are uploaded. When called before executeJob (ingestion), both are nil and only
// cleanup + status transition is performed.
func (p *Processor) handleCancelled(
	ctx context.Context,
	updater *StatusUpdater,
	jobItem *db.BatchItem,
	jobInfo *batch_types.JobInfo,
	requestCounts *openai.BatchRequestCounts,
) error {
	logger := klog.FromContext(ctx)

	var outputFileID, errorFileID string
	if requestCounts != nil && jobInfo != nil {
		// upload partial results
		logger.V(logging.INFO).Info("Job cancelled mid-execution, uploading partial results")
		outputFileID, errorFileID = p.uploadPartialResults(ctx, jobInfo, jobItem)
	}

	p.cleanupJobArtifacts(ctx, jobItem.ID, jobItem.TenantID)

	if err := updater.UpdateCancelledStatus(ctx, jobItem, requestCounts, outputFileID, errorFileID); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to update status to cancelled")
		return err
	}

	setRequestCountAttrs(ctx, requestCounts)

	// record processed metrics as success because we successfully finished user-initiated cancellation
	metrics.RecordJobProcessed(metrics.ResultSuccess, metrics.ReasonNone)
	logger.V(logging.INFO).Info("Job cancelled handled", "outputFileID", outputFileID, "errorFileID", errorFileID)
	return nil
}
