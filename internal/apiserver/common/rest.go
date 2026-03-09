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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	uotel "github.com/llm-d-incubation/batch-gateway/internal/util/otel"
)

// StatusRecorder is implemented by http.ResponseWriter wrappers that capture the status code.
// This allows downstream handlers (e.g. OTel instrumentation) to read the status
// without adding another wrapper layer.
type StatusRecorder interface {
	http.ResponseWriter
	StatusCode() int
}

type Route struct {
	Method      string
	Pattern     string
	HandlerFunc http.HandlerFunc
	SpanName    string // OTel span operation name; empty to skip tracing
}

type ApiHandler interface {
	GetRoutes() []Route
}

func RegisterHandler(mux *http.ServeMux, h ApiHandler) {
	routes := h.GetRoutes()
	for _, route := range routes {
		pattern := route.Method + " " + route.Pattern
		handler := route.HandlerFunc
		if route.SpanName != "" {
			handler = otelInstrumentHandler(route.SpanName, handler)
		}
		mux.HandleFunc(pattern, handler)
	}
}

// otelInstrumentHandler wraps a handler to create an OTel span with the given name.
// It sets tenant.id, file.id, and batch.id span attributes from the request context and path values.
// It reads the HTTP status code from the upstream StatusRecorder (e.g. the request middleware's
// response writer) instead of adding another wrapper layer.
func otelInstrumentHandler(spanName string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := uotel.StartSpan(r.Context(), spanName)
		defer span.End()

		span.SetAttributes(attribute.String(uotel.AttrTenantID, GetTenantIDFromContext(ctx)))
		if fileID := r.PathValue(PathParamFileID); fileID != "" {
			span.SetAttributes(attribute.String(uotel.AttrInputFileID, fileID))
		}
		if batchID := r.PathValue(PathParamBatchID); batchID != "" {
			span.SetAttributes(attribute.String(uotel.AttrBatchID, batchID))
		}

		sr, ok := w.(StatusRecorder)
		if !ok {
			// No upstream StatusRecorder (e.g. request middleware absent) — add one.
			fw := &otelStatusWriter{ResponseWriter: w}
			sr = fw
			w = fw
		}

		next(w, r.WithContext(ctx))

		status := sr.StatusCode()
		span.SetAttributes(attribute.Int("http.status_code", status))
		if status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
	}
}

// Compile-time check: otelStatusWriter implements StatusRecorder.
var _ StatusRecorder = (*otelStatusWriter)(nil)

// otelStatusWriter is a minimal StatusRecorder used only when no upstream
// wrapper (e.g. request middleware) is present.
type otelStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *otelStatusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *otelStatusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *otelStatusWriter) StatusCode() int {
	return w.status
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
