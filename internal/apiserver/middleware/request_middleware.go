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

// The file implements request middleware for generating request IDs, logging requests and recording metrics.
package middleware

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/health"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	"k8s.io/klog/v2"
)

type contextKey string

const (
	requestIDHeader            = "X-Request-ID"
	requestIDKey    contextKey = "requestID"
)

func RequestMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip /metrics and /health endpoints to avoid noise in logs and metrics
		if r.URL.Path == metrics.MetricsPath || r.URL.Path == health.HealthPath {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		metrics.RecordRequestStart()

		requestID := r.Header.Get(requestIDHeader)
		if requestID == "" {
			requestID = uuid.NewString()
		}
		w.Header().Set(requestIDHeader, requestID)

		// Create request logger with request ID
		logger := klog.FromContext(r.Context()).WithValues("requestID", requestID)
		ctx := klog.NewContext(r.Context(), logger)
		ctx = context.WithValue(ctx, requestIDKey, requestID)

		// Wrap response writer to capture status code
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		// Log incoming request
		logger.V(logging.TRACE).Info("incoming request",
			"method", r.Method,
			"path", r.URL.Path,
			"remoteAddr", r.RemoteAddr,
		)

		defer func() {
			duration := time.Since(start).Seconds()
			status := strconv.Itoa(rw.statusCode)
			metrics.RecordRequestFinish(r.Method, r.URL.Path, status, duration)
		}()

		next.ServeHTTP(rw, r.WithContext(ctx))
	})
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// GetRequestID retrieves the request ID from the context.
func GetRequestIDFromContext(ctx context.Context) string {
	if requestID, ok := ctx.Value(requestIDKey).(string); ok {
		return requestID
	}
	return "unknown"
}
