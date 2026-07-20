package progress_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	mockdb "github.com/llm-d/llm-d-batch-gateway/internal/database/mock"
	"github.com/llm-d/llm-d-batch-gateway/internal/processor/pipeline"
	"github.com/llm-d/llm-d-batch-gateway/internal/processor/worker"
	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
	benchhelpers "github.com/llm-d/llm-d-batch-gateway/test/benchmark"
)

type noopUpdater struct{}

func (noopUpdater) UpdateProgressCounts(_ context.Context, _ string, _ *openai.BatchRequestCounts) error {
	return nil
}

func BenchmarkProgressTracker(b *testing.B) {
	b.Run("record_serial", func(b *testing.B) {
		tracker := pipeline.NewProgressTracker(
			int64(b.N), noopUpdater{}, "bench-job", time.Hour, benchhelpers.DiscardLogger(),
		)
		b.ResetTimer()
		for range b.N {
			tracker.RecordSuccess(pipeline.ResultItem{})
		}
	})

	b.Run("record_parallel", func(b *testing.B) {
		tracker := pipeline.NewProgressTracker(
			int64(b.N), noopUpdater{}, "bench-job", time.Hour, benchhelpers.DiscardLogger(),
		)
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				tracker.RecordSuccess(pipeline.ResultItem{})
			}
		})
	})
}

func BenchmarkUpdateProgressCounts(b *testing.B) {
	b.Run("serial", func(b *testing.B) {
		dbClient := benchhelpers.NewMockBatchDBClient()
		statusClient := mockdb.NewMockBatchStatusClient()
		updater := worker.NewStatusUpdater(dbClient, statusClient, 86400)
		counts := &openai.BatchRequestCounts{Total: 1000, Completed: 500, Failed: 0}
		ctx := context.Background()

		b.ResetTimer()
		for range b.N {
			if err := updater.UpdateProgressCounts(ctx, "bench-job", counts); err != nil {
				b.Fatalf("UpdateProgressCounts: %v", err)
			}
		}
	})

	b.Run("parallel", func(b *testing.B) {
		dbClient := benchhelpers.NewMockBatchDBClient()
		statusClient := mockdb.NewMockBatchStatusClient()
		updater := worker.NewStatusUpdater(dbClient, statusClient, 86400)
		counts := &openai.BatchRequestCounts{Total: 1000, Completed: 500, Failed: 0}
		ctx := context.Background()
		var counter atomic.Int64

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				jobID := fmt.Sprintf("bench-job-%d", counter.Add(1))
				if err := updater.UpdateProgressCounts(ctx, jobID, counts); err != nil {
					b.Fatalf("UpdateProgressCounts: %v", err)
				}
			}
		})
	})
}
