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

// The file provides in-memory mock implementations for BatchStatusClient.
package mock

import (
	"context"
	"sync"
	"time"
)

type MockBatchStatusClient struct {
	mu     sync.RWMutex
	status map[string][]byte // Map of job ID to status data
}

func NewMockBatchStatusClient() *MockBatchStatusClient {
	return &MockBatchStatusClient{
		status: make(map[string][]byte),
	}
}

func (m *MockBatchStatusClient) StatusSet(ctx context.Context, ID string, TTL int, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Store a copy of the data to avoid external modifications
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	m.status[ID] = dataCopy

	// Note: In a real implementation, TTL would be used to expire the data.
	// For this mock, we'll just store the data without expiration.

	return nil
}

func (m *MockBatchStatusClient) StatusGet(ctx context.Context, ID string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	data, exists := m.status[ID]
	if !exists {
		// If no data exists, return (nil, nil) as per the interface contract
		return nil, nil
	}

	// Return a copy to avoid external modifications
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	return dataCopy, nil
}

func (m *MockBatchStatusClient) StatusDelete(ctx context.Context, ID string) (nDeleted int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.status, ID)

	return 1, nil
}

func (m *MockBatchStatusClient) GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parentCtx, timeLimit)
}

func (m *MockBatchStatusClient) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Clear the status map
	m.status = make(map[string][]byte)

	return nil
}
