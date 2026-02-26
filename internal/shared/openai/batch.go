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

import (
	"errors"
	"time"
)

// https://platform.openai.com/docs/api-reference/batch

// Endpoint represents a batch API endpoint
type Endpoint string

// Supported batch API endpoints
const (
	EndpointResponses       Endpoint = "/v1/responses"
	EndpointChatCompletions Endpoint = "/v1/chat/completions"
	EndpointEmbeddings      Endpoint = "/v1/embeddings"
	EndpointCompletions     Endpoint = "/v1/completions"
	EndpointModerations     Endpoint = "/v1/moderations"
)

func (e Endpoint) String() string {
	return string(e)
}

type BatchStatus string

const (
	BatchStatusValidating BatchStatus = "validating"
	BatchStatusFailed     BatchStatus = "failed"
	BatchStatusInProgress BatchStatus = "in_progress"
	BatchStatusFinalizing BatchStatus = "finalizing"
	BatchStatusCompleted  BatchStatus = "completed"
	BatchStatusExpired    BatchStatus = "expired"
	BatchStatusCancelling BatchStatus = "cancelling"
	BatchStatusCancelled  BatchStatus = "cancelled"
)

func (s BatchStatus) String() string {
	return string(s)
}

func (s BatchStatus) IsFinal() bool {
	return s == BatchStatusCompleted || s == BatchStatusFailed || s == BatchStatusExpired || s == BatchStatusCancelled
}

type BatchSpec struct {
	// required. The object type, which is always `batch`.
	Object string `json:"object"`

	// required. The OpenAI API endpoint used by the batch.
	Endpoint Endpoint `json:"endpoint"`

	// required. The ID of the input file for the batch.
	InputFileID string `json:"input_file_id"`

	// required. The time frame within which the batch should be processed.
	CompletionWindow string `json:"completion_window"`

	// optional. Set of 16 key-value pairs that can be attached to an object. This can be useful for storing additional information about the object in a structured format, and querying for objects via API or the dashboard.   Keys are strings with a maximum length of 64 characters. Values are strings with a maximum length of 512 characters.
	Metadata map[string]string `json:"metadata,omitempty"`

	// required. The Unix timestamp (in seconds) for when the batch was created.
	CreatedAt int64 `json:"created_at"`
}

type BatchStatusInfo struct {
	// required. The current status of the batch.
	Status BatchStatus `json:"status"`

	// optional. The ID of the file containing the outputs of successfully executed requests.
	OutputFileID string `json:"output_file_id,omitempty"`

	// optional. The ID of the file containing the outputs of requests with errors.
	ErrorFileID string `json:"error_file_id,omitempty"`

	// optional. The Unix timestamp (in seconds) for when the batch was cancelled.
	CancelledAt *int64 `json:"cancelled_at,omitempty"`

	// optional. The Unix timestamp (in seconds) for when the batch started cancelling.
	CancellingAt *int64 `json:"cancelling_at,omitempty"`

	// optional. The Unix timestamp (in seconds) for when the batch was completed.
	CompletedAt *int64 `json:"completed_at,omitempty"`

	// optional. The Unix timestamp (in seconds) for when the batch expired.
	ExpiredAt *int64 `json:"expired_at,omitempty"`

	// optional. The Unix timestamp (in seconds) for when the batch will expire.
	ExpiresAt *int64 `json:"expires_at,omitempty"`

	// optional. The Unix timestamp (in seconds) for when the batch failed.
	FailedAt *int64 `json:"failed_at,omitempty"`

	// optional. The Unix timestamp (in seconds) for when the batch started finalizing.
	FinalizingAt *int64 `json:"finalizing_at,omitempty"`

	// optional. The Unix timestamp (in seconds) for when the batch started processing.
	InProgressAt *int64 `json:"in_progress_at,omitempty"`

	// optional. The Model ID used to process the batch
	Model string `json:"model,omitempty"`

	// optional. The request counts for different statuses within the batch.
	RequestCounts BatchRequestCounts `json:"request_counts"`

	// optional.
	Errors *BatchErrors `json:"errors,omitempty"`

	// optional. Represents token usage details including input tokens, output tokens, a
	// breakdown of output tokens, and the total tokens used.
	Usage *BatchUsage `json:"usage,omitempty"`
}

type Batch struct {
	// required.
	ID string `json:"id"`
	BatchSpec
	BatchStatusInfo
}

type ListBatchResponse struct {
	// required. The type of object returned, must be `list`.
	Object string `json:"object"`

	// required. A list of items used to generate this response.
	Data []Batch `json:"data"`

	// required. The ID of the first item in the list.
	FirstID string `json:"first_id"`

	// required. The ID of the last item in the list.
	LastID string `json:"last_id"`

	// required. Whether there are more items available.
	HasMore bool `json:"has_more"`
}

