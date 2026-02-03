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

	"github.com/llm-d-incubation/batch-gateway/internal/database/api"
)

type MockBatchDBClient struct {
	jobs sync.Map
}

func NewMockBatchDBClient() *MockBatchDBClient {
	return &MockBatchDBClient{}
}

func (m *MockBatchDBClient) DBStore(ctx context.Context, job *api.BatchItem) (string, error) {
	m.jobs.Store(job.ID, job)
	return job.ID, nil
}

func (m *MockBatchDBClient) DBGet(
	ctx context.Context, query *api.BatchDBQuery,
	includeStatic bool, start, limit int) (
	[]*api.BatchItem, int, bool, error) {
	var results []*api.BatchItem

	// If IDs are specified, get by IDs
	if len(query.IDs) > 0 {
		for _, id := range query.IDs {
			if value, ok := m.jobs.Load(id); ok {
				if job, ok := value.(*api.BatchItem); ok {
					results = append(results, job)
				}
			}
		}
	} else {
		m.jobs.Range(func(key, value any) bool {
			if job, ok := value.(*api.BatchItem); ok {
				results = append(results, job)
				if len(results) >= limit && limit > 0 {
					return false
				}
			}
			return true
		})
	}

	return results, 0, false, nil
}

func (m *MockBatchDBClient) DBUpdate(
	ctx context.Context, job *api.BatchItem) error {
	if _, ok := m.jobs.Load(job.ID); !ok {
		return fmt.Errorf("cannot update job with ID '%s': job doesn't exist", job.ID)
	}
	m.jobs.Store(job.ID, job)
	return nil
}

func (m *MockBatchDBClient) DBDelete(ctx context.Context, IDs []string) ([]string, error) {
	var deleted []string
	for _, id := range IDs {
		if _, ok := m.jobs.LoadAndDelete(id); ok {
			deleted = append(deleted, id)
		}
	}
	return deleted, nil
}

func (m *MockBatchDBClient) GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parentCtx, timeLimit)
}

func (m *MockBatchDBClient) Close() error {
	m.jobs.Clear()
	return nil
}
