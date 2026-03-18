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
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

func (p *Processor) watchCancel(ctx context.Context, params *jobExecutionParams) {
	logger := klog.FromContext(ctx)
	if params.cancelRequested == nil {
		params.cancelRequested = &atomic.Bool{}
	}
	if params.cancellingOnce == nil {
		params.cancellingOnce = &sync.Once{}
	}
	for {
		select {
		case <-ctx.Done():
			logger.V(logging.DEBUG).Info("watchCancel: context done")
			return

		case event, ok := <-params.eventWatcher.Events:
			if !ok {
				logger.V(logging.DEBUG).Info("watchCancel: event channel closed")
				return
			}

			if event.Type == db.BatchEventCancel {
				logger.V(logging.INFO).Info("watchCancel: cancel event received")

				// signal
				params.cancelRequested.Store(true)

				// cancel the inference context to abort in-flight HTTP requests immediately,
				// freeing downstream resources.
				params.inferCancelFn()

				// Note: We don't update the DB status to 'cancelling' here because
				// the API server already wrote 'cancelling' to the DB before sending this event.
			}
		}
	}
}

// handleCancelled finalizes a user-cancelled job.
// When called after executeJob (execution), requestCounts and jobInfo are non-nil and partial
// results are uploaded. When called before executeJob (ingestion), both are nil and only
// cleanup + status transition is performed.
func (p *Processor) handleCancelled(ctx context.Context, params *jobExecutionParams) error {
	logger := klog.FromContext(ctx)

	var outputFileID, errorFileID string
	if params.requestCounts != nil && params.jobInfo != nil {
		// upload partial results
		logger.V(logging.INFO).Info("Job cancelled mid-execution, uploading partial results")
		outputFileID, errorFileID = p.uploadPartialResults(ctx, params.jobInfo, params.jobItem)
	}

	p.cleanupJobArtifacts(ctx, params.jobItem.ID, params.jobItem.TenantID)

	if err := params.updater.UpdateCancelledStatus(ctx, params.jobItem, params.requestCounts, outputFileID, errorFileID); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to update status to cancelled")
		return err
	}

	setRequestCountAttrs(ctx, params.requestCounts)

	// record processed metrics as success because we successfully finished user-initiated cancellation
	metrics.RecordJobProcessed(metrics.ResultSuccess, metrics.ReasonNone)
	logger.V(logging.INFO).Info("Job cancelled handled", "outputFileID", outputFileID, "errorFileID", errorFileID)
	return nil
}
