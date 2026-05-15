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

package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/database/api"
)

var _ api.InFlightClient = (*MockInFlightClient)(nil)

type MockInFlightClient struct {
	mu      sync.Mutex
	entries map[string]*api.InFlightEntry
}

func NewMockInFlightClient() *MockInFlightClient {
	return &MockInFlightClient{
		entries: make(map[string]*api.InFlightEntry),
	}
}

func (m *MockInFlightClient) InFlightSet(_ context.Context, jobID, processorID string) error {
	if jobID == "" {
		return fmt.Errorf("jobID is empty")
	}
	if processorID == "" {
		return fmt.Errorf("processorID is empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[jobID] = &api.InFlightEntry{
		ProcessorID: processorID,
		LastSeen:    time.Now().Unix(),
	}
	return nil
}

func (m *MockInFlightClient) InFlightDelete(_ context.Context, jobID string) error {
	if jobID == "" {
		return fmt.Errorf("jobID is empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, jobID)
	return nil
}

func (m *MockInFlightClient) InFlightGetAll(_ context.Context) (map[string]*api.InFlightEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make(map[string]*api.InFlightEntry, len(m.entries))
	for k, v := range m.entries {
		copied := *v
		result[k] = &copied
	}
	return result, nil
}

func (m *MockInFlightClient) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = nil
	return nil
}
