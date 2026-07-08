//go:build integration

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

package integration

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	dbapi "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
)

// Mini-benchmarks for the Postgres exchange backend against a real database.
// Run with the same container as the integration tests:
//
//	make test-integration-postgres  # or start the container manually, then:
//	POSTGRES_TEST_URL=... go test -tags=integration -run '^$' -bench BenchmarkPostgresExchange ./test/integration/
//
// These measure single-connection-pool round trips on a local socket — a
// floor for latency and a rough ceiling for single-pod throughput, not a
// cluster benchmark.

func BenchmarkPostgresExchangeEnqueue(b *testing.B) {
	client := newExchangeTestClient(b)
	ctx := context.Background()
	slo := time.Now().UTC().Add(time.Hour)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := client.PQEnqueue(ctx, testJob(fmt.Sprintf("bench-%d", i), slo)); err != nil {
			b.Fatalf("PQEnqueue: %v", err)
		}
	}
}

func BenchmarkPostgresExchangeEnqueueParallel(b *testing.B) {
	client := newExchangeTestClient(b)
	ctx := context.Background()
	slo := time.Now().UTC().Add(time.Hour)
	var seq atomic.Int64

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := seq.Add(1)
			if err := client.PQEnqueue(ctx, testJob(fmt.Sprintf("bench-p-%d", i), slo)); err != nil {
				b.Fatalf("PQEnqueue: %v", err)
			}
		}
	})
}

// BenchmarkPostgresExchangeEnqueueDequeue measures a full queue round trip:
// one enqueue followed by one non-blocking drain (the SKIP LOCKED delete).
func BenchmarkPostgresExchangeEnqueueDequeue(b *testing.B) {
	client := newExchangeTestClient(b)
	ctx := context.Background()
	slo := time.Now().UTC().Add(time.Hour)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := client.PQEnqueue(ctx, testJob(fmt.Sprintf("bench-rt-%d", i), slo)); err != nil {
			b.Fatalf("PQEnqueue: %v", err)
		}
		items, err := client.PQDequeue(ctx, 0, 1)
		if err != nil {
			b.Fatalf("PQDequeue: %v", err)
		}
		if len(items) != 1 {
			b.Fatalf("expected 1 item, got %d", len(items))
		}
	}
}

// BenchmarkPostgresExchangeBlockingWake measures the LISTEN/NOTIFY path:
// a parked blocking dequeue woken by an enqueue. This is the latency that
// matters for idle workers picking up new jobs.
func BenchmarkPostgresExchangeBlockingWake(b *testing.B) {
	client := newExchangeTestClient(b)
	ctx := context.Background()
	slo := time.Now().UTC().Add(time.Hour)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		done := make(chan error, 1)
		go func() {
			items, err := client.PQDequeue(ctx, 10*time.Second, 1)
			if err == nil && len(items) != 1 {
				err = fmt.Errorf("expected 1 item, got %d", len(items))
			}
			done <- err
		}()
		// Give the consumer time to finish its empty first drain and park on
		// the LISTEN subscription before the wake is sent.
		time.Sleep(2 * time.Millisecond)
		if err := client.PQEnqueue(ctx, testJob(fmt.Sprintf("bench-w-%d", i), slo)); err != nil {
			b.Fatalf("PQEnqueue: %v", err)
		}
		if err := <-done; err != nil {
			b.Fatalf("PQDequeue: %v", err)
		}
	}
}

// BenchmarkPostgresExchangeStatusSet measures the high-frequency progress
// write path: repeated upserts of the same row on the UNLOGGED status table.
func BenchmarkPostgresExchangeStatusSet(b *testing.B) {
	client := newExchangeTestClient(b)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		payload := []byte(fmt.Sprintf(`{"total": 1000, "completed": %d, "failed": 0}`, i))
		if err := client.StatusSet(ctx, "bench-job", 3600, payload); err != nil {
			b.Fatalf("StatusSet: %v", err)
		}
	}
}

func BenchmarkPostgresExchangeStatusSetParallel(b *testing.B) {
	client := newExchangeTestClient(b)
	ctx := context.Background()
	var seq atomic.Int64

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		// Each goroutine writes its own job row, matching real workers that
		// each update their own job's progress.
		jobID := fmt.Sprintf("bench-job-%d", seq.Add(1))
		for pb.Next() {
			if err := client.StatusSet(ctx, jobID, 3600, []byte(`{"total": 1000, "completed": 5}`)); err != nil {
				b.Fatalf("StatusSet: %v", err)
			}
		}
	})
}

func BenchmarkPostgresExchangeInFlightHeartbeat(b *testing.B) {
	client := newExchangeTestClient(b)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := client.InFlightSet(ctx, "bench-job", "bench-proc"); err != nil {
			b.Fatalf("InFlightSet: %v", err)
		}
	}
}

// BenchmarkPostgresExchangeEventRoundTrip measures produce -> NOTIFY ->
// dispatcher drain -> consumer channel delivery for a single job.
func BenchmarkPostgresExchangeEventRoundTrip(b *testing.B) {
	client := newExchangeTestClient(b)
	ctx := context.Background()

	ch, err := client.ECConsumerGetChannel(ctx, "bench-job")
	if err != nil {
		b.Fatalf("ECConsumerGetChannel: %v", err)
	}
	defer ch.CloseFn()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := client.ECProducerSendEvents(ctx, []dbapi.BatchEvent{
			{ID: "bench-job", Type: dbapi.BatchEventCancel, TTL: 3600},
		}); err != nil {
			b.Fatalf("ECProducerSendEvents: %v", err)
		}
		select {
		case <-ch.Events:
		case <-time.After(5 * time.Second):
			b.Fatal("event never delivered")
		}
	}
}
