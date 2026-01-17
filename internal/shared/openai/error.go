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

type Error struct {
	Code    int     `json:"code,omitempty"`
	Type    string  `json:"type"`
	Message string  `json:"message"`
	Param   *string `json:"param"`
}

func NewError(code int, errorType string, message string, param *string) Error {
	if errorType == "" {
		errorType = ErrorCodeToType(code)
	}
	return Error{
		Code:    code,
		Type:    errorType,
		Message: message,
		Param:   param,
	}
}

type ErrorResponse struct {
	Error Error `json:"error"`
}

func ErrorCodeToType(code int) string {
	// https://platform.openai.com/docs/guides/error-codes
	// https://www.npmjs.com/package/openai
	errorType := ""
	switch code {
	case http.StatusBadRequest:
		errorType = "BadRequestError"
	case http.StatusUnauthorized:
		errorType = "AuthenticationError"
	case http.StatusForbidden:
		errorType = "PermissionDeniedError"
	case http.StatusNotFound:
		errorType = "NotFoundError"
	case http.StatusUnprocessableEntity:
		errorType = "UnprocessableEntityError"
	case http.StatusTooManyRequests:
		errorType = "RateLimitError"
	case http.StatusNotImplemented:
		errorType = "NotImplementedError"
	default:
		if code >= http.StatusInternalServerError {
			errorType = "InternalServerError"
		} else {
			errorType = "APIConnectionError"
		}
	}
	return errorType
}
