//go:build bench

package preprocessor_test

import (
	"context"
	"fmt"
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

var models = []string{"model-a", "model-b", "model-c"}

func BenchmarkPreprocessor(b *testing.B) {
	for _, tc := range []struct {
		name      string
		lineCount int
	}{
		{"1K_lines", 1_000},
		{"10K_lines", 10_000},
		{"50K_lines", 50_000},
	} {
		b.Run(tc.name, func(b *testing.B) {
			benchPreprocess(b, tc.lineCount)
		})
	}
}

func benchPreprocess(b *testing.B, lineCount int) {
	b.Helper()
	ctx := context.Background()

	cfg := config.NewConfig()
	cfg.WorkDir = b.TempDir()

	filesClient := mockfiles.NewMockBatchFilesClient(b.TempDir())
	fileDBClient := benchhelpers.NewMockFileDBClient()

	inferClient := &benchhelpers.MockInferenceClient{}
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

	inputFileID := "bench-input-file"
	tenantID := "bench-tenant"
	inputData := benchhelpers.GenJSONLInput(lineCount, models)

	benchhelpers.SeedInputFile(b, filesClient, fileDBClient, inputFileID, tenantID, inputData)

	b.ResetTimer()
	start := time.Now()

	for i := range b.N {
		jobID := fmt.Sprintf("bench-job-%d", i)
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
			b.Fatalf("PreProcessJobForTest: %v", err)
		}
	}

	b.StopTimer()
	elapsed := time.Since(start)
	b.ReportMetric(float64(lineCount)*float64(b.N)/elapsed.Seconds(), "lines/sec")
	b.ReportMetric(float64(len(inputData))*float64(b.N)/elapsed.Seconds()/(1024*1024), "MB/sec")
}
