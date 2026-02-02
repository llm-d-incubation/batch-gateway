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

package batch_utils

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	"k8s.io/klog/v2"
)

func FromDBToBatchJob(job *db.BatchJob) (*openai.Batch, error) {
	batch := &openai.Batch{
		ID: job.ID,
	}

	if err := json.Unmarshal(job.Spec, &batch.BatchSpec); err != nil {
		return nil, fmt.Errorf("failed to unmarshal batch spec: %w", err)
	}

	if err := json.Unmarshal(job.Status, &batch.BatchStatusInfo); err != nil {
		return nil, fmt.Errorf("failed to unmarshal batch status: %w", err)
	}

	return batch, nil
}

func FromDBToJobInfo(job *db.BatchJob) (*JobInfo, error) {
	jobInfo := &JobInfo{
		JobID:    job.ID,
		BatchJob: &openai.Batch{},
	}

	batchJob, err := FromDBToBatchJob(job)
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

func IsJobExpired(job *openai.Batch) bool {
	if job.BatchStatusInfo.ExpiresAt == nil {
		return false
	}

	return time.Now().Unix() >= *job.BatchStatusInfo.ExpiresAt
}

// UpdateBatchStatusInfo updates the status info of a batch job.
// It returns the updated status info object and an error if the status is invalid.
// It does not update the request counts if not provided. (nil allowed)
// slo is needed if the status is Validating.
// It does not update the database or status client.
func updateBatchStatusInfo(
	originalStatus *openai.BatchStatusInfo,
	newStatus openai.BatchStatus,
	requestCounts *openai.BatchRequestCounts,
	slo *time.Time,
) (*openai.BatchStatusInfo, error) {
	now := time.Now().Unix()

	// status update
	updatedStatus := *originalStatus
	updatedStatus.Status = newStatus

	switch newStatus {
	case openai.BatchStatusInProgress:
		updatedStatus.InProgressAt = &now
	case openai.BatchStatusCompleted:
		updatedStatus.CompletedAt = &now
	case openai.BatchStatusFailed:
		updatedStatus.FailedAt = &now
	case openai.BatchStatusCancelled:
		updatedStatus.CancelledAt = &now
	case openai.BatchStatusExpired:
		updatedStatus.ExpiredAt = &now
	case openai.BatchStatusFinalizing:
		updatedStatus.FinalizingAt = &now
	case openai.BatchStatusCancelling:
		updatedStatus.CancellingAt = &now
	case openai.BatchStatusValidating:
		if slo == nil {
			return nil, fmt.Errorf("SLO is required for status %s", newStatus)
		}
		expiresAt := slo.Unix()
		updatedStatus.ExpiresAt = &expiresAt
	default:
		return nil, fmt.Errorf("Invalid status: %s", newStatus)
	}

	// if metadata is provided, update the request counts
	// cancelled requests are not counted in the request counts
	if requestCounts != nil {
		updatedStatus.RequestCounts = openai.BatchRequestCounts{
			Total:     int64(requestCounts.Total),
			Completed: int64(requestCounts.Completed),
			Failed:    int64(requestCounts.Failed),
		}
	}

	return &updatedStatus, nil
}

// UpdateRequestCountsStatus updates Status Client with the updated request counts info of a batch job.
func UpdateRequestCountsStatus(
	statusClient db.BatchStatusClient,
	ctx context.Context,
	jobID string,
	requestCounts *openai.BatchRequestCounts,
) error {
	// light payload for frequent updates
	payload := []byte(fmt.Sprintf(`{"total": %d, "completed": %d, "failed": %d}`, requestCounts.Total, requestCounts.Completed, requestCounts.Failed))

	// update status client - TTL is set to 24 hours
	if err := statusClient.Set(ctx, jobID, 24*60*60, payload); err != nil {
		return err
	}

	return nil
}

// DBJobStatusUpdate updates DB and Status Client with the updated status info of a database BatchJob.
func UpdateDBJobStatus(
	dbClient db.BatchDBClient,
	statusClient db.BatchStatusClient,
	ctx context.Context,
	dbJob *db.BatchJob,
	newStatus openai.BatchStatus,
	requestCounts *openai.BatchRequestCounts,
	slo *time.Time,
) error {
	// get logger from context
	logger := klog.FromContext(ctx)

	// original status parsing
	var originalStatus openai.BatchStatusInfo
	if err := json.Unmarshal(dbJob.Status, &originalStatus); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to unmarshal original status", "jobID", dbJob.ID)
		return err
	}

	// field update
	updatedStatus, err := updateBatchStatusInfo(&originalStatus, openai.BatchStatus(newStatus), requestCounts, slo)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to update status info", "jobID", dbJob.ID)
		return err
	}

	statusBytes, err := json.Marshal(updatedStatus)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to marshal updated status", "jobID", dbJob.ID)
		return err
	}

	// status update in status client
	// - TTL is set to 24 hours
	if requestCounts != nil {
		if err := statusClient.Set(ctx, dbJob.ID, 24*60*60, statusBytes); err != nil {
			logger.V(logging.ERROR).Error(err, "Failed to update status in status client", "jobID", dbJob.ID)
			return err
		}
	}

	// status update in db client
	if requestCounts != nil {
		if err := UpdateRequestCountsStatus(statusClient, ctx, dbJob.ID, requestCounts); err != nil {
			logger.V(logging.ERROR).Error(err, "Failed to update temp status", "jobID", dbJob.ID)
			return err
		}
	}

	return nil
}

func ValidateBatchFile(fileID string) error {
	// TODO:: validate the file
	return nil
}
