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

// The file defines error types and error handling utilities for OpenAI-compatible responses.
package openai

import (
	"net/http"
)

// APIError represents an error that originates from the API.
// Per the OpenAI spec, Code is a nullable string in the JSON body (e.g. "model_not_found"),
// Type is the error category (e.g. "invalid_request_error"), and HTTPStatus carries
// the HTTP status code used for the response header.
type APIError struct {
	HTTPStatus int     `json:"-"`
	Code       *string `json:"code"`
	Type       string  `json:"type"`
	Message    string  `json:"message"`
	Param      *string `json:"param"`
}

func NewAPIError(httpStatus int, errorType string, message string, param *string) APIError {
	if errorType == "" {
		errorType = ErrorCodeToType(httpStatus)
	}
	return APIError{
		HTTPStatus: httpStatus,
		Type:       errorType,
		Message:    message,
		Param:      param,
	}
}

type ErrorResponse struct {
	Error APIError `json:"error"`
}

func ErrorCodeToType(code int) string {
	// Values match the error.type field in actual OpenAI API responses.
	// https://platform.openai.com/docs/guides/error-codes
	switch code {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		if code >= http.StatusInternalServerError {
			return "server_error"
		}
		return "api_error"
	}
}
