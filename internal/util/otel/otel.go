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

package otel

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-logr/logr"
)

const defaultServiceName = "batch-gateway"

// Span attribute keys for batch-gateway resources.
const (
	AttrBatchID      = "batch.id"
	AttrInputFileID  = "batch.input_file.id"
	AttrOutputFileID = "batch.output_file.id"
	AttrErrorFileID  = "batch.error_file.id"
	AttrTenantID     = "tenant.id"
	// Job-level request counts as span attributes for persistent trace-based analysis.
	// These complement the ephemeral Redis progress store (UpdateProgressCounts),
	// which is TTL-based and used for real-time status polling only.
	AttrRequestTotal     = "batch.request.total"
	AttrRequestCompleted = "batch.request.completed"
	AttrRequestFailed    = "batch.request.failed"
	AttrModelCount       = "batch.model.count"
	AttrRequestCount     = "batch.request.count"
	AttrInputLineCount   = "batch.input.line_count"
	AttrRejectedCount    = "batch.input.rejected_count"
	AttrSizeBucket       = "batch.size_bucket"
)

// baseLoggerKey stores the logger captured before the first trace enrichment.
// Nested StartSpan calls enrich from this base rather than from the
// accumulated context logger, preventing duplicate trace_id/span_id fields.
type baseLoggerKey struct{}

// StartSpan creates a new span using the batch-gateway tracer.
// When the span carries a valid trace context, the logger in the returned
// context is enriched with trace_id and span_id so that all downstream
// log lines emitted via logr.FromContextOrDiscard(ctx) are automatically
// correlated with the active trace.
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	ctx, span := otel.Tracer(defaultServiceName).Start(ctx, name, opts...)
	if sc := span.SpanContext(); sc.IsValid() {
		base, ok := ctx.Value(baseLoggerKey{}).(logr.Logger)
		if !ok {
			base = logr.FromContextOrDiscard(ctx)
			ctx = context.WithValue(ctx, baseLoggerKey{}, base)
		}
		logger := base.WithValues(
			"trace_id", sc.TraceID().String(),
			"span_id", sc.SpanID().String(),
		)
		ctx = logr.NewContext(ctx, logger)
	}
	return ctx, span
}

// SetAttr sets attributes on the span in the given context.
func SetAttr(ctx context.Context, attrs ...attribute.KeyValue) {
	trace.SpanFromContext(ctx).SetAttributes(attrs...)
}

// DetachedContext returns a new background context that carries a span linked to
// the span in the original context. Use this when the original context is cancelled
// (e.g. pod shutdown) but you still need to perform traced operations (e.g. re-enqueue).
// The linked span appears in Jaeger as a separate trace with a link back to the original,
// avoiding orphan spans with no connection to the parent trace.
func DetachedContext(ctx context.Context, name string) (context.Context, trace.Span) {
	var links []trace.Link
	if sc := trace.SpanFromContext(ctx).SpanContext(); sc.IsValid() {
		links = append(links, trace.Link{SpanContext: sc})
	}
	bgCtx := logr.NewContext(context.Background(), logr.FromContextOrDiscard(ctx))
	if base, ok := ctx.Value(baseLoggerKey{}).(logr.Logger); ok {
		bgCtx = context.WithValue(bgCtx, baseLoggerKey{}, base)
	}
	return StartSpan(bgCtx, name, trace.WithLinks(links...))
}

// InitTracer sets up an OpenTelemetry TracerProvider with an OTLP gRPC exporter.
// It reads the endpoint from the OTEL_EXPORTER_OTLP_ENDPOINT environment variable.
// If the endpoint is not set, tracing is disabled (no-op) and a nil shutdown function is returned.
// The service name defaults to "batch-gateway" and can be overridden via OTEL_SERVICE_NAME.
func InitTracer(ctx context.Context) (shutdown func(context.Context) error, err error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		logr.FromContextOrDiscard(ctx).Info("OTEL_EXPORTER_OTLP_ENDPOINT not set, tracing disabled")
		return func(context.Context) error { return nil }, nil
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = defaultServiceName
	}

	// The OTLP exporter respects standard OTel env vars (OTEL_EXPORTER_OTLP_ENDPOINT,
	// OTEL_EXPORTER_OTLP_INSECURE, etc.) automatically.
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	logr.FromContextOrDiscard(ctx).Info("OpenTelemetry tracing initialized", "endpoint", endpoint, "service", serviceName)

	return tp.Shutdown, nil
}
