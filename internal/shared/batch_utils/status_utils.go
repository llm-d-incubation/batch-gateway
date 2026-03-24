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
	"fmt"
	"time"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
)

// IsJobRunnable checks if the job is in runnable status
func IsJobRunnable(job *openai.Batch) bool {
	return job.Status == openai.BatchStatusValidating ||
		job.Status == openai.BatchStatusInProgress
}

// IsJobExpired checks if the job is expired
func IsJobExpired(task *db.BatchJobPriority) bool {
	if !task.SLO.IsZero() && time.Now().After(task.SLO) {
		return true
	}
	return false
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
