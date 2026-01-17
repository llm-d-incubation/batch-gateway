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

// The file defines the Batch API data structures matching the OpenAI specification.
package openai

// https://platform.openai.com/docs/api-reference/batch
type Batch struct {
	ID string `json:"id"`

	// The object type, which is always `batch`.
	Object string `json:"object"`

	// The OpenAI API endpoint used by the batch.
	Endpoint string `json:"endpoint"`

	Errors BatchErrors `json:"errors,omitempty"`

	// The ID of the input file for the batch.
	InputFileID string `json:"input_file_id"`

	// The time frame within which the batch should be processed.
	CompletionWindow string `json:"completion_window"`

	// The current status of the batch.
	Status string `json:"status"`

	// The ID of the file containing the outputs of successfully executed requests.
	OutputFileID string `json:"output_file_id,omitempty"`

	// The ID of the file containing the outputs of requests with errors.
	ErrorFileID string `json:"error_file_id,omitempty"`

	// The Unix timestamp (in seconds) for when the batch was created.
	CreatedAt int32 `json:"created_at"`

	// The Unix timestamp (in seconds) for when the batch started processing.
	InProgressAt int32 `json:"in_progress_at,omitempty"`

	// The Unix timestamp (in seconds) for when the batch will expire.
	ExpiresAt int32 `json:"expires_at,omitempty"`

	// The Unix timestamp (in seconds) for when the batch started finalizing.
	FinalizingAt int32 `json:"finalizing_at,omitempty"`

	// The Unix timestamp (in seconds) for when the batch was completed.
	CompletedAt int32 `json:"completed_at,omitempty"`

	// The Unix timestamp (in seconds) for when the batch failed.
	FailedAt int32 `json:"failed_at,omitempty"`

	// The Unix timestamp (in seconds) for when the batch expired.
	ExpiredAt int32 `json:"expired_at,omitempty"`

	// The Unix timestamp (in seconds) for when the batch started cancelling.
	CancellingAt int32 `json:"cancelling_at,omitempty"`

	// The Unix timestamp (in seconds) for when the batch was cancelled.
	CancelledAt int32 `json:"cancelled_at,omitempty"`

	RequestCounts BatchRequestCounts `json:"request_counts,omitempty"`

	// Set of 16 key-value pairs that can be attached to an object. This can be useful for storing additional information about the object in a structured format, and querying for objects via API or the dashboard.   Keys are strings with a maximum length of 64 characters. Values are strings with a maximum length of 512 characters.
	Metadata *map[string]string `json:"metadata,omitempty"`
}

type BatchErrorsData struct {

	// An error code identifying the error type.
	Code string `json:"code,omitempty"`

	// A human-readable message providing more details about the error.
	Message string `json:"message,omitempty"`

	// The name of the parameter that caused the error, if applicable.
	Param *string `json:"param,omitempty"`

	// The line number of the input file where the error occurred, if applicable.
	Line *int32 `json:"line,omitempty"`
}

type BatchErrors struct {

	// The object type, which is always `list`.
	Object string `json:"object,omitempty"`

	Data []BatchErrorsData `json:"data,omitempty"`
}

// BatchRequestCounts - The request counts for different statuses within the batch.
type BatchRequestCounts struct {

	// Total number of requests in the batch.
	Total int32 `json:"total"`

	// Number of requests that have been completed successfully.
	Completed int32 `json:"completed"`

	// Number of requests that have failed.
	Failed int32 `json:"failed"`
}
