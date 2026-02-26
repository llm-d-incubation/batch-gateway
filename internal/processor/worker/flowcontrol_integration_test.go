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

package worker

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/klog/v2"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/database/mock"
	"github.com/llm-d-incubation/batch-gateway/internal/inference"
	inferencemetrics "github.com/llm-d-incubation/batch-gateway/internal/inference/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
)

// Mock inference client for testing
type mockInferenceClient struct{}

func (m *mockInferenceClient) Generate(ctx context.Context, req *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
	return &inference.GenerateResponse{RequestID: req.RequestID}, nil
}

// Mock metrics client that always returns an error
type errorMetricsClient struct {
	err error
}

func (e *errorMetricsClient) Budget(ctx context.Context) (float64, error) {
	return 0, e.err
}

// trackingQueueClient wraps a queue client and tracks PQDequeue calls
type trackingQueueClient struct {
	db.BatchPriorityQueueClient
	dequeueCount atomic.Int64
}

func (t *trackingQueueClient) PQDequeue(ctx context.Context, timeout time.Duration, maxObjs int) ([]*db.BatchJobPriority, error) {
	t.dequeueCount.Add(1)
	return t.BatchPriorityQueueClient.PQDequeue(ctx, timeout, maxObjs)
}

func TestFlowControl_ProcessorWithZeroBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()

		// Create mock database clients
		mockDB := mock.NewMockDBClient[db.BatchItem, db.BatchQuery](
			func(b *db.BatchItem) string { return b.ID },
			func(q *db.BatchQuery) *db.BaseQuery { return &q.BaseQuery },
		)
		mockQueue := &trackingQueueClient{
			BatchPriorityQueueClient: mock.NewMockBatchPriorityQueueClient(),
		}
		mockStatus := mock.NewMockBatchStatusClient()
		mockEvent := mock.NewMockBatchEventChannelClient()
		mockInference := &mockInferenceClient{}

		// Create processor clients
		clients := NewProcessorClients(mockDB, mockQueue, mockStatus, mockEvent, mockInference)

		// Test with budget=0 - should NOT pull from queue
		metricsClient := &inferencemetrics.NoopClient{Value: 0.0}

		cfg := &config.ProcessorConfig{
			NumWorkers:        1,
			PollInterval:      100 * time.Millisecond,
			MaxJobConcurrency: 5,
		}

		proc := NewProcessor(cfg, &clients, metricsClient)

		// Start processor in background with timeout
		procCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()

		go proc.RunPollingLoop(procCtx)

		// Let it run for a bit (should do 2-3 poll cycles)
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()

		// With budget=0, queue should NOT have been pulled
		require.Equal(t, int64(0), mockQueue.dequeueCount.Load(), "queue should not be pulled when budget=0")
	})
}

func TestFlowControl_ProcessorWithFullBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()

		// Create mock database clients
		mockDB := mock.NewMockDBClient[db.BatchItem, db.BatchQuery](
			func(b *db.BatchItem) string { return b.ID },
			func(q *db.BatchQuery) *db.BaseQuery { return &q.BaseQuery },
		)
		mockQueue := &trackingQueueClient{
			BatchPriorityQueueClient: mock.NewMockBatchPriorityQueueClient(),
		}
		mockStatus := mock.NewMockBatchStatusClient()
		mockEvent := mock.NewMockBatchEventChannelClient()
		mockInference := &mockInferenceClient{}

		// Create processor clients
		clients := NewProcessorClients(mockDB, mockQueue, mockStatus, mockEvent, mockInference)

		// Test with budget=1 - should pull from queue
		metricsClient := &inferencemetrics.NoopClient{Value: 1.0}

		cfg := &config.ProcessorConfig{
			NumWorkers:        1,
			PollInterval:      100 * time.Millisecond,
			MaxJobConcurrency: 5,
		}

		proc := NewProcessor(cfg, &clients, metricsClient)

		// Start processor in background with timeout
		procCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()

		go proc.RunPollingLoop(procCtx)

		// Let it run for a bit (should do 2-3 poll cycles)
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()

		// With budget=1, queue should have been pulled at least once
		require.Greater(t, mockQueue.dequeueCount.Load(), int64(0), "queue should be pulled when budget=1")
	})
}

func TestFlowControl_ProcessorWithMetricsError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Initialize klog flags and capture output
		var logBuf bytes.Buffer
		klog.InitFlags(nil)
		flag.Parse()
		_ = flag.Set("v", "2")
		klog.LogToStderr(false)
		klog.SetOutput(&logBuf)

		// Set verbosity level to capture WARNING level logs (level 2)

		ctx := context.Background()

		// Create mock database clients
		mockDB := mock.NewMockDBClient[db.BatchItem, db.BatchQuery](
			func(b *db.BatchItem) string { return b.ID },
			func(q *db.BatchQuery) *db.BaseQuery { return &q.BaseQuery },
		)
		mockQueue := &trackingQueueClient{
			BatchPriorityQueueClient: mock.NewMockBatchPriorityQueueClient(),
		}
		mockStatus := mock.NewMockBatchStatusClient()
		mockEvent := mock.NewMockBatchEventChannelClient()
		mockInference := &mockInferenceClient{}

		// Create processor clients
		clients := NewProcessorClients(mockDB, mockQueue, mockStatus, mockEvent, mockInference)

		// Test with error-returning metrics client - should still pull from queue
		// (defaults to budget=1.0 on error)
		metricsClient := &errorMetricsClient{err: fmt.Errorf("metrics service unavailable")}

		cfg := &config.ProcessorConfig{
			NumWorkers:        1,
			PollInterval:      100 * time.Millisecond,
			MaxJobConcurrency: 5,
		}

		proc := NewProcessor(cfg, &clients, metricsClient)

		// Start processor in background with timeout
		procCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()

		go proc.RunPollingLoop(procCtx)

		// Let it run for a bit (should do 2-3 poll cycles)
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()

		// Flush logs to ensure they're written to the buffer
		klog.Flush()

		// Even with metrics error, queue should still be pulled (defaults to full capacity)
		require.Greater(t, mockQueue.dequeueCount.Load(), int64(0), "queue should be pulled even when metrics fail (defaults to budget=1.0)")

		// Verify warning was logged
		logOutput := logBuf.String()
		require.Contains(t, logOutput, "Failed to get dispatch budget, assuming full capacity", "should log warning when metrics fail")
	})
}
