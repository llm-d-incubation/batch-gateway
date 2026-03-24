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
	"maps"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/alicebob/miniredis/v2"
	db_api "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	dbredis "github.com/llm-d-incubation/batch-gateway/internal/database/redis"
	ucom "github.com/llm-d-incubation/batch-gateway/internal/util/com"
	uredis "github.com/llm-d-incubation/batch-gateway/internal/util/redis"
	utls "github.com/llm-d-incubation/batch-gateway/internal/util/tls"
)

func setupRedisDSClients(t *testing.T, redisUrl, redisCaCert string) (
	*dbredis.DSClientRedis, *dbredis.BatchDBClientRedis, *dbredis.FileDBClientRedis, *dbredis.ExchangeDBClientRedis) {
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
	batchClient, err := dbredis.NewBatchDBClientRedis(ctx, baseClient, nil, 0)
	if err != nil {
		t.Fatalf("Failed to create batch redis client: %v", err)
	}
	fileClient, err := dbredis.NewFileDBClientRedis(ctx, baseClient, nil, 0)
	if err != nil {
		t.Fatalf("Failed to create file redis client: %v", err)
	}
	exchClient, err := dbredis.NewExchangeDBClientRedis(ctx, baseClient, nil, 0)
	if err != nil {
		t.Fatalf("Failed to create exchange redis client: %v", err)
	}
	return baseClient, batchClient, fileClient, exchClient
}

