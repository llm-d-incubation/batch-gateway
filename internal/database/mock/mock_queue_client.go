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

// The file provides in-memory mock implementations for BatchDBClient.
package mock

import (
	"context"
	"sync"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/database/api"
)

// Compile-time check: MockBatchPriorityQueueClient implements api.BatchPriorityQueueClient.
var _ api.BatchPriorityQueueClient = (*MockBatchPriorityQueueClient)(nil)

type MockBatchPriorityQueueClient struct {
	mu    sync.Mutex
	queue []*api.BatchJobPriority
}

func NewMockBatchPriorityQueueClient() *MockBatchPriorityQueueClient {
	return &MockBatchPriorityQueueClient{
		queue: make([]*api.BatchJobPriority, 0),
	}
}

func (m *MockBatchPriorityQueueClient) PQEnqueue(ctx context.Context, jobPriority *api.BatchJobPriority) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Insert in sorted order by SLO (earlier SLO = higher priority)
	insertIdx := len(m.queue)
	for i, jp := range m.queue {
		if jobPriority.SLO.Before(jp.SLO) {
			insertIdx = i
			break
		}
	}

	// Insert at the correct position
	m.queue = append(m.queue, nil)
	copy(m.queue[insertIdx+1:], m.queue[insertIdx:])
	m.queue[insertIdx] = jobPriority

	return nil
}

func (m *MockBatchPriorityQueueClient) PQDequeue(ctx context.Context, timeout time.Duration, maxObjs int) ([]*api.BatchJobPriority, error) {
	deadline := time.Now().Add(timeout)

	for {
		m.mu.Lock()
		if len(m.queue) > 0 {
			// Determine how many objects to return
			count := min(maxObjs, len(m.queue))

			// Get the first 'count' items (highest priority)
			result := make([]*api.BatchJobPriority, count)
			copy(result, m.queue[:count])

			// Remove them from the queue
			m.queue = m.queue[count:]

			m.mu.Unlock()
			return result, nil
		}
		m.mu.Unlock()

		// If timeout is zero, return immediately
		if timeout == 0 {
			return []*api.BatchJobPriority{}, nil
		}

		// Check if we've exceeded the timeout
		if time.Now().After(deadline) {
			return []*api.BatchJobPriority{}, nil
		}

		// Check if context is cancelled
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
			// Small sleep before checking again
		}
	}
}

func (m *MockBatchPriorityQueueClient) PQDelete(ctx context.Context, jobPriority *api.BatchJobPriority) (nDeleted int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, jp := range m.queue {
		if jp.ID == jobPriority.ID {
			// Remove the item
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
			return 1, nil
		}
	}

	return 0, nil
}

func (m *MockBatchPriorityQueueClient) GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parentCtx, timeLimit)
}

func (m *MockBatchPriorityQueueClient) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.queue = nil
	return nil
}
