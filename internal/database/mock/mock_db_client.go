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

// The file provides in-memory mock implementations for DBClient.
package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/database/api"
)

// MockDBClient is a generic in-memory implementation of api.DBClient[T] for testing.
type MockDBClient[T any] struct {
	items sync.Map
}

// NewMockDBClient creates a new mock DB client.
func NewMockDBClient[T any]() *MockDBClient[T] {
	return &MockDBClient[T]{}
}

func (m *MockDBClient[T]) DBStore(ctx context.Context, item *api.BaseItem[T]) error {
	if item == nil {
		return fmt.Errorf("item is nil")
	}
	if item.ID == "" {
		return fmt.Errorf("item has empty ID")
	}
	m.items.Store(item.ID, item)
	return nil
}

func (m *MockDBClient[T]) DBGet(
	ctx context.Context, query *api.Query,
	includeStatic bool, start, limit int) (
	[]*api.BaseItem[T], int, bool, error) {
	var allMatches []*api.BaseItem[T]

	// If IDs are specified, get by IDs
	if len(query.IDs) > 0 {
		for _, id := range query.IDs {
			if value, ok := m.items.Load(id); ok {
				if item, ok := value.(*api.BaseItem[T]); ok {
					allMatches = append(allMatches, item)
				}
			}
		}
	} else {
		// Collect all items, applying filters
		m.items.Range(func(key, value any) bool {
			if item, ok := value.(*api.BaseItem[T]); ok {
				// Filter by tenant if specified
				if query.TenantID != "" && item.TenantID != query.TenantID {
					return true
				}
				// Filter by tag selectors if specified
				if len(query.TagSelectors) > 0 {
					matches := true
					for tagKey, tagValue := range query.TagSelectors {
						if itemTagValue, ok := item.Tags[tagKey]; !ok || itemTagValue != tagValue {
							matches = false
							break
						}
					}
					if !matches {
						return true
					}
				}
				allMatches = append(allMatches, item)
			}
			return true
		})
	}

	// Handle pagination
	totalMatches := len(allMatches)

	if start >= totalMatches {
		return []*api.BaseItem[T]{}, start, false, nil
	}

	allMatches = allMatches[start:]

	var results []*api.BaseItem[T]
	expectedMore := false
	if limit > 0 && len(allMatches) > limit {
		results = allMatches[:limit]
		expectedMore = true
	} else {
		results = allMatches
	}

	nextCursor := start + len(results)

	return results, nextCursor, expectedMore, nil
}

func (m *MockDBClient[T]) DBUpdate(ctx context.Context, item *api.BaseItem[T]) error {
	if item == nil {
		return fmt.Errorf("item is nil")
	}
	if item.ID == "" {
		return fmt.Errorf("item has empty ID")
	}
	if _, ok := m.items.Load(item.ID); !ok {
		return fmt.Errorf("cannot update item with ID '%s': item doesn't exist", item.ID)
	}
	m.items.Store(item.ID, item)
	return nil
}

func (m *MockDBClient[T]) DBDelete(ctx context.Context, IDs []string) ([]string, error) {
	var deleted []string
	for _, id := range IDs {
		if _, ok := m.items.LoadAndDelete(id); ok {
			deleted = append(deleted, id)
		}
	}
	return deleted, nil
}

func (m *MockDBClient[T]) GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parentCtx, timeLimit)
}

func (m *MockDBClient[T]) Close() error {
	m.items.Clear()
	return nil
}
