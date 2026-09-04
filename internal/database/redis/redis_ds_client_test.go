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

// Test for the redis database client.

package redis_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/alicebob/miniredis/v2"
	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	dbredis "github.com/llm-d/llm-d-batch-gateway/internal/database/redis"
	uredis "github.com/llm-d/llm-d-batch-gateway/internal/util/redis"
	utls "github.com/llm-d/llm-d-batch-gateway/internal/util/tls"
	goredis "github.com/redis/go-redis/v9"
)

func setupRedisDSClients(t *testing.T, redisUrl, redisCaCert string) (
	*dbredis.DSClientRedis, *dbredis.ExchangeDBClientRedis) {
	t.Helper()
	cfg := &uredis.RedisClientConfig{
		Url:         redisUrl,
		ServiceName: "test-service",
	}
	if redisCaCert != "" {
		cfg.EnableTLS = true
		cfg.Certificates = &utls.Certificates{
			CaCertFile: redisCaCert,
		}
	}
	ctx := context.Background()
	baseClient, err := dbredis.NewDSClientRedis(ctx, cfg, 0)
	if err != nil {
		t.Fatalf("Failed to create base redis client: %v", err)
	}
	exchClient, err := dbredis.NewExchangeDBClientRedis(ctx, baseClient, nil, 0)
	if err != nil {
		t.Fatalf("Failed to create exchange redis client: %v", err)
	}
	return baseClient, exchClient
}

