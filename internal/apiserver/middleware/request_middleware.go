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
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/health"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	"k8s.io/klog/v2"
)

const (
	RequestIdHeaderKey = "x-request-id"
)

var (
	fileIDRegex  = regexp.MustCompile(`^/v1/files/([^/]+)`)
	batchIDRegex = regexp.MustCompile(`^/v1/batches/([^/]+)`)
)

func RequestMiddleware(config *common.ServerConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			// Skip /metrics and /health endpoints to avoid noise in logs and metrics
			if path == metrics.MetricsPath || path == health.HealthPath {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			metrics.RecordRequestStart()

			// Extract request ID from header
			requestID := r.Header.Get(RequestIdHeaderKey)
			if requestID == "" {
				requestID = uuid.NewString()
			}
			w.Header().Set(RequestIdHeaderKey, requestID)

			// Extract tenant ID from header
			tenantHeader := config.GetTenantHeader()
			tenantID := r.Header.Get(tenantHeader)
			if tenantID == "" {
				tenantID = common.DefaultTenantID
			}

			// Extract file ID and batch ID from path for logging
			fileID := ""
			if m := fileIDRegex.FindStringSubmatch(path); len(m) > 1 {
				fileID = m[1]
			}
			batchID := ""
			if m := batchIDRegex.FindStringSubmatch(path); len(m) > 1 {
				batchID = m[1]
			}

			// Create request logger with request ID and tenant ID
			logger := klog.FromContext(r.Context())
			logger = logger.WithValues("requestID", requestID)
			logger = logger.WithValues("tenantID", tenantID)
			if fileID != "" {
				logger = logger.WithValues("fileID", fileID)
			}
			if batchID != "" {
				logger = logger.WithValues("batchID", batchID)
			}

			ctx := klog.NewContext(r.Context(), logger)
			ctx = context.WithValue(ctx, common.RequestIDKey, requestID)
			ctx = context.WithValue(ctx, common.TenantIDKey, tenantID)

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

func (rw *responseWriter) StatusCode() int {
	return rw.statusCode
}
