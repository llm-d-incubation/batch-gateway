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
	"maps"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/alicebob/miniredis/v2"
	db_api "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	dbredis "github.com/llm-d-incubation/batch-gateway/internal/database/redis"
	uredis "github.com/llm-d-incubation/batch-gateway/internal/util/redis"
	utls "github.com/llm-d-incubation/batch-gateway/internal/util/tls"
)

func setupRedisClients(t *testing.T, redisUrl, redisCaCert string) (
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

func TestRedisClient(t *testing.T) {

	redisUrl := os.Getenv("TEST_REDIS_URL")
	redisCaCert := os.Getenv("TEST_REDIS_CACERT_PATH")
	var (
		minirds *miniredis.Miniredis
		tagKey1 string = "key-tag-1"
		tagKey2 string = "key-tag-2"
		tagKey3 string = "key-tag-3"
		tagVal1 string = "val-tag-1"
		tagVal2 string = "val-tag-2"
		tagVal3 string = "val-tag-3"
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

	t.Run("creates clients", func(t *testing.T) {
		baseClient, batchClient, fileClient, exchClient := setupRedisClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			baseClient.Close()
		})
		t.Logf("Memory address of the clients: base=%p batch=%p file=%p exchange=%p",
			baseClient, batchClient, fileClient, exchClient)
		if baseClient == nil || batchClient == nil || fileClient == nil || exchClient == nil {
			t.Fatal("Expected redis clients to be non-nil")
		}
	})

	t.Run("batch db operations", func(t *testing.T) {
		baseClient, batchClient, _, _ := setupRedisClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			baseClient.Close()
		})

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
					t.Fatalf("Failed to store item: %v", err)
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
					t.Fatalf("Failed to store item: %v", err)
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
		if !sameMembersInStrSlice(deletedIDs, batchesAllIDs) {
			t.Fatalf("Deletion IDs mismatch: %v != %v", deletedIDs, batchesAllIDs)
		}

	})

	t.Run("file db operations", func(t *testing.T) {
		baseClient, _, fileClient, _ := setupRedisClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			baseClient.Close()
		})

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
					t.Fatalf("Failed to store item: %v", err)
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
					t.Fatalf("Failed to store item: %v", err)
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
		if !sameMembersInStrSlice(deletedIDs, filesAllIDs) {
			t.Fatalf("Deletion IDs mismatch: %v != %v", deletedIDs, filesAllIDs)
		}

	})

}

func sameMembersBatch(t *testing.T, sl []*db_api.BatchItem, mp map[string]*db_api.BatchItem) bool {
	for _, item := range sl {
		isEqualBatchItem(t, item, mp[item.ID])
	}
	return true
}

func isEqualBatchItem(t *testing.T, a, b *db_api.BatchItem) bool {
	if a == nil || b == nil {
		t.Fatal("Invalid items to compare")
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
	for _, item := range sl {
		isEqualFileItem(t, item, mp[item.ID])
	}
	return true
}

func isEqualFileItem(t *testing.T, a, b *db_api.FileItem) bool {
	if a == nil || b == nil {
		t.Fatal("Invalid items to compare")
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

func sameMembersInStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int)
	for _, x := range a {
		counts[x]++
	}
	for _, x := range b {
		counts[x]--
		if counts[x] < 0 {
			return false
		}
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}
