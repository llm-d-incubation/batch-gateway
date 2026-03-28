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

package http

// ErrorCategory defines the category of an HTTP client error
type ErrorCategory string

const (
	ErrCategoryRateLimit  ErrorCategory = "RATE_LIMIT"   // retryable
	ErrCategoryServer     ErrorCategory = "SERVER_ERROR" // retryable
	ErrCategoryInvalidReq ErrorCategory = "INVALID_REQ"  // not retryable
	ErrCategoryAuth       ErrorCategory = "AUTH_ERROR"   // not retryable
	ErrCategoryParse      ErrorCategory = "PARSE_ERROR"  // not retryable
	ErrCategoryUnknown    ErrorCategory = "UNKNOWN"      // not retryable
)

// ClientError represents an HTTP client error with category and context
type ClientError struct {
	Category     ErrorCategory
	Message      string
	RawError     error  // original error message
	StatusCode   int    // HTTP status code (0 for non-HTTP errors like network/timeout)
	ResponseBody []byte // raw HTTP response body (nil for non-HTTP errors)
}

func (e *ClientError) Error() string {
	return e.Message
}

// OpenAIErrorType returns a best-effort OpenAI-style error type string.
// There is no global enum in the OpenAI spec for this field, so we map internal
// categories to commonly used error type values.
func (e *ClientError) OpenAIErrorType() string {
	switch e.Category {
	case ErrCategoryInvalidReq:
		return "invalid_request_error"
	case ErrCategoryAuth:
		return "authentication_error"
	case ErrCategoryRateLimit:
		return "rate_limit_error"
	case ErrCategoryServer:
		return "server_error"
	case ErrCategoryParse:
		return "invalid_response"
	default:
		return "unknown_error"
	}
}

// IsRetryable checks if the error is retryable
func (e *ClientError) IsRetryable() bool {
	return e.Category == ErrCategoryRateLimit || e.Category == ErrCategoryServer
}
