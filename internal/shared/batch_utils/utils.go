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

// this file contains the utility functions for the batch object
package batch_utils

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/converter"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
)

// FromDBItemToJobInfoObject: convert db item to Processor's JobInfo object
func FromDBItemToJobInfoObject(job *db.BatchItem) (*batch_types.JobInfo, error) {
	jobInfo := &batch_types.JobInfo{
		JobID:    job.ID,
		BatchJob: &openai.Batch{},
	}

	batchJob, err := converter.DBItemToBatch(job)
	if err != nil {
		return nil, err
	}

	jobInfo.BatchJob = batchJob

	for _, tag := range job.Tags {
		if strings.HasPrefix(tag, "tenant:") {
			jobInfo.TenantID = strings.TrimPrefix(tag, "tenant:")
			break
		}
	}

	return jobInfo, nil
}

// IsJobRunnable checks if the job is in runnable status
func IsJobRunnable(job *openai.Batch) bool {
	return job.BatchStatusInfo.Status == openai.BatchStatusValidating ||
		job.BatchStatusInfo.Status == openai.BatchStatusInProgress
}

// BuildUpdatedStatusInfo: build updated BatchStatusInfo object including timestamps
func BuildUpdatedStatusInfo(
	originalStatus *openai.BatchStatusInfo,
	newStatus openai.BatchStatus,
	counts *openai.BatchRequestCounts,
	slo *time.Time,
) (*openai.BatchStatusInfo, error) {
	now := time.Now().Unix()
	updated := *originalStatus
	updated.Status = newStatus

	switch newStatus {
	case openai.BatchStatusInProgress:
		updated.InProgressAt = &now
	case openai.BatchStatusCompleted:
		updated.CompletedAt = &now
	case openai.BatchStatusFailed:
		updated.FailedAt = &now
	case openai.BatchStatusCancelled:
		updated.CancelledAt = &now
	case openai.BatchStatusExpired:
		updated.ExpiredAt = &now
	case openai.BatchStatusFinalizing:
		updated.FinalizingAt = &now
	case openai.BatchStatusCancelling:
		updated.CancellingAt = &now
	case openai.BatchStatusValidating:
		if slo == nil {
			return nil, fmt.Errorf("SLO is required for status %s", newStatus)
		}
		expiresAt := slo.Unix()
		updated.ExpiresAt = &expiresAt
	default:
		return nil, fmt.Errorf("invalid status: %s", newStatus)
	}
	// expiresAt is not updated if the status is expired.

	if counts != nil {
		updated.RequestCounts = openai.BatchRequestCounts{
			Total:     counts.Total,
			Completed: counts.Completed,
			Failed:    counts.Failed,
		}
	}
	return &updated, nil
}

func GetJobPriorityDataFromQueueItem(item *db.BatchJobPriority) (*batch_types.BatchJobPriorityData, error) {
	data := &batch_types.BatchJobPriorityData{}
	if err := json.Unmarshal(item.Data, data); err != nil {
		return nil, fmt.Errorf("failed to unmarshal job priority data: %w", err)
	}
	return data, nil
}
