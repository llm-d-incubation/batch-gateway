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
	"testing"

	"github.com/go-logr/logr"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"go.opentelemetry.io/otel"
)

// capturingSink is a logr.LogSink that records WithValues calls for test assertions.
type capturingSink struct {
	values map[string]any
}

var _ logr.LogSink = (*capturingSink)(nil)

func newCapturingSink() *capturingSink {
	return &capturingSink{values: make(map[string]any)}
}

func (s *capturingSink) Init(logr.RuntimeInfo)        {}
func (s *capturingSink) Enabled(int) bool             { return true }
func (s *capturingSink) Info(int, string, ...any)     {}
func (s *capturingSink) Error(error, string, ...any)  {}
func (s *capturingSink) WithName(string) logr.LogSink { return s }
func (s *capturingSink) WithValues(keysAndValues ...any) logr.LogSink {
	next := &capturingSink{values: make(map[string]any, len(s.values)+len(keysAndValues)/2)}
	for k, v := range s.values {
		next.values[k] = v
	}
	for i := 0; i+1 < len(keysAndValues); i += 2 {
		if key, ok := keysAndValues[i].(string); ok {
			next.values[key] = keysAndValues[i+1]
		}
	}
	return next
}

func TestStartSpan_InjectsTraceFields(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background()) //nolint:errcheck
	original := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(original)

	sink := newCapturingSink()
	logger := logr.New(sink)
	ctx := logr.NewContext(context.Background(), logger)

	ctx, span := StartSpan(ctx, "test-span")
	defer span.End()

	sc := span.SpanContext()
	if !sc.IsValid() {
		t.Fatal("span context should be valid with a real tracer provider")
	}

	enriched := logr.FromContextOrDiscard(ctx).GetSink().(*capturingSink)

	if got, ok := enriched.values["trace_id"]; !ok {
		t.Error("expected trace_id in logger values")
	} else if got != sc.TraceID().String() {
		t.Errorf("trace_id = %q, want %q", got, sc.TraceID().String())
	}

	if got, ok := enriched.values["span_id"]; !ok {
		t.Error("expected span_id in logger values")
	} else if got != sc.SpanID().String() {
		t.Errorf("span_id = %q, want %q", got, sc.SpanID().String())
	}
}

func TestStartSpan_NoOpProvider_NoTraceFields(t *testing.T) {
	sink := newCapturingSink()
	logger := logr.New(sink)
	ctx := logr.NewContext(context.Background(), logger)

	ctx, span := StartSpan(ctx, "noop-span")
	defer span.End()

	result := logr.FromContextOrDiscard(ctx).GetSink().(*capturingSink)

	if _, ok := result.values["trace_id"]; ok {
		t.Error("trace_id should not be present with no-op tracer provider")
	}
	if _, ok := result.values["span_id"]; ok {
		t.Error("span_id should not be present with no-op tracer provider")
	}
}

func TestStartSpan_NestedSpans_NoDuplicateKeys(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background()) //nolint:errcheck
	original := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(original)

	sink := newCapturingSink()
	logger := logr.New(sink).WithValues("jobId", "job-123")
	ctx := logr.NewContext(context.Background(), logger)

	ctx, outerSpan := StartSpan(ctx, "outer-span")
	defer outerSpan.End()

	ctx, innerSpan := StartSpan(ctx, "inner-span")
	defer innerSpan.End()

	innerSC := innerSpan.SpanContext()
	enriched := logr.FromContextOrDiscard(ctx).GetSink().(*capturingSink)

	if got := enriched.values["span_id"]; got != innerSC.SpanID().String() {
		t.Errorf("span_id = %q, want inner span's %q", got, innerSC.SpanID().String())
	}
	if got := enriched.values["trace_id"]; got != innerSC.TraceID().String() {
		t.Errorf("trace_id = %q, want %q", got, innerSC.TraceID().String())
	}
	if got, ok := enriched.values["jobId"]; !ok || got != "job-123" {
		t.Errorf("pre-existing jobId should be preserved, got %v", got)
	}
}

func TestDetachedContext_InjectsNewTraceFields(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background()) //nolint:errcheck
	original := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(original)

	sink := newCapturingSink()
	logger := logr.New(sink)
	parentCtx := logr.NewContext(context.Background(), logger)

	parentCtx, parentSpan := StartSpan(parentCtx, "parent-span")
	defer parentSpan.End()
	parentSC := parentSpan.SpanContext()

	detachedCtx, detachedSpan := DetachedContext(parentCtx, "detached-span")
	defer detachedSpan.End()
	detachedSC := detachedSpan.SpanContext()

	if parentSC.TraceID() == detachedSC.TraceID() {
		t.Error("detached span should have a different trace_id than the parent")
	}

	enriched := logr.FromContextOrDiscard(detachedCtx).GetSink().(*capturingSink)

	if got, ok := enriched.values["trace_id"]; !ok {
		t.Error("expected trace_id in detached context logger")
	} else if got != detachedSC.TraceID().String() {
		t.Errorf("detached trace_id = %q, want %q (not parent's %q)", got, detachedSC.TraceID().String(), parentSC.TraceID().String())
	}

	if got, ok := enriched.values["span_id"]; !ok {
		t.Error("expected span_id in detached context logger")
	} else if got != detachedSC.SpanID().String() {
		t.Errorf("detached span_id = %q, want %q", got, detachedSC.SpanID().String())
	}
}
