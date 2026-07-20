//go:build bench

package executor_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	mockdb "github.com/llm-d/llm-d-batch-gateway/internal/database/mock"
	mockfiles "github.com/llm-d/llm-d-batch-gateway/internal/files_store/mock"
	"github.com/llm-d/llm-d-batch-gateway/internal/processor/config"
	"github.com/llm-d/llm-d-batch-gateway/internal/processor/worker"
	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d/llm-d-batch-gateway/internal/shared/types"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/clientset"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
	benchhelpers "github.com/llm-d/llm-d-batch-gateway/test/benchmark"
)

var models = []string{"model-a"}

// timingInferenceClient wraps MockInferenceClient and records per-request
// latency (including semaphore wait time visible from the caller's side).
type timingInferenceClient struct {
	mu        sync.Mutex
	inner     benchhelpers.MockInferenceClient
	latencies []time.Duration
}

func (t *timingInferenceClient) Generate(ctx context.Context, req *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
	start := time.Now()
	resp, err := t.inner.Generate(ctx, req)
	elapsed := time.Since(start)
	t.mu.Lock()
	t.latencies = append(t.latencies, elapsed)
	t.mu.Unlock()
	return resp, err
}

func (t *timingInferenceClient) reset() {
	t.mu.Lock()
	t.latencies = t.latencies[:0]
	t.mu.Unlock()
}

func (t *timingInferenceClient) percentiles() (p50, p99 time.Duration) {
	t.mu.Lock()
	sorted := make([]time.Duration, len(t.latencies))
	copy(sorted, t.latencies)
	t.mu.Unlock()

	if len(sorted) == 0 {
		return 0, 0
	}
	slices.Sort(sorted)
	p50 = sorted[len(sorted)*50/100]
	p99 = sorted[len(sorted)*99/100]
	return p50, p99
}

func BenchmarkExecutor(b *testing.B) {
	for _, rc := range []struct {
		name  string
		count int
	}{
		{"1K", 1_000},
		{"10K", 10_000},
	} {
		for _, wc := range []struct {
			name    string
			workers int
		}{
			{"workers_1", 1},
			{"workers_10", 10},
			{"workers_50", 50},
		} {
			name := fmt.Sprintf("%s/%s", rc.name, wc.name)
			b.Run(name, func(b *testing.B) {
				benchExecute(b, rc.count, wc.workers)
			})
		}
	}
}

func benchExecute(b *testing.B, requestCount, workers int) {
	b.Helper()
	ctx := context.Background()

	cfg := config.NewConfig()
	cfg.WorkDir = b.TempDir()
	cfg.NumWorkers = workers
	cfg.Concurrency.Global = workers
	cfg.Concurrency.PerEndpoint = workers

	inferClient := &timingInferenceClient{}
	filesClient := mockfiles.NewMockBatchFilesClient(b.TempDir())
	fileDBClient := benchhelpers.NewMockFileDBClient()

	clients := &clientset.Clientset{
		BatchDB:   benchhelpers.NewMockBatchDBClient(),
		FileDB:    fileDBClient,
		File:      filesClient,
		Queue:     mockdb.NewMockBatchPriorityQueueClient(),
		Status:    mockdb.NewMockBatchStatusClient(),
		Event:     mockdb.NewMockBatchEventChannelClient(),
		InFlight:  mockdb.NewMockInFlightClient(),
		Inference: inference.NewSingleClientResolver(inferClient),
	}

	p, err := worker.NewProcessorForTest(cfg, clients, benchhelpers.DiscardLogger())
	if err != nil {
		b.Fatalf("NewProcessorForTest: %v", err)
	}

	tenantID := "bench-tenant"
	inputFileID := "bench-exec-input"
	inputData := benchhelpers.GenJSONLInput(requestCount, models)
	benchhelpers.SeedInputFile(b, filesClient, fileDBClient, inputFileID, tenantID, inputData)

	// Preprocess once per iteration to create plan files and model map.
	for i := range b.N {
		jobID := fmt.Sprintf("exec-job-%d", i)
		jobInfo := &batch_types.JobInfo{
			JobID: jobID,
			BatchJob: &openai.Batch{
				ID: jobID,
				BatchSpec: openai.BatchSpec{
					InputFileID: inputFileID,
				},
				BatchStatusInfo: openai.BatchStatusInfo{
					Status: openai.BatchStatusInProgress,
				},
			},
			TenantID: tenantID,
		}
		if err := p.PreProcessJobForTest(ctx, ctx, ctx, jobInfo); err != nil {
			b.Fatalf("PreProcessJobForTest (setup iter %d): %v", i, err)
		}
	}

	b.ResetTimer()
	start := time.Now()

	for i := range b.N {
		inferClient.reset()

		jobID := fmt.Sprintf("exec-job-%d", i)
		jobInfo := &batch_types.JobInfo{
			JobID: jobID,
			BatchJob: &openai.Batch{
				ID: jobID,
				BatchSpec: openai.BatchSpec{
					InputFileID: inputFileID,
				},
				BatchStatusInfo: openai.BatchStatusInfo{
					Status: openai.BatchStatusInProgress,
				},
			},
			TenantID: tenantID,
		}
		if _, err := p.ExecuteJobForTest(ctx, jobInfo); err != nil {
			b.Fatalf("ExecuteJobForTest: %v", err)
		}
	}

	b.StopTimer()
	elapsed := time.Since(start)
	b.ReportMetric(float64(requestCount)*float64(b.N)/elapsed.Seconds(), "req/sec")

	p50, p99 := inferClient.percentiles()
	b.ReportMetric(float64(p50.Microseconds()), "p50-µs")
	b.ReportMetric(float64(p99.Microseconds()), "p99-µs")
}
