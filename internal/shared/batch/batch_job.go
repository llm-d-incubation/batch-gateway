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

package batch

import (
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
)

// Job represents a batch job data from DB TODO:: job struct to use in processor.

type Job struct {
	ID             string            `json:"id"`
	Location       string            `json:"location"`
	SLO            time.Time         `json:"slo"`
	ResultMetadata JobResultMetadata `json:"result_metadata"`
}

// Request represents a line in input jsonl file
type Request struct {
	RequestId   string        `json:"request_id"`
	Model       string        `json:"model"`
	JobID       string        `json:"job_id"`
	EndPoint    string        `json:"end_point"`
	RequestBody []byte        `json:"request_body"`
	Status      RequestStatus `json:"status"`
	//... other job fields
}

// JobResultMetadata
type JobResultMetadata struct {
	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

func (rm JobResultMetadata) Validate() bool {
	return rm.Succeeded+rm.Failed == rm.Total
}

// RequestLineStatus
type RequestStatus int

const (
	StatusOK RequestStatus = iota
	Error
)

func (rs RequestStatus) String() string {
	return [...]string{
		"OK", "ERROR",
	}[rs]
}

// BatchStatus - follows the OpenAI batch statuses
type BatchStatus openai.BatchStatus

const (
	StatusValidating BatchStatus = BatchStatus(openai.BatchStatusValidating)
	StatusInProgress BatchStatus = BatchStatus(openai.BatchStatusInProgress)
	StatusFinalizing BatchStatus = BatchStatus(openai.BatchStatusFinalizing)
	StatusCompleted  BatchStatus = BatchStatus(openai.BatchStatusCompleted)
	StatusFailed     BatchStatus = BatchStatus(openai.BatchStatusFailed)
	StatusExpired    BatchStatus = BatchStatus(openai.BatchStatusExpired)
	StatusCancelling BatchStatus = BatchStatus(openai.BatchStatusCancelling)
	StatusCancelled  BatchStatus = BatchStatus(openai.BatchStatusCancelled)
)