func TestRedisDSClient(t *testing.T) {

	redisUrl := os.Getenv("TEST_REDIS_URL")
	redisCaCert := os.Getenv("TEST_REDIS_CACERT_PATH")
	var (
		minirds *miniredis.Miniredis
		tagKey1 = "key-tag-1"
		tagKey2 = "key-tag-2"
		tagKey3 = "key-tag-3"
		tagVal1 = "val-tag-1"
		tagVal2 = "val-tag-2"
		tagVal3 = "val-tag-3"
	)

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
		baseClient, batchClient, fileClient, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})
		t.Logf("Memory address of the clients: base=%p batch=%p file=%p exchange=%p",
			baseClient, batchClient, fileClient, exchClient)
		if baseClient == nil || batchClient == nil || fileClient == nil || exchClient == nil {
			t.Fatalf("Expected redis clients to be non-nil")
		}
	})

	t.Run("Batch db operations", func(t *testing.T) {
		t.Parallel()
		baseClient, batchClient, _, _ := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Store.
		nBatches := 20
		nBatchesRmv := 10
		var wg sync.WaitGroup
		batches, batchesRmv := make(map[string]*db_api.BatchItem), make(map[string]*db_api.BatchItem)
		var batchesIDs, batchesAllIDs []string
		for i := 0; i < nBatchesRmv; i++ {
			batchID := uuid.New().String()
			batch := &db_api.BatchItem{
				BaseIndexes: db_api.BaseIndexes{
					ID:       batchID,
					TenantID: "Tnt2",
					Expiry:   time.Now().Add(time.Second).Unix(),
					Tags:     map[string]string{tagKey1: tagVal1, tagKey2: tagVal2},
				},
				BaseContents: db_api.BaseContents{
					Spec:   []byte("spec"),
					Status: []byte("status"),
				},
			}
			batchesRmv[batchID] = batch
			batchesAllIDs = append(batchesAllIDs, batchID)
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := batchClient.DBStore(context.Background(), batch)
				if err != nil {
					t.Errorf("Failed to store item: %v", err)
				}
			}()
		}
		wg.Wait()
		for i := 0; i < nBatches; i++ {
			batchID := uuid.New().String()
			batch := &db_api.BatchItem{
				BaseIndexes: db_api.BaseIndexes{
					ID:       batchID,
					TenantID: "Tnt1",
					Expiry:   time.Now().Add(time.Hour).Unix(),
					Tags:     map[string]string{tagKey1: tagVal1, tagKey3: tagVal3},
				},
				BaseContents: db_api.BaseContents{
					Spec:   []byte("spec"),
					Status: []byte("status"),
				},
			}
			batches[batchID] = batch
			batchesIDs = append(batchesIDs, batchID)
			batchesAllIDs = append(batchesAllIDs, batchID)
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := batchClient.DBStore(context.Background(), batch)
				if err != nil {
					t.Errorf("Failed to store item: %v", err)
				}
			}()
		}
		wg.Wait()
		time.Sleep(3 * time.Second) // To pass the expiry time of the short expiry items.

		// Get expired.
		expectMore := true
		nRet, cursor := 0, 0
		for expectMore {
			resItems, cur, expectM, err := batchClient.DBGet(context.Background(),
				&db_api.BatchQuery{
					BaseQuery: db_api.BaseQuery{
						Expired: true,
					},
				}, true, cursor, nBatchesRmv*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			sameMembersBatch(t, resItems, batchesRmv)
			nRet += len(resItems)
			expectMore = expectM
			cursor = cur
		}
		if nRet != nBatchesRmv {
			t.Fatalf("Invalid number of items %d != %d", nRet, nBatchesRmv)
		}

		// Get by IDs.
		expectMore = true
		nRet, cursor = 0, 0
		for expectMore {
			resItems, cur, expectM, err := batchClient.DBGet(context.Background(),
				&db_api.BatchQuery{
					BaseQuery: db_api.BaseQuery{
						IDs: batchesIDs,
					},
				}, true, cursor, nBatches*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			sameMembersBatch(t, resItems, batches)
			nRet += len(resItems)
			expectMore = expectM
			cursor = cur
		}
		if nRet != nBatches {
			t.Fatalf("Invalid number of items %d != %d", nRet, nBatches)
		}

		// Get by tenant.
		expectMore = true
		nRet, cursor = 0, 0
		for expectMore {
			resItems, cur, expectM, err := batchClient.DBGet(context.Background(),
				&db_api.BatchQuery{
					BaseQuery: db_api.BaseQuery{
						TenantID: "Tnt2",
					},
				}, true, cursor, nBatchesRmv*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			sameMembersBatch(t, resItems, batchesRmv)
			nRet += len(resItems)
			expectMore = expectM
			cursor = cur
		}
		if nRet != nBatchesRmv {
			t.Fatalf("Invalid number of items %d != %d", nRet, nBatchesRmv)
		}

		// Get by tags.
		expectMore = true
		nRet, cursor = 0, 0
		for expectMore {
			resItems, cur, expectM, err := batchClient.DBGet(context.Background(),
				&db_api.BatchQuery{
					BaseQuery: db_api.BaseQuery{
						TagSelectors:    db_api.Tags{tagKey1: tagVal1, tagKey3: tagVal3},
						TagsLogicalCond: db_api.LogicalCondAnd,
					},
				}, true, cursor, nBatches*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			sameMembersBatch(t, resItems, batches)
			nRet += len(resItems)
			expectMore = expectM
			cursor = cur
		}
		if nRet != nBatches {
			t.Fatalf("Invalid number of items %d != %d", nRet, nBatches)
		}
		expectMore = true
		nRet, cursor = 0, 0
		for expectMore {
			resItems, cur, expectM, err := batchClient.DBGet(context.Background(),
				&db_api.BatchQuery{
					BaseQuery: db_api.BaseQuery{
						TagSelectors:    db_api.Tags{tagKey1: tagVal1, tagKey3: tagVal3},
						TagsLogicalCond: db_api.LogicalCondOr,
					},
				}, true, cursor, nBatches*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			nRet += len(resItems)
			expectMore = expectM
			cursor = cur
		}
		if nRet != nBatches+nBatchesRmv {
			t.Fatalf("Invalid number of items %d != %d", nRet, nBatches+nBatchesRmv)
		}

		// Get by tags and tenant.
		expectMore = true
		nRet, cursor = 0, 0
		for expectMore {
			resItems, cur, expectM, err := batchClient.DBGet(context.Background(),
				&db_api.BatchQuery{
					BaseQuery: db_api.BaseQuery{
						TenantID:        "Tnt1",
						TagSelectors:    db_api.Tags{tagKey1: tagVal1, tagKey3: tagVal3},
						TagsLogicalCond: db_api.LogicalCondOr,
					},
				}, true, cursor, nBatches*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			sameMembersBatch(t, resItems, batches)
			nRet += len(resItems)
			expectMore = expectM
			cursor = cur
		}
		if nRet != nBatches {
			t.Fatalf("Invalid number of items %d != %d", nRet, nBatches)
		}

		// Update.
		updId := batchesIDs[0]
		updBatch := batches[updId]
		updBatch.Status = []byte("statusUpdated")
		err := batchClient.DBUpdate(context.Background(), updBatch)
		if err != nil {
			t.Fatalf("Failed to update item: %v", err)
		}
		resItems, _, expectM, err := batchClient.DBGet(context.Background(),
			&db_api.BatchQuery{
				BaseQuery: db_api.BaseQuery{
					IDs: []string{updId},
				},
			}, true, 0, 1)
		if err != nil {
			t.Fatalf("Failed to get item: %v", err)
		}
		if expectM {
			t.Fatalf("Invalid expect more")
		}
		if len(resItems) != 1 {
			t.Fatalf("Invalid number of returned items")
		}
		isEqualBatchItem(t, updBatch, resItems[0])

		// Delete.
		deletedIDs, err := batchClient.DBDelete(context.Background(), batchesAllIDs)
		if err != nil {
			t.Fatalf("Failed to delete items: %v", err)
		}
		if deletedIDs == nil || len(deletedIDs) != len(batchesAllIDs) {
			t.Fatalf("Failed to delete items: %d", len(deletedIDs))
		}
		if !ucom.SameMembersInStrSlice(deletedIDs, batchesAllIDs) {
			t.Fatalf("Deletion IDs mismatch: %v != %v", deletedIDs, batchesAllIDs)
		}

	})

	t.Run("File db operations", func(t *testing.T) {
		t.Parallel()
		baseClient, _, fileClient, _ := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Store.
		nFiles := 20
		nFilesRmv := 10
		var wg sync.WaitGroup
		files, filesRmv := make(map[string]*db_api.FileItem), make(map[string]*db_api.FileItem)
		var filesIDs, filesAllIDs []string
		for i := 0; i < nFilesRmv; i++ {
			fileID := uuid.New().String()
			file := &db_api.FileItem{
				BaseIndexes: db_api.BaseIndexes{
					ID:       fileID,
					TenantID: "Tnt2",
					Expiry:   time.Now().Add(time.Second).Unix(),
					Tags:     map[string]string{tagKey1: tagVal1, tagKey2: tagVal2},
				},
				Purpose: "file",
				BaseContents: db_api.BaseContents{
					Spec:   []byte("spec"),
					Status: []byte("status"),
				},
			}
			filesRmv[fileID] = file
			filesAllIDs = append(filesAllIDs, fileID)
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := fileClient.DBStore(context.Background(), file)
				if err != nil {
					t.Errorf("Failed to store item: %v", err)
				}
			}()
		}
		wg.Wait()
		for i := 0; i < nFiles; i++ {
			fileID := uuid.New().String()
			file := &db_api.FileItem{
				BaseIndexes: db_api.BaseIndexes{
					ID:       fileID,
					TenantID: "Tnt1",
					Expiry:   time.Now().Add(time.Hour).Unix(),
					Tags:     map[string]string{tagKey1: tagVal1, tagKey3: tagVal3},
				},
				BaseContents: db_api.BaseContents{
					Spec:   []byte("spec"),
					Status: []byte("status"),
				},
			}
			files[fileID] = file
			filesIDs = append(filesIDs, fileID)
			filesAllIDs = append(filesAllIDs, fileID)
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := fileClient.DBStore(context.Background(), file)
				if err != nil {
					t.Errorf("Failed to store item: %v", err)
				}
			}()
		}
		wg.Wait()
		time.Sleep(3 * time.Second) // To pass the expiry time of the short expiry items.

		// Get expired.
		expectMore := true
		nRet, cursor := 0, 0
		for expectMore {
			resItems, cur, expectM, err := fileClient.DBGet(context.Background(),
				&db_api.FileQuery{
					BaseQuery: db_api.BaseQuery{
						Expired: true,
					},
				}, true, cursor, nFilesRmv*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			sameMembersFile(t, resItems, filesRmv)
			nRet += len(resItems)
			expectMore = expectM
			cursor = cur
		}
		if nRet != nFilesRmv {
			t.Fatalf("Invalid number of items %d != %d", nRet, nFilesRmv)
		}

		// Get by IDs.
		expectMore = true
		nRet, cursor = 0, 0
		for expectMore {
			resItems, cur, expectM, err := fileClient.DBGet(context.Background(),
				&db_api.FileQuery{
					BaseQuery: db_api.BaseQuery{
						IDs: filesIDs,
					},
				}, true, cursor, nFiles*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			sameMembersFile(t, resItems, files)
			nRet += len(resItems)
			expectMore = expectM
			cursor = cur
		}
		if nRet != nFiles {
			t.Fatalf("Invalid number of items %d != %d", nRet, nFiles)
		}

		// Get by tenant.
		expectMore = true
		nRet, cursor = 0, 0
		for expectMore {
			resItems, cur, expectM, err := fileClient.DBGet(context.Background(),
				&db_api.FileQuery{
					BaseQuery: db_api.BaseQuery{
						TenantID: "Tnt2",
					},
				}, true, cursor, nFilesRmv*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			sameMembersFile(t, resItems, filesRmv)
			nRet += len(resItems)
			expectMore = expectM
			cursor = cur
		}
		if nRet != nFilesRmv {
			t.Fatalf("Invalid number of items %d != %d", nRet, nFilesRmv)
		}

		// Get by tags.
		expectMore = true
		nRet, cursor = 0, 0
		for expectMore {
			resItems, cur, expectM, err := fileClient.DBGet(context.Background(),
				&db_api.FileQuery{
					BaseQuery: db_api.BaseQuery{
						TagSelectors:    db_api.Tags{tagKey1: tagVal1, tagKey3: tagVal3},
						TagsLogicalCond: db_api.LogicalCondAnd,
					},
				}, true, cursor, nFiles*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			sameMembersFile(t, resItems, files)
			nRet += len(resItems)
			expectMore = expectM
			cursor = cur
		}
		if nRet != nFiles {
			t.Fatalf("Invalid number of items %d != %d", nRet, nFiles)
		}
		expectMore = true
		nRet, cursor = 0, 0
		for expectMore {
			resItems, cur, expectM, err := fileClient.DBGet(context.Background(),
				&db_api.FileQuery{
					BaseQuery: db_api.BaseQuery{
						TagSelectors:    db_api.Tags{tagKey1: tagVal1, tagKey3: tagVal3},
						TagsLogicalCond: db_api.LogicalCondOr,
					},
				}, true, cursor, nFiles*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			nRet += len(resItems)
			expectMore = expectM
			cursor = cur
		}
		if nRet != nFiles+nFilesRmv {
			t.Fatalf("Invalid number of items %d != %d", nRet, nFiles+nFilesRmv)
		}

		// Get by tags and tenant.
		expectMore = true
		nRet, cursor = 0, 0
		for expectMore {
			resItems, cur, expectM, err := fileClient.DBGet(context.Background(),
				&db_api.FileQuery{
					BaseQuery: db_api.BaseQuery{
						TenantID:        "Tnt1",
						TagSelectors:    db_api.Tags{tagKey1: tagVal1, tagKey3: tagVal3},
						TagsLogicalCond: db_api.LogicalCondOr,
					},
				}, true, cursor, nFiles*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			sameMembersFile(t, resItems, files)
			nRet += len(resItems)
			expectMore = expectM
			cursor = cur
		}
		if nRet != nFiles {
			t.Fatalf("Invalid number of items %d != %d", nRet, nFiles)
		}

		// Update.
		updId := filesIDs[0]
		updFile := files[updId]
		updFile.Status = []byte("statusUpdated")
		err := fileClient.DBUpdate(context.Background(), updFile)
		if err != nil {
			t.Fatalf("Failed to update item: %v", err)
		}
		resItems, _, expectM, err := fileClient.DBGet(context.Background(),
			&db_api.FileQuery{
				BaseQuery: db_api.BaseQuery{
					IDs: []string{updId},
				},
			}, true, 0, 1)
		if err != nil {
			t.Fatalf("Failed to get item: %v", err)
		}
		if expectM {
			t.Fatalf("Invalid expect more")
		}
		if len(resItems) != 1 {
			t.Fatalf("Invalid number of returned items")
		}
		isEqualFileItem(t, updFile, resItems[0])

		// Delete.
		deletedIDs, err := fileClient.DBDelete(context.Background(), filesAllIDs)
		if err != nil {
			t.Fatalf("Failed to delete items: %v", err)
		}
		if deletedIDs == nil || len(deletedIDs) != len(filesAllIDs) {
			t.Fatalf("Failed to delete items: %d", len(deletedIDs))
		}
		if !ucom.SameMembersInStrSlice(deletedIDs, filesAllIDs) {
			t.Fatalf("Deletion IDs mismatch: %v != %v", deletedIDs, filesAllIDs)
		}

	})

	t.Run("Event exchange operations", func(t *testing.T) {
		t.Parallel()
		if minirds != nil {
			t.Skip("Miniredis model")
		}
		baseClient, _, _, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
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
		baseClient, _, _, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
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
		baseClient, _, _, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
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
		baseClient, _, _, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
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
		baseClient, _, _, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
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
		baseClient, _, _, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
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
		baseClient, _, _, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
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
		baseClient, _, _, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
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
		baseClient, _, _, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
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
		baseClient, _, _, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
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
		baseClient, _, _, exchClient := setupRedisDSClients(t, redisUrl, redisCaCert)
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

		// Dequeue and verify large data.
		items, err = exchClient.PQDequeue(context.Background(), 1*time.Second, 1)
		if err != nil {
			t.Fatalf("Failed to dequeue large item: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("Expected 1 item, got %d", len(items))
		}
		if !bytes.Equal(items[0].Data, largeData) {
			t.Fatalf("Large data mismatch")
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

	t.Run("includeStatic parameter - Batch", func(t *testing.T) {
		t.Parallel()
		baseClient, batchClient, _, _ := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Store batch with spec.
		batchID := uuid.New().String()
		spec := []byte("important spec data")
		batch := &db_api.BatchItem{
			BaseIndexes: db_api.BaseIndexes{
				ID:       batchID,
				TenantID: "Tnt1",
				Expiry:   time.Now().Add(time.Hour).Unix(),
				Tags:     map[string]string{tagKey1: tagVal1},
			},
			BaseContents: db_api.BaseContents{
				Spec:   spec,
				Status: []byte("status"),
			},
		}
		err := batchClient.DBStore(context.Background(), batch)
		if err != nil {
			t.Fatalf("Failed to store batch: %v", err)
		}

		// Get with includeStatic=true.
		resItems, _, _, err := batchClient.DBGet(context.Background(),
			&db_api.BatchQuery{
				BaseQuery: db_api.BaseQuery{
					IDs: []string{batchID},
				},
			}, true, 0, 10)
		if err != nil {
			t.Fatalf("Failed to get batch: %v", err)
		}
		if len(resItems) != 1 {
			t.Fatalf("Expected 1 item, got %d", len(resItems))
		}
		if !bytes.Equal(resItems[0].Spec, spec) {
			t.Fatalf("Spec should be included when includeStatic=true")
		}

		// Get with includeStatic=false.
		resItems, _, _, err = batchClient.DBGet(context.Background(),
			&db_api.BatchQuery{
				BaseQuery: db_api.BaseQuery{
					IDs: []string{batchID},
				},
			}, false, 0, 10)
		if err != nil {
			t.Fatalf("Failed to get batch: %v", err)
		}
		if len(resItems) != 1 {
			t.Fatalf("Expected 1 item, got %d", len(resItems))
		}
		if len(resItems[0].Spec) != 0 {
			t.Fatalf("Spec should be excluded when includeStatic=false, got: %v", resItems[0].Spec)
		}
		// Status should still be present.
		if len(resItems[0].Status) == 0 {
			t.Fatalf("Status should still be present")
		}

		// Cleanup.
		_, _ = batchClient.DBDelete(context.Background(), []string{batchID})
	})

	t.Run("includeStatic parameter - File", func(t *testing.T) {
		t.Parallel()
		baseClient, _, fileClient, _ := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Store file with spec.
		fileID := uuid.New().String()
		spec := []byte("important spec data")
		file := &db_api.FileItem{
			BaseIndexes: db_api.BaseIndexes{
				ID:       fileID,
				TenantID: "Tnt1",
				Expiry:   time.Now().Add(time.Hour).Unix(),
				Tags:     map[string]string{tagKey1: tagVal1},
			},
			Purpose: "test",
			BaseContents: db_api.BaseContents{
				Spec:   spec,
				Status: []byte("status"),
			},
		}
		err := fileClient.DBStore(context.Background(), file)
		if err != nil {
			t.Fatalf("Failed to store file: %v", err)
		}

		// Get with includeStatic=true.
		resItems, _, _, err := fileClient.DBGet(context.Background(),
			&db_api.FileQuery{
				BaseQuery: db_api.BaseQuery{
					IDs: []string{fileID},
				},
			}, true, 0, 10)
		if err != nil {
			t.Fatalf("Failed to get file: %v", err)
		}
		if len(resItems) != 1 {
			t.Fatalf("Expected 1 item, got %d", len(resItems))
		}
		if !bytes.Equal(resItems[0].Spec, spec) {
			t.Fatalf("Spec should be included when includeStatic=true")
		}

		// Get with includeStatic=false.
		resItems, _, _, err = fileClient.DBGet(context.Background(),
			&db_api.FileQuery{
				BaseQuery: db_api.BaseQuery{
					IDs: []string{fileID},
				},
			}, false, 0, 10)
		if err != nil {
			t.Fatalf("Failed to get file: %v", err)
		}
		if len(resItems) != 1 {
			t.Fatalf("Expected 1 item, got %d", len(resItems))
		}
		if len(resItems[0].Spec) != 0 {
			t.Fatalf("Spec should be excluded when includeStatic=false, got: %v", resItems[0].Spec)
		}

		// Cleanup.
		_, _ = fileClient.DBDelete(context.Background(), []string{fileID})
	})

	t.Run("Negative cases - Batch", func(t *testing.T) {
		t.Parallel()
		baseClient, batchClient, _, _ := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Store with empty ID should fail validation.
		invalidBatch := &db_api.BatchItem{
			BaseIndexes: db_api.BaseIndexes{
				ID: "",
			},
			BaseContents: db_api.BaseContents{
				Spec:   []byte("spec"),
				Status: []byte("status"),
			},
		}
		err := batchClient.DBStore(context.Background(), invalidBatch)
		if err == nil {
			t.Fatalf("Expected error when storing batch with empty ID")
		}

		// Get with non-existent IDs.
		resItems, _, _, err := batchClient.DBGet(context.Background(),
			&db_api.BatchQuery{
				BaseQuery: db_api.BaseQuery{
					IDs: []string{"non-existent-id-1", "non-existent-id-2"},
				},
			}, true, 0, 10)
		if err != nil {
			t.Fatalf("Get should not error for non-existent IDs: %v", err)
		}
		if len(resItems) != 0 {
			t.Fatalf("Expected 0 items for non-existent IDs, got %d", len(resItems))
		}

		// Get with empty query.
		resItems, _, _, err = batchClient.DBGet(context.Background(), nil, true, 0, 10)
		if err != nil {
			t.Fatalf("Get should handle nil query gracefully: %v", err)
		}
		if len(resItems) != 0 {
			t.Fatalf("Expected 0 items for nil query, got %d", len(resItems))
		}

		// Get with empty IDs list.
		resItems, _, expectMore, err := batchClient.DBGet(context.Background(),
			&db_api.BatchQuery{
				BaseQuery: db_api.BaseQuery{
					IDs: []string{},
				},
			}, true, 0, 10)
		if err != nil {
			t.Fatalf("Get should handle empty IDs list: %v", err)
		}
		if len(resItems) != 0 || expectMore {
			t.Fatalf("Expected 0 items and no more for empty IDs")
		}

		// Update non-existent item.
		nonExistentBatch := &db_api.BatchItem{
			BaseIndexes: db_api.BaseIndexes{
				ID:       "non-existent-update-id",
				TenantID: "Tnt1",
			},
			BaseContents: db_api.BaseContents{
				Status: []byte("updated"),
			},
		}
		err = batchClient.DBUpdate(context.Background(), nonExistentBatch)
		if err != nil {
			t.Fatalf("Update of non-existent item should not error: %v", err)
		}
		// Cleanup: delete the key created by the update.
		_, _ = batchClient.DBDelete(context.Background(), []string{"non-existent-update-id"})

		// Update with empty ID should fail validation.
		invalidUpdate := &db_api.BatchItem{
			BaseIndexes: db_api.BaseIndexes{
				ID: "",
			},
		}
		err = batchClient.DBUpdate(context.Background(), invalidUpdate)
		if err == nil {
			t.Fatalf("Expected error when updating batch with empty ID")
		}

		// Delete non-existent items.
		deletedIDs, err := batchClient.DBDelete(context.Background(), []string{"non-existent-1", "non-existent-2"})
		if err != nil {
			t.Fatalf("Delete should not error for non-existent IDs: %v", err)
		}
		if len(deletedIDs) != 0 {
			t.Fatalf("Expected 0 deleted IDs, got %d", len(deletedIDs))
		}

		// Delete with empty IDs list.
		deletedIDs, err = batchClient.DBDelete(context.Background(), []string{})
		if err != nil {
			t.Fatalf("Delete should handle empty IDs list: %v", err)
		}
		if len(deletedIDs) != 0 {
			t.Fatalf("Expected 0 deleted IDs for empty list, got %d", len(deletedIDs))
		}
	})

	t.Run("Negative cases - File", func(t *testing.T) {
		t.Parallel()
		baseClient, _, fileClient, _ := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Store with empty ID should fail validation.
		invalidFile := &db_api.FileItem{
			BaseIndexes: db_api.BaseIndexes{
				ID: "",
			},
			Purpose: "test",
		}
		err := fileClient.DBStore(context.Background(), invalidFile)
		if err == nil {
			t.Fatalf("Expected error when storing file with empty ID")
		}

		// Get by purpose with empty string.
		resItems, _, _, err := fileClient.DBGet(context.Background(),
			&db_api.FileQuery{
				Purpose: "",
			}, true, 0, 10)
		if err != nil {
			t.Fatalf("Get should handle empty purpose: %v", err)
		}
		if len(resItems) != 0 {
			t.Fatalf("Expected 0 items for empty purpose, got %d", len(resItems))
		}
	})

	t.Run("Edge cases - Empty fields", func(t *testing.T) {
		t.Parallel()
		baseClient, batchClient, _, _ := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Store batch with empty spec and status.
		batchID := uuid.New().String()
		batch := &db_api.BatchItem{
			BaseIndexes: db_api.BaseIndexes{
				ID:       batchID,
				TenantID: "Tnt1",
				Expiry:   0,
				Tags:     map[string]string{},
			},
			BaseContents: db_api.BaseContents{
				Spec:   []byte{},
				Status: []byte{},
			},
		}
		err := batchClient.DBStore(context.Background(), batch)
		if err != nil {
			t.Fatalf("Failed to store batch with empty fields: %v", err)
		}

		// Retrieve and verify.
		resItems, _, _, err := batchClient.DBGet(context.Background(),
			&db_api.BatchQuery{
				BaseQuery: db_api.BaseQuery{
					IDs: []string{batchID},
				},
			}, true, 0, 10)
		if err != nil {
			t.Fatalf("Failed to get batch: %v", err)
		}
		if len(resItems) != 1 {
			t.Fatalf("Expected 1 item, got %d", len(resItems))
		}
		if resItems[0].Expiry != 0 {
			t.Fatalf("Expected expiry 0, got %d", resItems[0].Expiry)
		}
		if len(resItems[0].Tags) != 0 {
			t.Fatalf("Expected empty tags, got %v", resItems[0].Tags)
		}

		// Cleanup.
		_, _ = batchClient.DBDelete(context.Background(), []string{batchID})
	})

	t.Run("Edge cases - Update with empty fields", func(t *testing.T) {
		t.Parallel()
		baseClient, batchClient, _, _ := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Store batch.
		batchID := uuid.New().String()
		batch := &db_api.BatchItem{
			BaseIndexes: db_api.BaseIndexes{
				ID:       batchID,
				TenantID: "Tnt1",
				Tags:     map[string]string{tagKey1: tagVal1},
			},
			BaseContents: db_api.BaseContents{
				Spec:   []byte("spec"),
				Status: []byte("status"),
			},
		}
		err := batchClient.DBStore(context.Background(), batch)
		if err != nil {
			t.Fatalf("Failed to store batch: %v", err)
		}

		// Update with empty status and tags - should do nothing.
		updateBatch := &db_api.BatchItem{
			BaseIndexes: db_api.BaseIndexes{
				ID:   batchID,
				Tags: map[string]string{},
			},
			BaseContents: db_api.BaseContents{
				Status: []byte{},
			},
		}
		err = batchClient.DBUpdate(context.Background(), updateBatch)
		if err != nil {
			t.Fatalf("Failed to update batch: %v", err)
		}

		// Verify original values unchanged.
		resItems, _, _, err := batchClient.DBGet(context.Background(),
			&db_api.BatchQuery{
				BaseQuery: db_api.BaseQuery{
					IDs: []string{batchID},
				},
			}, true, 0, 10)
		if err != nil {
			t.Fatalf("Failed to get batch: %v", err)
		}
		if len(resItems) != 1 {
			t.Fatalf("Expected 1 item, got %d", len(resItems))
		}
		if !bytes.Equal(resItems[0].Status, []byte("status")) {
			t.Fatalf("Status should be unchanged")
		}

		// Cleanup.
		_, _ = batchClient.DBDelete(context.Background(), []string{batchID})
	})

	t.Run("Get by IDs with tenant filter", func(t *testing.T) {
		t.Parallel()
		baseClient, batchClient, _, _ := setupRedisDSClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = baseClient.Close()
		})

		// Store batches with different tenants.
		batch1ID := uuid.New().String()
		batch1 := &db_api.BatchItem{
			BaseIndexes: db_api.BaseIndexes{
				ID:       batch1ID,
				TenantID: "TenantA",
			},
			BaseContents: db_api.BaseContents{
				Status: []byte("status1"),
			},
		}
		batch2ID := uuid.New().String()
		batch2 := &db_api.BatchItem{
			BaseIndexes: db_api.BaseIndexes{
				ID:       batch2ID,
				TenantID: "TenantB",
			},
			BaseContents: db_api.BaseContents{
				Status: []byte("status2"),
			},
		}
		_ = batchClient.DBStore(context.Background(), batch1)
		_ = batchClient.DBStore(context.Background(), batch2)

		// Get by IDs with tenant filter.
		resItems, _, _, err := batchClient.DBGet(context.Background(),
			&db_api.BatchQuery{
				BaseQuery: db_api.BaseQuery{
					IDs:      []string{batch1ID, batch2ID},
					TenantID: "TenantA",
				},
			}, true, 0, 10)
		if err != nil {
			t.Fatalf("Failed to get batches: %v", err)
		}
		if len(resItems) != 1 {
			t.Fatalf("Expected 1 item with TenantA, got %d", len(resItems))
		}
		if resItems[0].ID != batch1ID {
			t.Fatalf("Expected batch1, got %s", resItems[0].ID)
		}

		// Cleanup.
		_, _ = batchClient.DBDelete(context.Background(), []string{batch1ID, batch2ID})
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
	if !bytes.EqualFold(a.Data, b.Data) {
		t.Fatalf("Data mismatch %v != %v", a.Data, b.Data)
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

func sameMembersBatch(t *testing.T, sl []*db_api.BatchItem, mp map[string]*db_api.BatchItem) bool {
	t.Helper()
	for _, item := range sl {
		isEqualBatchItem(t, item, mp[item.ID])
	}
	return true
}

func isEqualBatchItem(t *testing.T, a, b *db_api.BatchItem) bool {
	t.Helper()
	if a == nil || b == nil {
		t.Fatalf("Invalid items to compare")
	}
	if a.ID != b.ID {
		t.Fatalf("Mismatch id %s != %s", a.ID, b.ID)
	}
	if a.TenantID != b.TenantID {
		t.Fatalf("Mismatch TenantID %s != %s", a.TenantID, b.TenantID)
	}
	if a.Expiry != b.Expiry {
		t.Fatalf("Mismatch expiry %d != %d", a.Expiry, b.Expiry)
	}
	if !maps.Equal(a.Tags, b.Tags) {
		t.Fatalf("Mismatch tags %v != %v", a.Tags, b.Tags)
	}
	if !bytes.Equal(a.Spec, b.Spec) {
		t.Fatalf("Mismatch spec %s != %s", a.Spec, b.Spec)
	}
	if !bytes.Equal(a.Status, b.Status) {
		t.Fatalf("Mismatch status %s != %s", a.Spec, b.Spec)
	}
	return true
}

func sameMembersFile(t *testing.T, sl []*db_api.FileItem, mp map[string]*db_api.FileItem) bool {
	t.Helper()
	for _, item := range sl {
		isEqualFileItem(t, item, mp[item.ID])
	}
	return true
}

func isEqualFileItem(t *testing.T, a, b *db_api.FileItem) bool {
	t.Helper()
	if a == nil || b == nil {
		t.Fatalf("Invalid items to compare")
	}
	if a.ID != b.ID {
		t.Fatalf("Mismatch id %s != %s", a.ID, b.ID)
	}
	if a.TenantID != b.TenantID {
		t.Fatalf("Mismatch TenantID %s != %s", a.TenantID, b.TenantID)
	}
	if a.Expiry != b.Expiry {
		t.Fatalf("Mismatch expiry %d != %d", a.Expiry, b.Expiry)
	}
	if !maps.Equal(a.Tags, b.Tags) {
		t.Fatalf("Mismatch tags %v != %v", a.Tags, b.Tags)
	}
	if a.Purpose != b.Purpose {
		t.Fatalf("Mismatch purpose %s != %s", a.Purpose, b.Purpose)
	}
	if !bytes.Equal(a.Spec, b.Spec) {
		t.Fatalf("Mismatch spec %s != %s", a.Spec, b.Spec)
	}
	if !bytes.Equal(a.Status, b.Status) {
		t.Fatalf("Mismatch status %s != %s", a.Spec, b.Spec)
	}
	return true
}
