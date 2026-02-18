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

// The file provides shared utilities for the REST API.
package common

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

type ContextKey string

const (
	RequestIDKey ContextKey = "requestID"
	TenantIDKey  ContextKey = "tenantID"

	DefaultRequestID = "unknown"
	DefaultTenantID  = "default"
)

type Route struct {
	Method      string
	Pattern     string
	HandlerFunc http.HandlerFunc
}

type ApiHandler interface {
	GetRoutes() []Route
}

func RegisterHandler(mux *http.ServeMux, h ApiHandler) {
	routes := h.GetRoutes()
	for _, route := range routes {
		pattern := route.Method + " " + route.Pattern
		mux.HandleFunc(pattern, route.HandlerFunc)
	}
}

func DecodeJSON(r io.Reader, obj any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	return decoder.Decode(obj)
}

func WriteJSONResponse(w http.ResponseWriter, r *http.Request, status int, obj interface{}) {
	logger := logging.FromRequest(r)

	w.Header().Set("Content-Type", "application/json")

	data, err := json.Marshal(obj)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"Internal Server Error"}`))
		logger.Error(err, "failed to marshal JSON response", "status", status, "type", fmt.Sprintf("%T", obj))
		return
	}

	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		logger.Error(err, "failed to write response", "status", status, "dataLen", len(data))
		return
	}
}

func WriteAPIError(w http.ResponseWriter, r *http.Request, oaiErr openai.APIError) {
	errorResp := openai.ErrorResponse{
		Error: oaiErr,
	}

	WriteJSONResponse(w, r, oaiErr.Code, errorResp)
}

func WriteInternalServerError(w http.ResponseWriter, r *http.Request) {
	apiErr := openai.NewAPIError(http.StatusInternalServerError, "", "Internal Server Error", nil)
	WriteAPIError(w, r, apiErr)
}

func GetRequestIDFromContext(ctx context.Context) string {
	if requestID, ok := ctx.Value(RequestIDKey).(string); ok {
		return requestID
	}
	return DefaultRequestID
}

func GetTenantIDFromContext(ctx context.Context) string {
	if tenantID, ok := ctx.Value(TenantIDKey).(string); ok {
		return tenantID
	}
	return DefaultTenantID
}
