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

package inference

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/llm-d-incubation/llm-d-async/api"
	"github.com/llm-d-incubation/llm-d-async/producer"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestAsyncSharedClient_SubmitPropagatesTraceContext(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	mr := miniredis.RunT(t)
	poolName := "otel-pool"
	requestQueue := asyncQueuePrefix + "requests:" + poolName

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	p, err := producer.NewRedisSortedSetProducer(
		producer.RedisSortedSetConfig{
			RequestQueueName: asyncQueuePrefix + "requests:" + poolName,
			ResultQueueName:  asyncQueuePrefix + "results:" + poolName,
		},
		producer.WithRedisClient(rdb),
	)
	if err != nil {
		t.Fatalf("NewRedisSortedSetProducer: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	client := newAsyncSharedClient(p, time.Second, testLogger(t))

	ctx, parentSpan := otel.Tracer("test").Start(context.Background(), "process-batch")
	parentTraceID := parentSpan.SpanContext().TraceID().String()

	if submitErr := client.Submit(ctx, &GenerateRequest{
		RequestID: "otel-req-1",
		Endpoint:  "/v1/completions",
		Params:    map[string]any{"model": "test-model", "prompt": "hello"},
	}); submitErr != nil {
		t.Fatalf("Submit error: %s", submitErr.Message)
	}
	parentSpan.End()

	members, err := mr.ZMembers(requestQueue)
	if err != nil {
		t.Fatalf("ZMembers error: %v", err)
	}
	if len(members) == 0 {
		t.Fatal("expected at least one message in request queue")
	}

	var ir api.InternalRequest
	if err := json.Unmarshal([]byte(members[0]), &ir); err != nil {
		t.Fatalf("unmarshal InternalRequest: %v", err)
	}
	if ir.PublicRequest == nil {
		t.Fatal("expected PublicRequest in InternalRequest")
	}
	metadata := ir.PublicRequest.ReqMetadata()
	if metadata == nil {
		t.Fatal("expected non-nil Metadata on enqueued request")
	}

	traceparent, ok := metadata["traceparent"]
	if !ok {
		t.Fatal("expected 'traceparent' key in request Metadata")
	}
	if len(traceparent) == 0 {
		t.Fatal("expected non-empty traceparent value")
	}

	if !strings.Contains(traceparent, parentTraceID) {
		t.Errorf("traceparent %q does not contain parent trace ID %q", traceparent, parentTraceID)
	}
}