type BatchError struct {

	// optional. An error code identifying the error type.
	Code string `json:"code,omitempty"`

	// optional. optional. A human-readable message providing more details about the error.
	Message string `json:"message,omitempty"`

	// optional. The name of the parameter that caused the error, if applicable.
	Param string `json:"param,omitempty"`

	// optional. The line number of the input file where the error occurred, if applicable.
	Line int64 `json:"line,omitempty"`
}

type BatchErrors struct {

	// optional. The object type, which is always `list`.
	Object string `json:"object"`

	// optional.
	Data []BatchError `json:"data"`
}

// BatchRequestCounts - The request counts for different statuses within the batch.
type BatchRequestCounts struct {

	// required. Total number of requests in the batch.
	Total int64 `json:"total"`

	// required. Number of requests that have been completed successfully.
	Completed int64 `json:"completed"`

	// required. Number of requests that have failed.
	Failed int64 `json:"failed"`
}

type BatchUsage struct {
	// required. The number of input tokens.
	InputTokens int64 `json:"input_tokens"`

	// required. A detailed breakdown of the input tokens.
	InputTokensDetails BatchUsageInputTokensDetails `json:"input_tokens_details"`

	// required. The number of output tokens.
	OutputTokens int64 `json:"output_tokens"`

	// required. A detailed breakdown of the output tokens.
	OutputTokensDetails BatchUsageOutputTokensDetails `json:"output_tokens_details"`

	// required. The total number of tokens used.
	TotalTokens int64 `json:"total_tokens"`
}

type BatchUsageInputTokensDetails struct {
	// required. The number of tokens that were retrieved from the cache.
	// [More on prompt caching](https://platform.openai.com/docs/guides/prompt-caching).
	CachedTokens int64 `json:"cached_tokens"`
}

type BatchUsageOutputTokensDetails struct {
	// required. The number of reasoning tokens.
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type CreateBatchRequest struct {

	// required. The ID of an uploaded file that contains requests for the new batch.  See [upload file](/docs/api-reference/files/create) for how to upload a file.  Your input file must be formatted as a [JSONL file](/docs/api-reference/batch/request-input), and must be uploaded with the purpose `batch`. The file can contain up to 50,000 requests, and can be up to 200 MB in size.
	InputFileID string `json:"input_file_id"`

	// required. The endpoint to be used for all requests in the batch. Currently `/v1/responses`, `/v1/chat/completions`, `/v1/embeddings`, and `/v1/completions` are supported. Note that `/v1/embeddings` batches are also restricted to a maximum of 50,000 embedding inputs across all requests in the batch.
	Endpoint Endpoint `json:"endpoint"`

	// required. The time frame within which the batch should be processed. Currently only `24h` is supported.
	CompletionWindow string `json:"completion_window"`

	// optional. Set of 16 key-value pairs that can be attached to an object. This can be useful for storing additional information about the object in a structured format, and querying for objects via API or the dashboard.   Keys are strings with a maximum length of 64 characters. Values are strings with a maximum length of 512 characters.
	Metadata map[string]string `json:"metadata"`

	// optional. The expiration policy for the output and/or error file that are generated for a batch.
	OutputExpiresAfter *OutputExpiresAfter `json:"output_expires_after"`
}

type OutputExpiresAfter struct {
	// required. The number of seconds after the anchor time that the file will expire. Must be
	// between 3600 (1 hour) and 2592000 (30 days).
	Seconds int64 `json:"seconds"`
	// required. Anchor timestamp after which the expiration policy applies. Supported anchors:
	// `created_at`. Note that the anchor is the file creation time, not the time the
	// batch is created.
	//
	// This field can be elided, and will marshal its zero value as "created_at".
	Anchor string `json:"anchor"`
}

func (r *CreateBatchRequest) Validate() error {
	if r.CompletionWindow == "" {
		return errors.New("completion_window is required")
	}

	if _, err := time.ParseDuration(r.CompletionWindow); err != nil {
		return errors.New("completion_window must be a valid duration (e.g., 24h)")
	}

	if r.Endpoint == "" {
		return errors.New("endpoint is required")
	}

	validEndpoints := map[Endpoint]bool{
		EndpointResponses:       true,
		EndpointChatCompletions: true,
		EndpointEmbeddings:      true,
		EndpointCompletions:     true,
		EndpointModerations:     true,
	}
	if !validEndpoints[r.Endpoint] {
		return errors.New("invalid endpoint: " + string(r.Endpoint))
	}

	if r.InputFileID == "" {
		return errors.New("input_file_id is required")
	}

	if r.OutputExpiresAfter != nil {
		if r.OutputExpiresAfter.Anchor != "created_at" {
			return errors.New("output_expires_after.anchor must be 'created_at'")
		}

		if r.OutputExpiresAfter.Seconds < 3600 || r.OutputExpiresAfter.Seconds > 2592000 {
			return errors.New("output_expires_after.seconds must be between 3600 (1 hour) and 2592000 (30 days)")
		}
	}

	return nil
}
