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
	"reflect"
	"sync"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/database/api"
)

// MockDBClient is a generic in-memory implementation of api.DBClient[T] for testing.
type MockDBClient[T any] struct {
	items    sync.Map
	idGetter func(*T) string
}

// NewMockDBClient creates a new mock DB client.
// idGetter is a function that extracts the ID from the item pointer.
func NewMockDBClient[T any](idGetter func(*T) string) *MockDBClient[T] {
	return &MockDBClient[T]{
		idGetter: idGetter,
	}
}

func (m *MockDBClient[T]) DBStore(ctx context.Context, item *T) error {
	if item == nil {
		return fmt.Errorf("item is nil")
	}
	id := m.idGetter(item)
	if id == "" {
		return fmt.Errorf("item has empty ID")
	}
	m.items.Store(id, item)
	return nil
}

func (m *MockDBClient[T]) DBGet(
	ctx context.Context, query *api.Query,
	includeStatic bool, start, limit int) (
	[]*T, int, bool, error) {
	var allMatches []*T

	// If IDs are specified, get by IDs
	if len(query.IDs) > 0 {
		for _, id := range query.IDs {
			if value, ok := m.items.Load(id); ok {
				if item, ok := value.(*T); ok {
					if m.matchesFilters(*item, query) {
						allMatches = append(allMatches, item)
					}
				}
			}
		}
	} else {
		// Collect all items, applying filters
		m.items.Range(func(key, value any) bool {
			if item, ok := value.(*T); ok {
				if m.matchesFilters(*item, query) {
					allMatches = append(allMatches, item)
				}
			}
			return true
		})
	}

	// Handle pagination
	totalMatches := len(allMatches)

	if start >= totalMatches {
		return []*T{}, start, false, nil
	}

	allMatches = allMatches[start:]

	var results []*T
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

func (m *MockDBClient[T]) DBUpdate(ctx context.Context, item *T) error {
	if item == nil {
		return fmt.Errorf("item is nil")
	}
	id := m.idGetter(item)
	if id == "" {
		return fmt.Errorf("item has empty ID")
	}
	if _, ok := m.items.Load(id); !ok {
		return fmt.Errorf("cannot update item with ID '%s': item doesn't exist", id)
	}
	m.items.Store(id, item)
	return nil
}

func (m *MockDBClient[T]) DBDelete(ctx context.Context, ids []string) ([]string, error) {
	var deleted []string
	for _, id := range ids {
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

// matchesFilters checks if an item matches the query filters.
func (m *MockDBClient[T]) matchesFilters(item T, query *api.Query) bool {
	val := reflect.ValueOf(item)

	// Filter by tenant if specified
	if query.TenantID != "" {
		tenantIDField := val.FieldByName("TenantID")
		if tenantIDField.IsValid() && tenantIDField.Kind() == reflect.String {
			if tenantIDField.String() != query.TenantID {
				return false
			}
		}
	}

	// Filter by tag selectors if specified
	if len(query.TagSelectors) > 0 {
		tagsField := val.FieldByName("Tags")
		if tagsField.IsValid() && tagsField.Kind() == reflect.Map {
			itemTags, ok := tagsField.Interface().(api.Tags)
			if !ok {
				return false
			}
			for tagKey, tagValue := range query.TagSelectors {
				if itemTagValue, ok := itemTags[tagKey]; !ok || itemTagValue != tagValue {
					return false
				}
			}
		} else {
			return false
		}
	}

	return true
}
