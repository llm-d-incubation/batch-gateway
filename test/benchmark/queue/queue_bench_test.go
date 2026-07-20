package queue_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	mockdb "github.com/llm-d/llm-d-batch-gateway/internal/database/mock"
	dbredis "github.com/llm-d/llm-d-batch-gateway/internal/database/redis"
	uredis "github.com/llm-d/llm-d-batch-gateway/internal/util/redis"
)

func newRedisExchangeClient(b *testing.B) *dbredis.ExchangeDBClientRedis {
	b.Helper()
	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		b.Fatalf("start miniredis: %v", err)
	}
	b.Cleanup(func() { mr.Close() })

	ctx := context.Background()
	cfg := &uredis.RedisClientConfig{
		Url:         "redis://" + mr.Addr(),
		ServiceName: "bench-service",
	}
	base, err := dbredis.NewDSClientRedis(ctx, cfg, 0)
	if err != nil {
		b.Fatalf("NewDSClientRedis: %v", err)
	}
	b.Cleanup(func() { _ = base.Close() })

	exch, err := dbredis.NewExchangeDBClientRedis(ctx, base, nil, 0)
	if err != nil {
		b.Fatalf("NewExchangeDBClientRedis: %v", err)
	}
	return exch
}

// BenchmarkPQEnqueue measures Redis ZADD NX throughput via miniredis.
func BenchmarkPQEnqueue(b *testing.B) {
	b.Run("serial", func(b *testing.B) {
		exch := newRedisExchangeClient(b)
		ctx := context.Background()
		slo := time.Now().Add(24 * time.Hour)

		b.ResetTimer()
		for i := range b.N {
			item := &db_api.BatchJobPriority{
				ID:  fmt.Sprintf("job-%d", i),
				SLO: slo,
			}
			if err := exch.PQEnqueue(ctx, item); err != nil {
				b.Fatalf("PQEnqueue: %v", err)
			}
		}
	})

	b.Run("parallel_10", func(b *testing.B) {
		exch := newRedisExchangeClient(b)
		ctx := context.Background()
		slo := time.Now().Add(24 * time.Hour)
		var counter atomic.Int64

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				id := counter.Add(1)
				item := &db_api.BatchJobPriority{
					ID:  fmt.Sprintf("job-%d", id),
					SLO: slo,
				}
				if err := exch.PQEnqueue(ctx, item); err != nil {
					b.Fatalf("PQEnqueue: %v", err)
				}
			}
		})
	})
}

// BenchmarkPQDequeue measures in-memory priority queue dequeue throughput.
// Uses the mock client because miniredis does not support ZMPOP/BZMPOP.
// Keeps a fixed pool of items and refills when depleted.
func BenchmarkPQDequeue(b *testing.B) {
	b.Run("serial", func(b *testing.B) {
		pq := mockdb.NewMockBatchPriorityQueueClient()
		ctx := context.Background()
		slo := time.Now().Add(24 * time.Hour)

		const poolSize = 1000
		var seq int
		populate := func() {
			for range poolSize {
				item := &db_api.BatchJobPriority{
					ID:  fmt.Sprintf("job-%d", seq),
					SLO: slo.Add(time.Duration(seq) * time.Microsecond),
				}
				_ = pq.PQEnqueue(ctx, item)
				seq++
			}
		}
		populate()
		remaining := poolSize

		b.ResetTimer()
		for range b.N {
			if remaining == 0 {
				b.StopTimer()
				populate()
				remaining = poolSize
				b.StartTimer()
			}
			_, err := pq.PQDequeue(ctx, 0, 1)
			if err != nil {
				b.Fatalf("PQDequeue: %v", err)
			}
			remaining--
		}
	})
}

// BenchmarkPQMixed measures concurrent enqueue/dequeue throughput
// using the mock client.
func BenchmarkPQMixed(b *testing.B) {
	pq := mockdb.NewMockBatchPriorityQueueClient()
	ctx := context.Background()
	slo := time.Now().Add(24 * time.Hour)

	// Pre-populate so dequeuers have items to consume.
	for i := range 1000 {
		item := &db_api.BatchJobPriority{
			ID:  fmt.Sprintf("pre-%d", i),
			SLO: slo.Add(time.Duration(i) * time.Microsecond),
		}
		if err := pq.PQEnqueue(ctx, item); err != nil {
			b.Fatalf("PQEnqueue (pre-populate): %v", err)
		}
	}

	var enqCounter atomic.Int64

	b.ResetTimer()

	var wg sync.WaitGroup

	// Enqueue goroutines.
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				id := enqCounter.Add(1)
				if id > int64(b.N) {
					return
				}
				item := &db_api.BatchJobPriority{
					ID:  fmt.Sprintf("mix-enq-%d", id),
					SLO: slo,
				}
				_ = pq.PQEnqueue(ctx, item)
			}
		}()
	}

	// Dequeue goroutines.
	var deqCount atomic.Int64
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for deqCount.Add(1) <= int64(b.N) {
				_, _ = pq.PQDequeue(ctx, 0, 1)
			}
		}()
	}

	wg.Wait()
}
