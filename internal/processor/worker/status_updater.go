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

// this file contains the status updater logic for the processor
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/batch_utils"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	"k8s.io/klog/v2"

	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

type StatusUpdater struct {
	db             db.BatchDBClient
	status         db.BatchStatusClient
	progressTTLSec int
}

func NewStatusUpdater(db db.BatchDBClient, status db.BatchStatusClient, progressTTLSec int) *StatusUpdater {
	return &StatusUpdater{
		db:             db,
		status:         status,
		progressTTLSec: progressTTLSec,
	}
}

// UpdateProgressCounts: frequent light payload update for request counts
func (s *StatusUpdater) UpdateProgressCounts(
	ctx context.Context,
	jobID string,
	requestCounts *openai.BatchRequestCounts,
) error {
	if requestCounts == nil {
		return fmt.Errorf("requestCounts is nil")
	}

	// light payload for frequent updates
	payload := []byte(fmt.Sprintf(`{"total": %d, "completed": %d, "failed": %d}`, requestCounts.Total, requestCounts.Completed, requestCounts.Failed))

	if err := s.status.StatusSet(ctx, jobID, s.progressTTLSec, payload); err != nil {
		return err
	}
	return nil
}

// UpdatePersistentStatus updates the persistent status of the job in DB.
// Optional modifiers are applied to the status info before marshaling.
// tenant ID(tenantId) and job ID(jobId) should be in the logger in the context
func (s *StatusUpdater) UpdatePersistentStatus(
	ctx context.Context,
	dbJob *db.BatchItem,
	newStatus openai.BatchStatus,
	counts *openai.BatchRequestCounts,
	slo *time.Time,
	modifiers ...func(*openai.BatchStatusInfo),
) error {
	if dbJob == nil {
		return fmt.Errorf("dbJob is nil")
	}
	if len(dbJob.Status) == 0 {
		return fmt.Errorf("dbJob.Status is empty")
	}

	logger := klog.FromContext(ctx)

	var original openai.BatchStatusInfo
	if err := json.Unmarshal(dbJob.Status, &original); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to unmarshal batch status")
		return err
	}

	updated, err := batch_utils.BuildUpdatedStatusInfo(&original, newStatus, counts, slo)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to build updated batch status")
		return err
	}

	for _, fn := range modifiers {
		fn(updated)
	}

	statusBytes, err := json.Marshal(updated)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to marshal updated batch status")
		return err
	}

	if err := s.db.DBUpdate(ctx, &db.BatchItem{
		BaseIndexes: db.BaseIndexes{
			ID: dbJob.ID,
		},
		BaseContents: db.BaseContents{
			Status: statusBytes,
		},
	}); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to update batch status in DB")
		return err
	}
	logger.V(logging.INFO).Info("Batch status updated successfully", "newStatus", newStatus)
	return nil
}

// UpdateCompletedStatus transitions the job to completed and sets the output and error file IDs.
// Per the OpenAI batch spec, both IDs are optional: outputFileID is empty when all requests
// failed, and errorFileID is empty when no requests failed.
func (s *StatusUpdater) UpdateCompletedStatus(
	ctx context.Context,
	dbJob *db.BatchItem,
	counts *openai.BatchRequestCounts,
	outputFileID string,
	errorFileID string,
) error {
	return s.UpdatePersistentStatus(ctx, dbJob, openai.BatchStatusCompleted, counts, nil,
		func(info *openai.BatchStatusInfo) {
			if outputFileID != "" {
				info.OutputFileID = outputFileID
			}
			if errorFileID != "" {
				info.ErrorFileID = errorFileID
			}
		},
	)
}
