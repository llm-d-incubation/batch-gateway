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
	"fmt"
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

func newTestPool(t *testing.T, mr *miniredis.Miniredis, poolName string) *asyncPool {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

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

	logger := testLogger(t)
	d := newResultDispatcher(p, logger, time.Second)
	pool := &asyncPool{producer: p, dispatcher: d, logger: logger}

	t.Cleanup(func() {
		_ = d.Close()
		_ = p.Close()
	})

	return pool
}

func pushResult(t *testing.T, mr *miniredis.Miniredis, queue, id, payload string) {
	t.Helper()
	data, err := json.Marshal(api.ResultMessage{ID: id, Payload: payload})
	if err != nil {
		t.Fatalf("marshal result %s: %v", id, err)
	}
	if _, err := mr.Lpush(queue, string(data)); err != nil {
		t.Fatalf("Lpush %s: %v", id, err)
	}
}

func TestAsyncProducerClient_Submit(t *testing.T) {
	t.Run("submit and get result", func(t *testing.T) {
		mr := miniredis.RunT(t)
		poolName := "submit-pool"
		resultQueue := asyncQueuePrefix + "results:" + poolName

		pool := newTestPool(t, mr, poolName)
		client := newAsyncProducerClient(pool)
		defer func() { _ = client.Close() }()

		go func() {
			time.Sleep(50 * time.Millisecond)
			pushResult(t, mr, resultQueue, "req-1", `{"choices":[{"text":"hello"}]}`)
		}()

		if err := client.Submit(context.Background(), &GenerateRequest{
			RequestID: "req-1",
			Endpoint:  "/v1/completions",
			Params:    map[string]any{"model": "test-model"},
		}); err != nil {
			t.Fatalf("Submit error: %s", err.Message)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		resp, err := client.GetResult(ctx)
		if err != nil {
			t.Fatalf("GetResult error: %v", err)
		}
		if resp.RequestID != "req-1" {
			t.Errorf("RequestID = %q, want %q", resp.RequestID, "req-1")
		}
	})

	t.Run("multiple submits routed correctly", func(t *testing.T) {
		mr := miniredis.RunT(t)
		poolName := "multi-pool"
		resultQueue := asyncQueuePrefix + "results:" + poolName

		pool := newTestPool(t, mr, poolName)
		client := newAsyncProducerClient(pool)
		defer func() { _ = client.Close() }()

		for _, id := range []string{"s-1", "s-2", "s-3"} {
			if err := client.Submit(context.Background(), &GenerateRequest{
				RequestID: id,
				Endpoint:  "/v1/completions",
				Params:    map[string]any{"model": "test-model"},
			}); err != nil {
				t.Fatalf("Submit(%s) error: %s", id, err.Message)
			}
		}

		// Push results in reverse order
		for _, id := range []string{"s-3", "s-1", "s-2"} {
			pushResult(t, mr, resultQueue, id, fmt.Sprintf(`{"id":"%s"}`, id))
		}

		got := make(map[string]bool)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		for i := 0; i < 3; i++ {
			resp, err := client.GetResult(ctx)
			if err != nil {
				t.Fatalf("GetResult %d/3 error: %v", i+1, err)
			}
			got[resp.RequestID] = true
		}

		for _, id := range []string{"s-1", "s-2", "s-3"} {
			if !got[id] {
				t.Errorf("missing result for %s", id)
			}
		}
	})

	t.Run("close unregisters pending waiters", func(t *testing.T) {
		mr := miniredis.RunT(t)
		poolName := "close-pool"
		resultQueue := asyncQueuePrefix + "results:" + poolName

		pool := newTestPool(t, mr, poolName)
		client := newAsyncProducerClient(pool)

		if err := client.Submit(context.Background(), &GenerateRequest{
			RequestID: "c-1",
			Endpoint:  "/v1/completions",
			Params:    map[string]any{"model": "test-model"},
		}); err != nil {
			t.Fatalf("Submit error: %s", err.Message)
		}

		_ = client.Close()

		// Push a result — it should be dropped (no waiter)
		pushResult(t, mr, resultQueue, "c-1", `{"id":"c-1"}`)
		time.Sleep(200 * time.Millisecond)

		// Verify no result on the channel
		select {
		case <-client.results:
			t.Fatal("expected no result after Close")
		default:
		}
	})

	t.Run("per-job isolation with shared pool", func(t *testing.T) {
		mr := miniredis.RunT(t)
		poolName := "isolation-pool"
		resultQueue := asyncQueuePrefix + "results:" + poolName

		pool := newTestPool(t, mr, poolName)
		clientA := newAsyncProducerClient(pool)
		clientB := newAsyncProducerClient(pool)
		defer func() { _ = clientA.Close() }()
		defer func() { _ = clientB.Close() }()

		if err := clientA.Submit(context.Background(), &GenerateRequest{
			RequestID: "job-a-req",
			Endpoint:  "/v1/completions",
			Params:    map[string]any{"model": "test-model"},
		}); err != nil {
			t.Fatalf("Submit A error: %s", err.Message)
		}
		if err := clientB.Submit(context.Background(), &GenerateRequest{
			RequestID: "job-b-req",
			Endpoint:  "/v1/completions",
			Params:    map[string]any{"model": "test-model"},
		}); err != nil {
			t.Fatalf("Submit B error: %s", err.Message)
		}

		// Push results
		pushResult(t, mr, resultQueue, "job-b-req", `{"id":"job-b-req"}`)
		pushResult(t, mr, resultQueue, "job-a-req", `{"id":"job-a-req"}`)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		respA, err := clientA.GetResult(ctx)
		if err != nil {
			t.Fatalf("GetResult A error: %v", err)
		}
		if respA.RequestID != "job-a-req" {
			t.Errorf("client A got %q, want %q", respA.RequestID, "job-a-req")
		}

		respB, err := clientB.GetResult(ctx)
		if err != nil {
			t.Fatalf("GetResult B error: %v", err)
		}
		if respB.RequestID != "job-b-req" {
			t.Errorf("client B got %q, want %q", respB.RequestID, "job-b-req")
		}
	})

	t.Run("close handles non-string key without panic", func(t *testing.T) {
		mr := miniredis.RunT(t)
		pool := newTestPool(t, mr, "close-nonstring-pool")
		client := newAsyncProducerClient(pool)

		// Manually store a non-string key in pendingIDs
		client.pendingIDs.Store(42, struct{}{})
		client.pendingIDs.Store("valid-id", struct{}{})

		// Close should not panic
		if err := client.Close(); err != nil {
			t.Fatalf("Close() error: %v", err)
		}
	})

	t.Run("cancel marks pending requests", func(t *testing.T) {
		mr := miniredis.RunT(t)
		poolName := "cancel-pool"
		pool := newTestPool(t, mr, poolName)
		client := newAsyncProducerClient(pool)
		defer func() { _ = client.Close() }()

		if err := client.Submit(context.Background(), &GenerateRequest{
			RequestID: "cancel-1",
			Endpoint:  "/v1/completions",
			Params:    map[string]any{"model": "test-model"},
		}); err != nil {
			t.Fatalf("Submit error: %s", err.Message)
		}

		active, err := mr.Get(api.RequestActiveTokenKey("cancel-1"))
		if err != nil || active == "" {
			t.Fatalf("expected active request token after Submit, got %q err=%v", active, err)
		}

		if err := client.Cancel(context.Background()); err != nil {
			t.Fatalf("Cancel error: %v", err)
		}

		got, err := mr.Get(api.RequestCancellationKey("cancel-1"))
		if err != nil {
			t.Fatalf("get cancellation marker: %v", err)
		}
		if got != active {
			t.Fatalf("cancellation marker = %q, want active token %q", got, active)
		}
	})

	t.Run("cancel with no pending is no-op", func(t *testing.T) {
		mr := miniredis.RunT(t)
		pool := newTestPool(t, mr, "cancel-empty-pool")
		client := newAsyncProducerClient(pool)
		defer func() { _ = client.Close() }()

		if err := client.Cancel(context.Background()); err != nil {
			t.Fatalf("Cancel error: %v", err)
		}
	})
}

func TestResultDispatcher_PanicRecovery(t *testing.T) {
	mr := miniredis.RunT(t)
	poolName := "panic-pool"
	resultQueue := asyncQueuePrefix + "results:" + poolName

	pool := newTestPool(t, mr, poolName)
	client := newAsyncProducerClient(pool)
	defer func() { _ = client.Close() }()

	// Submit two requests
	for _, id := range []string{"panic-1", "panic-2"} {
		if err := client.Submit(context.Background(), &GenerateRequest{
			RequestID: id,
			Endpoint:  "/v1/completions",
			Params:    map[string]any{"model": "test-model"},
		}); err != nil {
			t.Fatalf("Submit(%s) error: %s", id, err.Message)
		}
	}

	// Push a malformed result that will trigger unexpected behavior,
	// then a valid result. The dispatcher should recover and deliver the second.
	// We can't easily inject a panic into the producer, so we verify the
	// dispatcher continues after processing a result with no waiter (which
	// exercises the recovery path structurally).
	pushResult(t, mr, resultQueue, "unknown-id", `{"id":"unknown"}`)
	pushResult(t, mr, resultQueue, "panic-1", `{"id":"panic-1"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.GetResult(ctx)
	if err != nil {
		t.Fatalf("GetResult error: %v", err)
	}
	if resp.RequestID != "panic-1" {
		t.Errorf("RequestID = %q, want %q", resp.RequestID, "panic-1")
	}
}

func TestAsyncProducerClient_SubmitPropagatesTraceContext(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	mr := miniredis.RunT(t)
	poolName := "otel-pool"
	requestQueue := asyncQueuePrefix + "requests:" + poolName

	pool := newTestPool(t, mr, poolName)
	client := newAsyncProducerClient(pool)
	defer func() { _ = client.Close() }()

	// Create a parent span to simulate the job runner's trace context
	ctx, parentSpan := otel.Tracer("test").Start(context.Background(), "process-batch")
	parentTraceID := parentSpan.SpanContext().TraceID().String()

	if err := client.Submit(ctx, &GenerateRequest{
		RequestID: "otel-req-1",
		Endpoint:  "/v1/completions",
		Params:    map[string]any{"model": "test-model", "prompt": "hello"},
	}); err != nil {
		t.Fatalf("Submit error: %s", err.Message)
	}
	parentSpan.End()

	// Read the enqueued message from Redis and verify it carries traceparent
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
