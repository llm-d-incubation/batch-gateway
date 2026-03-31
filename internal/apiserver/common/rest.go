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

type Route struct {
	Method      string
	Pattern     string
	HandlerFunc http.HandlerFunc
	SpanName    string // OTel span operation name; empty to skip tracing
}

type ApiHandler interface {
	GetRoutes() []Route
}

// RouteMiddleware produces a middleware wrapper for a given route.
// It receives the Route so it can use route-specific information (e.g. Pattern for metrics).
type RouteMiddleware func(route Route, next http.HandlerFunc) http.HandlerFunc

// RegisterHandler registers all routes from h on mux, wrapping each handler
// with the provided middleware chain. Middleware is applied in order: the first
// middleware is the outermost wrapper (executes first on request, last on response).
func RegisterHandler(mux *http.ServeMux, h ApiHandler, middlewares ...RouteMiddleware) {
	routes := h.GetRoutes()
	for _, route := range routes {
		pattern := route.Method + " " + route.Pattern
		handler := route.HandlerFunc
		// Apply middleware in reverse order so the first middleware is outermost
		for i := len(middlewares) - 1; i >= 0; i-- {
			handler = middlewares[i](route, handler)
		}
		mux.HandleFunc(pattern, handler)
	}
}

// RegisterNotFoundHandler registers a catch-all "/" route that returns a JSON 404
// response for any request not matched by more specific routes. The handler is
// wrapped with the same middleware chain as business routes.
func RegisterNotFoundHandler(mux *http.ServeMux, middlewares ...RouteMiddleware) {
	route := Route{Pattern: "/"}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oaiErr := openai.NewAPIError(http.StatusNotFound, "", fmt.Sprintf("Unknown request URL: %s %s", r.Method, r.URL.Path), nil)
		WriteAPIError(w, r, oaiErr)
	})
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](route, handler)
	}
	mux.HandleFunc("/", handler)
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
		_, _ = w.Write([]byte(`{"error":"Internal Server Error"}`))
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

	WriteJSONResponse(w, r, oaiErr.HTTPStatus, errorResp)
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