func TestRedisDSClient(t *testing.T) {

	redisUrl := os.Getenv("TEST_REDIS_URL")
	redisCaCert := os.Getenv("TEST_REDIS_CACERT_PATH")
	var minirds *miniredis.Miniredis

	// Start miniredis if no external redis URL is provided.
	if redisUrl == "" {
		minirds = miniredis.NewMiniRedis()
		if err := minirds.Start(); err != nil {
			t.Fatalf("Failed to start miniredis: %v", err)
		}
		redisUrl = "redis://" + minirds.Addr()
		t.Cleanup(func() {
			minirds.Close()
		})
	}

	t.Run("Create clients", func(t *testing.T) {
		t.Parallel()
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})
		if baseClient == nil || exchClient == nil {
			t.Fatalf("Expected redis clients to be non-nil")
		}
	})

	t.Run("Event exchange operations", func(t *testing.T) {
		t.Parallel()
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Get event channel.
		ID := uuid.New().String()
		ec, err := exchClient.ECConsumerGetChannel(context.Background(), ID)
		if err != nil {
			t.Fatalf("Failed to get event consumer channel: %v", err)
		}
		if ec == nil {
			t.Fatalf("Invalid event consumer channel")
			return
		}
		if ec.ID != ID {
			t.Fatalf("Mismatch ID %s != %s", ec.ID, ID)
		}
		defer ec.CloseFn()

		// Send events.
		events := []db_api.BatchEvent{
			{
				ID:   ID,
				Type: db_api.BatchEventCancel,
				TTL:  1000,
			},
			{
				ID:   ID,
				Type: db_api.BatchEventPause,
				TTL:  1000,
			},
		}
		sentIDs, err := exchClient.ECProducerSendEvents(context.Background(), events)
		if err != nil {
			t.Fatalf("Failed to send events: %v", err)
		}
		if len(sentIDs) != 1 {
			t.Fatalf("invalid number of returned IDs %d", len(sentIDs))
		}
		if sentIDs[0] != ID {
			t.Fatalf("Mismatch ID %s != %s", sentIDs[0], ID)
		}

		// Get the events.
		for _, evo := range events {
			select {
			case evc := <-ec.Events:
				isSameEvent(t, &evo, &evc)
			case <-time.After(1 * time.Second):
				t.Fatalf("Event channel timeout")
			}
		}
	})

	t.Run("Event exchange operations - Negative cases", func(t *testing.T) {
		t.Parallel()
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Send empty events array.
		sentIDs, err := exchClient.ECProducerSendEvents(context.Background(), []db_api.BatchEvent{})
		if err == nil {
			t.Fatalf("Expected error when sending empty events array")
		}
		if len(sentIDs) != 0 {
			t.Fatalf("Expected 0 sent IDs for empty events, got %d", len(sentIDs))
		}

		// Send event with empty ID.
		invalidEvent1 := []db_api.BatchEvent{
			{
				ID:   "",
				Type: db_api.BatchEventCancel,
				TTL:  1000,
			},
		}
		sentIDs, err = exchClient.ECProducerSendEvents(context.Background(), invalidEvent1)
		if err == nil {
			t.Fatalf("Expected error when sending event with empty ID")
		}
		if len(sentIDs) != 0 {
			t.Fatalf("Expected 0 sent IDs for invalid event, got %d", len(sentIDs))
		}

		// Send event with invalid Type (negative).
		invalidEvent2 := []db_api.BatchEvent{
			{
				ID:   uuid.New().String(),
				Type: db_api.BatchEventType(-1),
				TTL:  1000,
			},
		}
		sentIDs, err = exchClient.ECProducerSendEvents(context.Background(), invalidEvent2)
		if err == nil {
			t.Fatalf("Expected error when sending event with negative Type")
		}
		if len(sentIDs) != 0 {
			t.Fatalf("Expected 0 sent IDs for invalid event, got %d", len(sentIDs))
		}

		// Send event with invalid Type (>= BatchEventMaxVal).
		invalidEvent3 := []db_api.BatchEvent{
			{
				ID:   uuid.New().String(),
				Type: db_api.BatchEventMaxVal,
				TTL:  1000,
			},
		}
		sentIDs, err = exchClient.ECProducerSendEvents(context.Background(), invalidEvent3)
		if err == nil {
			t.Fatalf("Expected error when sending event with Type >= BatchEventMaxVal")
		}
		if len(sentIDs) != 0 {
			t.Fatalf("Expected 0 sent IDs for invalid event, got %d", len(sentIDs))
		}

		// Send event with TTL = 0.
		invalidEvent4 := []db_api.BatchEvent{
			{
				ID:   uuid.New().String(),
				Type: db_api.BatchEventCancel,
				TTL:  0,
			},
		}
		sentIDs, err = exchClient.ECProducerSendEvents(context.Background(), invalidEvent4)
		if err == nil {
			t.Fatalf("Expected error when sending event with TTL=0")
		}
		if len(sentIDs) != 0 {
			t.Fatalf("Expected 0 sent IDs for invalid event, got %d", len(sentIDs))
		}

		// Send event with negative TTL.
		invalidEvent5 := []db_api.BatchEvent{
			{
				ID:   uuid.New().String(),
				Type: db_api.BatchEventCancel,
				TTL:  -100,
			},
		}
		sentIDs, err = exchClient.ECProducerSendEvents(context.Background(), invalidEvent5)
		if err == nil {
			t.Fatalf("Expected error when sending event with negative TTL")
		}
		if len(sentIDs) != 0 {
			t.Fatalf("Expected 0 sent IDs for invalid event, got %d", len(sentIDs))
		}
	})

	t.Run("Event exchange operations - Edge case: all event types", func(t *testing.T) {
		t.Parallel()
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Test all event types (Cancel, Pause, Resume).
		ID := uuid.New().String()
		ec, err := exchClient.ECConsumerGetChannel(context.Background(), ID)
		if err != nil {
			t.Fatalf("Failed to get event consumer channel: %v", err)
		}

		allEventTypes := []db_api.BatchEvent{
			{ID: ID, Type: db_api.BatchEventCancel, TTL: 1000},
			{ID: ID, Type: db_api.BatchEventPause, TTL: 1000},
			{ID: ID, Type: db_api.BatchEventResume, TTL: 1000},
		}
		sentIDs, err := exchClient.ECProducerSendEvents(context.Background(), allEventTypes)
		if err != nil {
			t.Fatalf("Failed to send all event types: %v", err)
		}
		if len(sentIDs) != 1 {
			t.Fatalf("Expected 1 sent ID, got %d", len(sentIDs))
		}

		// Receive all event types.
		for _, expectedEvent := range allEventTypes {
			select {
			case receivedEvent := <-ec.Events:
				isSameEvent(t, &expectedEvent, &receivedEvent)
			case <-time.After(2 * time.Second):
				t.Fatalf("Timeout waiting for event type %d", expectedEvent.Type)
			}
		}
		ec.CloseFn() // Close immediately after use
	})

	t.Run("Event exchange operations - Edge case: pre-created events", func(t *testing.T) {
		t.Parallel()
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Send events before creating consumer channel.
		ID := uuid.New().String()
		earlyEvents := []db_api.BatchEvent{
			{ID: ID, Type: db_api.BatchEventCancel, TTL: 1000},
			{ID: ID, Type: db_api.BatchEventPause, TTL: 1000},
		}
		sentIDs, err := exchClient.ECProducerSendEvents(context.Background(), earlyEvents)
		if err != nil {
			t.Fatalf("Failed to send early events: %v", err)
		}
		if len(sentIDs) != 1 {
			t.Fatalf("Expected 1 sent ID for early events, got %d", len(sentIDs))
		}

		// Create consumer channel after events were sent.
		ec, err := exchClient.ECConsumerGetChannel(context.Background(), ID)
		if err != nil {
			t.Fatalf("Failed to get event consumer channel after sending events: %v", err)
		}

		// Should receive events that were sent before channel creation.
		for _, expectedEvent := range earlyEvents {
			select {
			case receivedEvent := <-ec.Events:
				isSameEvent(t, &expectedEvent, &receivedEvent)
			case <-time.After(2 * time.Second):
				t.Fatalf("Timeout waiting for early event type %d", expectedEvent.Type)
			}
		}
		ec.CloseFn() // Close immediately after use
	})

	t.Run("Event exchange operations - Edge case: multi-ID routing", func(t *testing.T) {
		t.Parallel()
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Send multiple events to different IDs.
		ID3 := uuid.New().String()
		ID4 := uuid.New().String()
		ec3, err := exchClient.ECConsumerGetChannel(context.Background(), ID3)
		if err != nil {
			t.Fatalf("Failed to get event consumer channel for ID3: %v", err)
		}

		ec4, err := exchClient.ECConsumerGetChannel(context.Background(), ID4)
		if err != nil {
			t.Fatalf("Failed to get event consumer channel for ID4: %v", err)
		}

		multiIDEvents := []db_api.BatchEvent{
			{ID: ID3, Type: db_api.BatchEventCancel, TTL: 1000},
			{ID: ID4, Type: db_api.BatchEventPause, TTL: 1000},
			{ID: ID3, Type: db_api.BatchEventResume, TTL: 1000},
		}
		sentIDs, err := exchClient.ECProducerSendEvents(context.Background(), multiIDEvents)
		if err != nil {
			t.Fatalf("Failed to send events to multiple IDs: %v", err)
		}
		if len(sentIDs) != 2 {
			t.Fatalf("Expected 2 sent IDs (ID3 and ID4), got %d", len(sentIDs))
		}

		// Verify events are routed to correct channels.
		receivedID3 := 0
		receivedID4 := 0
		for i := 0; i < 3; i++ {
			select {
			case event := <-ec3.Events:
				if event.ID != ID3 {
					t.Fatalf("Expected event for ID3, got %s", event.ID)
				}
				receivedID3++
			case event := <-ec4.Events:
				if event.ID != ID4 {
					t.Fatalf("Expected event for ID4, got %s", event.ID)
				}
				receivedID4++
			case <-time.After(2 * time.Second):
				t.Fatalf("Timeout waiting for events")
			}
		}
		if receivedID3 != 2 {
			t.Fatalf("Expected 2 events for ID3, got %d", receivedID3)
		}
		if receivedID4 != 1 {
			t.Fatalf("Expected 1 event for ID4, got %d", receivedID4)
		}
		ec3.CloseFn() // Close immediately after use
		ec4.CloseFn() // Close immediately after use
	})

	t.Run("Event exchange operations - Edge case: idempotent close", func(t *testing.T) {
		t.Parallel()
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Test closing consumer channel multiple times (should be idempotent).
		ID := uuid.New().String()
		ec, err := exchClient.ECConsumerGetChannel(context.Background(), ID)
		if err != nil {
			t.Fatalf("Failed to get event consumer channel: %v", err)
		}
		ec.CloseFn()
		ec.CloseFn() // Second close should not panic.
		ec.CloseFn() // Third close should not panic.
	})

	t.Run("Event exchange operations - Edge case: large event set", func(t *testing.T) {
		t.Parallel()
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Test sending large number of events.
		ID := uuid.New().String()
		ec, err := exchClient.ECConsumerGetChannel(context.Background(), ID)
		if err != nil {
			t.Fatalf("Failed to get event consumer channel: %v", err)
		}

		nEvents := 50
		largeEventSet := make([]db_api.BatchEvent, nEvents)
		for i := 0; i < nEvents; i++ {
			largeEventSet[i] = db_api.BatchEvent{
				ID:   ID,
				Type: db_api.BatchEventType(i % 3), // Cycle through Cancel, Pause, Resume
				TTL:  1000,
			}
		}
		sentIDs, err := exchClient.ECProducerSendEvents(context.Background(), largeEventSet)
		if err != nil {
			t.Fatalf("Failed to send large event set: %v", err)
		}
		if len(sentIDs) != 1 {
			t.Fatalf("Expected 1 sent ID for large event set, got %d", len(sentIDs))
		}

		// Receive all events.
		for i := 0; i < nEvents; i++ {
			select {
			case event := <-ec.Events:
				if event.ID != ID {
					t.Fatalf("Expected event for ID, got %s", event.ID)
				}
				expectedType := db_api.BatchEventType(i % 3)
				if event.Type != expectedType {
					t.Fatalf("Expected event type %d, got %d", expectedType, event.Type)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("Timeout waiting for event %d/%d", i+1, nEvents)
			}
		}
		ec.CloseFn() // Close immediately after use
	})

	t.Run("Status exchange operations", func(t *testing.T) {
		t.Parallel()
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		origStatus, updStatus := []byte("orig status"), []byte("updated status")

		// Set status.
		ID := uuid.New().String()
		err := exchClient.StatusSet(context.Background(), ID, 1000, origStatus)
		if err != nil {
			t.Fatalf("Failed to set status: %v", err)
		}

		// Get status.
		stData, err := exchClient.StatusGet(context.Background(), ID)
		if err != nil {
			t.Fatalf("Failed to get status: %v", err)
		}
		if !bytes.Equal(stData, origStatus) {
			t.Fatalf("Invalid status data:\ngot: %s\nwant:%s", stData, origStatus)
		}

		// Update status.
		err = exchClient.StatusSet(context.Background(), ID, 1000, updStatus)
		if err != nil {
			t.Fatalf("Failed to set status: %v", err)
		}

		// Get status.
		stData, err = exchClient.StatusGet(context.Background(), ID)
		if err != nil {
			t.Fatalf("Failed to get status: %v", err)
		}
		if !bytes.Equal(stData, updStatus) {
			t.Fatalf("Invalid status data:\ngot: %s\nwant:%s", stData, updStatus)
		}

		// Delete status.
		nDel, err := exchClient.StatusDelete(context.Background(), ID)
		if err != nil {
			t.Fatalf("Failed to delete status: %v", err)
		}
		if nDel != 1 {
			t.Fatalf("Invalid number of deleted items: %d != 1", nDel)
		}
		stData, err = exchClient.StatusGet(context.Background(), ID)
		if err != nil {
			t.Fatalf("Failed to get status: %v", err)
		}
		if len(stData) != 0 {
			t.Fatalf("Status data should be empty but got: %s", stData)
		}
	})

	t.Run("Queue exchange operations", func(t *testing.T) {
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		itemData := []byte("additional data")
		nHead, nTail := 30, 30
		itemsHead, itemsTail := make([]*db_api.BatchJobPriority, 0, nHead), make([]*db_api.BatchJobPriority, 0, nTail)

		// Enqueue.
		for i := 0; i < nTail; i++ {
			itemTail := &db_api.BatchJobPriority{
				ID:   uuid.New().String(),
				SLO:  time.Now().Add(time.Hour),
				TTL:  1000,
				Data: itemData,
			}
			err := exchClient.PQEnqueue(context.Background(), itemTail)
			if err != nil {
				t.Fatalf("Failed to enqueue: %v", err)
			}
			itemsTail = append(itemsTail, itemTail)
		}
		for i := 0; i < nHead; i++ {
			itemHead := &db_api.BatchJobPriority{
				ID:   uuid.New().String(),
				SLO:  time.Now().Add(time.Second),
				TTL:  1000,
				Data: itemData,
			}
			err := exchClient.PQEnqueue(context.Background(), itemHead)
			if err != nil {
				t.Fatalf("Failed to enqueue: %v", err)
			}
			itemsHead = append(itemsHead, itemHead)
		}

		// Dequeue.
		items, err := exchClient.PQDequeue(context.Background(), 6*time.Second, nHead)
		if err != nil {
			t.Fatalf("Failed to dequeue items: %v", err)
		}
		if len(items) != nHead {
			t.Fatalf("Invalid items list length %d", len(items))
		}
		for i, item := range items {
			isSamePrio(t, item, itemsHead[i])
		}

		// Delete.
		for i := 0; i < nTail; i++ {
			nDel, err := exchClient.PQDelete(context.Background(), itemsTail[i])
			if err != nil {
				t.Fatalf("Failed to delete items: %v", err)
			}
			if nDel != 1 {
				t.Fatalf("Invalid delete count %d", nDel)
			}
		}
	})

	t.Run("Queue exchange operations - Negative cases", func(t *testing.T) {
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Enqueue with nil item.
		err := exchClient.PQEnqueue(context.Background(), nil)
		if err == nil {
			t.Fatalf("Expected error when enqueuing nil item")
		}

		// Enqueue with empty ID.
		invalidItem := &db_api.BatchJobPriority{
			ID:  "",
			SLO: time.Now().Add(time.Hour),
			TTL: 1000,
		}
		err = exchClient.PQEnqueue(context.Background(), invalidItem)
		if err == nil {
			t.Fatalf("Expected error when enqueuing item with empty ID")
		}

		// Enqueue with zero SLO.
		invalidItem2 := &db_api.BatchJobPriority{
			ID:  uuid.New().String(),
			SLO: time.Time{},
			TTL: 1000,
		}
		err = exchClient.PQEnqueue(context.Background(), invalidItem2)
		if err == nil {
			t.Fatalf("Expected error when enqueuing item with zero SLO")
		}

		// Delete with nil item.
		nDel, err := exchClient.PQDelete(context.Background(), nil)
		if err == nil {
			t.Fatalf("Expected error when deleting nil item")
		}
		if nDel != 0 {
			t.Fatalf("Expected 0 deleted items for nil item, got %d", nDel)
		}

		// Delete with empty ID.
		invalidDeleteItem := &db_api.BatchJobPriority{
			ID:  "",
			SLO: time.Now().Add(time.Hour),
		}
		nDel, err = exchClient.PQDelete(context.Background(), invalidDeleteItem)
		if err == nil {
			t.Fatalf("Expected error when deleting item with empty ID")
		}
		if nDel != 0 {
			t.Fatalf("Expected 0 deleted items for invalid item, got %d", nDel)
		}

		// Delete non-existent item.
		nonExistentItem := &db_api.BatchJobPriority{
			ID:  uuid.New().String(),
			SLO: time.Now().Add(time.Hour),
		}
		nDel, err = exchClient.PQDelete(context.Background(), nonExistentItem)
		if err != nil {
			t.Fatalf("Delete of non-existent item should not error: %v", err)
		}
		if nDel != 0 {
			t.Fatalf("Expected 0 deleted items for non-existent item, got %d", nDel)
		}

		// Dequeue from empty queue with timeout.
		items, err := exchClient.PQDequeue(context.Background(), 1*time.Second, 10)
		if err != nil {
			t.Fatalf("Dequeue from empty queue should not error: %v", err)
		}
		if len(items) != 0 {
			t.Fatalf("Expected no items from empty queue, got %d", len(items))
		}
	})

	t.Run("Queue exchange operations - Edge cases", func(t *testing.T) {
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Enqueue items with identical SLO values.
		slo := time.Now().Add(time.Hour)
		nIdentical := 5
		for i := 0; i < nIdentical; i++ {
			item := &db_api.BatchJobPriority{
				ID:   uuid.New().String(),
				SLO:  slo,
				TTL:  1000,
				Data: []byte(fmt.Sprintf("data-%d", i)),
			}
			err := exchClient.PQEnqueue(context.Background(), item)
			if err != nil {
				t.Fatalf("Failed to enqueue item with identical SLO: %v", err)
			}
		}

		// Dequeue all items with identical SLO.
		items, err := exchClient.PQDequeue(context.Background(), 1*time.Second, nIdentical)
		if err != nil {
			t.Fatalf("Failed to dequeue items: %v", err)
		}
		if len(items) != nIdentical {
			t.Fatalf("Expected %d items, got %d", nIdentical, len(items))
		}

		// Enqueue items and dequeue with maxItems exceeding queue size.
		nItems := 3
		for i := 0; i < nItems; i++ {
			item := &db_api.BatchJobPriority{
				ID:   uuid.New().String(),
				SLO:  time.Now().Add(time.Hour),
				TTL:  1000,
				Data: []byte(fmt.Sprintf("small-%d", i)),
			}
			err := exchClient.PQEnqueue(context.Background(), item)
			if err != nil {
				t.Fatalf("Failed to enqueue item: %v", err)
			}
		}

		// Dequeue with maxItems larger than queue size.
		items, err = exchClient.PQDequeue(context.Background(), 1*time.Second, 100)
		if err != nil {
			t.Fatalf("Failed to dequeue items: %v", err)
		}
		if len(items) != nItems {
			t.Fatalf("Expected %d items (all available), got %d", nItems, len(items))
		}

		// Test with large data payload.
		largeData := make([]byte, 1024*100) // 100KB
		for i := range largeData {
			largeData[i] = byte(i % 256)
		}
		largeItem := &db_api.BatchJobPriority{
			ID:   uuid.New().String(),
			SLO:  time.Now().Add(time.Hour),
			TTL:  1000,
			Data: largeData,
		}
		err = exchClient.PQEnqueue(context.Background(), largeItem)
		if err != nil {
			t.Fatalf("Failed to enqueue item with large data: %v", err)
		}

		// Dequeue and verify item identity (Data is not stored in queue member).
		items, err = exchClient.PQDequeue(context.Background(), 1*time.Second, 1)
		if err != nil {
			t.Fatalf("Failed to dequeue large item: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("Expected 1 item, got %d", len(items))
		}
		if items[0].ID != largeItem.ID {
			t.Fatalf("ID mismatch after large data enqueue")
		}

		// Test dequeue with maxItems=0 - should error.
		item := &db_api.BatchJobPriority{
			ID:   uuid.New().String(),
			SLO:  time.Now().Add(time.Hour),
			TTL:  1000,
			Data: []byte("test"),
		}
		_ = exchClient.PQEnqueue(context.Background(), item)
		_, err = exchClient.PQDequeue(context.Background(), 1*time.Second, 0)
		if err == nil {
			t.Fatalf("Dequeue with maxItems=0 should error")
		}

		// Cleanup remaining items if any.
		_, _ = exchClient.PQDequeue(context.Background(), 1*time.Second, 100)
	})

	t.Run("Queue exchange operations - Zero timeout uses ZMPop", func(t *testing.T) {
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Enqueue items.
		enqueued := make([]*db_api.BatchJobPriority, 3)
		for i := 0; i < 3; i++ {
			enqueued[i] = &db_api.BatchJobPriority{
				ID:   uuid.New().String(),
				SLO:  time.Now().Add(time.Duration(i+1) * time.Hour),
				TTL:  1000,
				Data: []byte(fmt.Sprintf("zero-timeout-%d", i)),
			}
			if err := exchClient.PQEnqueue(context.Background(), enqueued[i]); err != nil {
				t.Fatalf("Failed to enqueue: %v", err)
			}
		}

		// Dequeue with timeout=0 (non-blocking ZMPop path).
		items, err := exchClient.PQDequeue(context.Background(), 0, 2)
		if err != nil {
			t.Fatalf("PQDequeue with zero timeout should not error: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("Expected 2 items, got %d", len(items))
		}
		// Verify priority ordering (lowest SLO first).
		isSamePrio(t, items[0], enqueued[0])
		isSamePrio(t, items[1], enqueued[1])

		// Dequeue remaining item.
		items, err = exchClient.PQDequeue(context.Background(), 0, 10)
		if err != nil {
			t.Fatalf("PQDequeue with zero timeout should not error: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("Expected 1 item, got %d", len(items))
		}
		isSamePrio(t, items[0], enqueued[2])

		// Dequeue from empty queue with zero timeout — should return immediately with no items.
		items, err = exchClient.PQDequeue(context.Background(), 0, 10)
		if err != nil {
			t.Fatalf("PQDequeue from empty queue with zero timeout should not error: %v", err)
		}
		if len(items) != 0 {
			t.Fatalf("Expected no items from empty queue, got %d", len(items))
		}
	})

	t.Run("Queue exchange operations - Negative timeout uses ZMPop", func(t *testing.T) {
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Enqueue an item.
		item := &db_api.BatchJobPriority{
			ID:   uuid.New().String(),
			SLO:  time.Now().Add(time.Hour),
			TTL:  1000,
			Data: []byte("negative-timeout"),
		}
		if err := exchClient.PQEnqueue(context.Background(), item); err != nil {
			t.Fatalf("Failed to enqueue: %v", err)
		}

		// Dequeue with negative timeout — should use non-blocking ZMPop path.
		items, err := exchClient.PQDequeue(context.Background(), -1*time.Second, 1)
		if err != nil {
			t.Fatalf("PQDequeue with negative timeout should not error: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("Expected 1 item, got %d", len(items))
		}
		isSamePrio(t, items[0], item)

		// Dequeue from empty queue with negative timeout — should return immediately.
		items, err = exchClient.PQDequeue(context.Background(), -5*time.Second, 10)
		if err != nil {
			t.Fatalf("PQDequeue from empty queue with negative timeout should not error: %v", err)
		}
		if len(items) != 0 {
			t.Fatalf("Expected no items from empty queue, got %d", len(items))
		}
	})

	t.Run("PQGetIDs", func(t *testing.T) {
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		tests := []struct {
			name     string
			enqueue  int
			dequeue  int
			wantSize int
		}{
			{
				name:     "empty queue returns empty map",
				enqueue:  0,
				wantSize: 0,
			},
			{
				name:     "returns all enqueued IDs",
				enqueue:  5,
				wantSize: 5,
			},
			{
				name:     "excludes dequeued IDs",
				enqueue:  5,
				dequeue:  2,
				wantSize: 3,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				var enqueuedIDs []string
				for range tt.enqueue {
					item := &db_api.BatchJobPriority{
						ID:   uuid.New().String(),
						SLO:  time.Now().Add(time.Hour),
						TTL:  1000,
						Data: []byte("test"),
					}
					if err := exchClient.PQEnqueue(context.Background(), item); err != nil {
						t.Fatalf("Failed to enqueue: %v", err)
					}
					enqueuedIDs = append(enqueuedIDs, item.ID)
				}

				if tt.dequeue > 0 {
					items, err := exchClient.PQDequeue(context.Background(), 1*time.Second, tt.dequeue)
					if err != nil {
						t.Fatalf("Failed to dequeue: %v", err)
					}
					if len(items) != tt.dequeue {
						t.Fatalf("expected %d dequeued, got %d", tt.dequeue, len(items))
					}
				}

				ids, err := exchClient.PQGetIDs(context.Background())
				if err != nil {
					t.Fatalf("PQGetIDs failed: %v", err)
				}
				if len(ids) != tt.wantSize {
					t.Fatalf("expected %d IDs, got %d", tt.wantSize, len(ids))
				}

				// Verify returned IDs are from the enqueued set.
				for id := range ids {
					found := false
					for _, eid := range enqueuedIDs {
						if id == eid {
							found = true
							break
						}
					}
					if !found {
						t.Fatalf("unexpected ID %s in PQGetIDs result", id)
					}
				}

				// Cleanup: drain remaining items.
				if tt.wantSize > 0 {
					_, _ = exchClient.PQDequeue(context.Background(), 1*time.Second, tt.wantSize)
				}
			})
		}
	})

	t.Run("Queue - Duplicate enqueue with different Data is deduplicated", func(t *testing.T) {
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		slo := time.Now().Add(time.Hour).UTC()
		jobID := uuid.New().String()

		itemWithData := &db_api.BatchJobPriority{
			ID:   jobID,
			SLO:  slo,
			Data: []byte(`{"created_at":1719750896}`),
			TTL:  1000,
		}
		if err := exchClient.PQEnqueue(context.Background(), itemWithData); err != nil {
			t.Fatalf("Failed to enqueue with Data: %v", err)
		}

		itemWithoutData := &db_api.BatchJobPriority{
			ID:  jobID,
			SLO: slo,
			TTL: 1000,
		}
		if err := exchClient.PQEnqueue(context.Background(), itemWithoutData); err != nil {
			t.Fatalf("Failed to enqueue without Data: %v", err)
		}

		items, err := exchClient.PQDequeue(context.Background(), 1*time.Second, 10)
		if err != nil {
			t.Fatalf("Failed to dequeue: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("Expected 1 item (dedup), got %d", len(items))
		}
		if items[0].ID != jobID {
			t.Fatalf("Expected ID %s, got %s", jobID, items[0].ID)
		}
	})

	t.Run("Queue - Cancel-flow precision alignment", func(t *testing.T) {
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		slo := time.Now().Add(time.Hour)
		jobID := uuid.New().String()

		item := &db_api.BatchJobPriority{
			ID:   jobID,
			SLO:  slo,
			Data: []byte(`{"created_at":1719750896}`),
			TTL:  1000,
		}
		if err := exchClient.PQEnqueue(context.Background(), item); err != nil {
			t.Fatalf("Failed to enqueue: %v", err)
		}

		deleteItem := &db_api.BatchJobPriority{
			ID:  jobID,
			SLO: time.UnixMicro(slo.UnixMicro()).UTC(),
		}
		nDel, err := exchClient.PQDelete(context.Background(), deleteItem)
		if err != nil {
			t.Fatalf("PQDelete failed: %v", err)
		}
		if nDel != 1 {
			t.Fatalf("Expected 1 deleted, got %d", nDel)
		}
	})

	t.Run("Queue - Same-SLO delete targets only matching job", func(t *testing.T) {
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		slo := time.Now().Add(time.Hour).UTC()
		ids := make([]string, 3)
		for i := range ids {
			ids[i] = uuid.New().String()
			item := &db_api.BatchJobPriority{
				ID:  ids[i],
				SLO: slo,
				TTL: 1000,
			}
			if err := exchClient.PQEnqueue(context.Background(), item); err != nil {
				t.Fatalf("Failed to enqueue item %d: %v", i, err)
			}
		}

		deleteItem := &db_api.BatchJobPriority{
			ID:  ids[1],
			SLO: slo,
		}
		nDel, err := exchClient.PQDelete(context.Background(), deleteItem)
		if err != nil {
			t.Fatalf("PQDelete failed: %v", err)
		}
		if nDel != 1 {
			t.Fatalf("Expected 1 deleted, got %d", nDel)
		}

		remaining, err := exchClient.PQGetIDs(context.Background())
		if err != nil {
			t.Fatalf("PQGetIDs failed: %v", err)
		}
		if len(remaining) != 2 {
			t.Fatalf("Expected 2 remaining, got %d", len(remaining))
		}
		if remaining[ids[1]] {
			t.Fatalf("Deleted job %s should not be in queue", ids[1])
		}
		if !remaining[ids[0]] || !remaining[ids[2]] {
			t.Fatalf("Surviving jobs missing from queue")
		}

		_, _ = exchClient.PQDequeue(context.Background(), 1*time.Second, 10)
	})

	t.Run("Queue - Backward compat with old-format member", func(t *testing.T) {
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		jobID := uuid.New().String()
		slo := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		oldMember := fmt.Sprintf(`{"id":"%s","slo":"%s","data":"eyJjcmVhdGVkX2F0IjoxNzE5NzUwODk2fQ=="}`,
			jobID, slo.Format(time.RFC3339Nano))
		score := float64(slo.UnixMicro())

		rawOpts, parseErr := goredis.ParseURL(redisUrl)
		if parseErr != nil {
			t.Fatalf("Failed to parse Redis URL: %v", parseErr)
		}
		rawClient := goredis.NewClient(rawOpts)
		t.Cleanup(func() { _ = rawClient.Close() })
		if err := rawClient.ZAdd(context.Background(), "llmd_batch:queue:priority", goredis.Z{
			Score:  score,
			Member: oldMember,
		}).Err(); err != nil {
			t.Fatalf("Failed to ZADD old-format member: %v", err)
		}

		items, err := exchClient.PQDequeue(context.Background(), 1*time.Second, 1)
		if err != nil {
			t.Fatalf("PQDequeue failed: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("Expected 1 item, got %d", len(items))
		}
		if items[0].ID != jobID {
			t.Fatalf("Expected ID %s, got %s", jobID, items[0].ID)
		}
		if items[0].Data == nil {
			t.Fatalf("Expected Data to be populated from old-format member")
		}
	})
}

func isSamePrio(t *testing.T, a, b *db_api.BatchJobPriority) bool {
	t.Helper()
	if a.ID != b.ID {
		t.Fatalf("ID mismatch %s != %s", a.ID, b.ID)
	}
	if !a.SLO.Equal(b.SLO) {
		t.Fatalf("SLO mismatch %v != %v", a.SLO, b.SLO)
	}
	return true
}

func isSameEvent(t *testing.T, a, b *db_api.BatchEvent) bool {
	t.Helper()
	if a.ID != b.ID {
		t.Fatalf("ID mismatch %s != %s", a.ID, b.ID)
	}
	if a.Type != b.Type {
		t.Fatalf("Type mismatch %v != %v", a.Type, b.Type)
	}
	return true
}
