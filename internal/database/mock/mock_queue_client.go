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
	"fmt"
	"sync"
	"time"

	"github.com/llm-d/llm-d-batch-gateway/internal/database/api"
)

// Compile-time check: MockBatchPriorityQueueClient implements api.BatchPriorityQueueClient.
var _ api.BatchPriorityQueueClient = (*MockBatchPriorityQueueClient)(nil)

type MockBatchPriorityQueueClient struct {
	mu       sync.Mutex
	queue    []*api.BatchJobPriority
	inflight api.InFlightClient
}

func NewMockBatchPriorityQueueClient() *MockBatchPriorityQueueClient {
	return &MockBatchPriorityQueueClient{
		queue: make([]*api.BatchJobPriority, 0),
	}
}

// LinkInFlight connects an in-flight client so PQDequeueAndClaim records claims
// in it.
func (m *MockBatchPriorityQueueClient) LinkInFlight(inflight api.InFlightClient) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inflight = inflight
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

func (m *MockBatchPriorityQueueClient) PQDequeueAndClaim(ctx context.Context, processorID string) (*api.BatchJobPriority, error) {
	if processorID == "" {
		return nil, fmt.Errorf("processorID is empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.queue) == 0 {
		return nil, nil
	}
	item := m.queue[0]
	if item == nil {
		return nil, fmt.Errorf("queued item is nil")
	}
	if m.inflight == nil {
		return nil, fmt.Errorf("in-flight client is missing")
	}
	if err := m.inflight.InFlightSet(ctx, item.ID, processorID); err != nil {
		return nil, err
	}
	m.queue = m.queue[1:]

	return item, nil
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

func (m *MockBatchPriorityQueueClient) PQGetIDs(ctx context.Context) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make(map[string]bool, len(m.queue))
	for _, jp := range m.queue {
		ids[jp.ID] = true
	}
	return ids, nil
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
