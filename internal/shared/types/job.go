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

package batch_types

import (
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
)

// Tag key prefixes and names stored in database tags (db.Tags).
const (
	TagPrefixPassThroughHeader   = "pth:"
	TagPrefixOTel                = "otel:"
	TagSLO                       = "slo_unix_micro"
	TagOutputExpiresAfterAnchor  = "output_expires_after_anchor"
	TagOutputExpiresAfterSeconds = "output_expires_after_seconds"
)

// Error codes written to the error JSONL file for requests that could not be executed
// before the job terminated. Output format follows the OpenAI Batch API error schema:
//
//	{"id": "batch_req_...", "custom_id": "...", "response": null, "error": {"code": "<code>", "message": "..."}}
//
// ErrCodeBatchExpired is defined by the OpenAI Batch API spec.
// ErrCodeBatchCancelled and ErrCodeBatchFailed are our extensions to preserve partial
// output on cancel/fail — OpenAI discards results in these cases.
const (
	ErrCodeBatchExpired   = "batch_expired"
	ErrCodeBatchCancelled = "batch_cancelled"
	ErrCodeBatchFailed    = "batch_failed"
)

type JobInfo struct {
	JobID              string            `json:"job_id"`
	TenantID           string            `json:"tenant_id"`
	BatchJob           *openai.Batch     `json:"batch_job"`
	PassThroughHeaders map[string]string `json:"pass_through_headers,omitempty"`
	TraceContext       map[string]string `json:"trace_context,omitempty"`
}

// Request represents a line in input jsonl file
type Request struct {
	CustomID string                 `json:"custom_id"` // custom id set by user
	Method   string                 `json:"method"`    // HTTP method (GET, POST, PUT, DELETE)
	URL      string                 `json:"url"`       // API endpoint (e.g., "/v1/chat/completions")
	Body     map[string]interface{} `json:"body"`      // request body
}

// ResponseData represents the response data in the output jsonl file
type ResponseData struct {
	StatusCode int                    `json:"status_code"` // HTTP status code (200, 400, 500, etc.)
	RequestID  string                 `json:"request_id"`  // request id set by inference server
	Body       map[string]interface{} `json:"body"`        // response body
}

type BatchJobPriorityData struct {
	CreatedAt int64 `json:"created_at"`
}
