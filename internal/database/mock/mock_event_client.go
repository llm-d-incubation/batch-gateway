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

// The file provides in-memory mock implementations for BatchEventChannelClient.
package mock

import (
	"context"
	"sync"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/database/api"
)

type eventChannel struct {
	ch      chan api.BatchEvent
	closeFn func()
}

// Compile-time check: MockBatchEventChannelClient implements api.BatchEventChannelClient.
var _ api.BatchEventChannelClient = (*MockBatchEventChannelClient)(nil)

type MockBatchEventChannelClient struct {
	mu       sync.RWMutex
	channels map[string][]*eventChannel // Map of job ID to list of event channels
}

func NewMockBatchEventChannelClient() *MockBatchEventChannelClient {
	return &MockBatchEventChannelClient{
		channels: make(map[string][]*eventChannel),
	}
}

func (m *MockBatchEventChannelClient) ECConsumerGetChannel(ctx context.Context, ID string) (*api.BatchEventsChan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Create a new channel for this consumer
	ch := make(chan api.BatchEvent, 100) // Buffered channel

	// Create the event channel struct
	ec := &eventChannel{
		ch: ch,
	}

	// Add close function that will remove this channel from the map
	ec.closeFn = func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		// Find and remove this specific channel
		if channels, exists := m.channels[ID]; exists {
			for i, c := range channels {
				if c == ec {
					// Remove this channel from the slice
					m.channels[ID] = append(channels[:i], channels[i+1:]...)
					break
				}
			}
			// Clean up the map entry if no more channels
			if len(m.channels[ID]) == 0 {
				delete(m.channels, ID)
			}
		}
		close(ch)
	}

	// Add this channel to the map
	m.channels[ID] = append(m.channels[ID], ec)

	// Return the BatchEventsChan
	return &api.BatchEventsChan{
		ID:      ID,
		Events:  ch,
		CloseFn: ec.closeFn,
	}, nil
}

func (m *MockBatchEventChannelClient) ECProducerSendEvents(ctx context.Context, events []api.BatchEvent) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sentIDs := make([]string, 0, len(events))

	for _, event := range events {
		// Get all channels for this job ID
		channels, exists := m.channels[event.ID]
		if !exists || len(channels) == 0 {
			// No consumers for this event, skip it
			continue
		}

		// Send the event to all channels for this job ID
		for _, ec := range channels {
			select {
			case ec.ch <- event:
				// Event sent successfully
			case <-ctx.Done():
				return sentIDs, ctx.Err()
			default:
				// Channel buffer is full, skip this channel
				// In a real implementation, you might want to handle this differently
			}
		}

		sentIDs = append(sentIDs, event.ID)
	}

	return sentIDs, nil
}

func (m *MockBatchEventChannelClient) GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parentCtx, timeLimit)
}

func (m *MockBatchEventChannelClient) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Close all channels
	for _, channels := range m.channels {
		for _, ec := range channels {
			close(ec.ch)
		}
	}

	// Clear the map
	m.channels = make(map[string][]*eventChannel)

	return nil
}
