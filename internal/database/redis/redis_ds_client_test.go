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
		var batchIDs []string
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
			batchIDs = append(batchIDs, batchID)
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
		time.Sleep(3 * time.Second) // To pass the expiry time of the short expiry jobs.

		// Get expired.
		expectMore := true
		nRet, cursor := 0, 0
		for expectMore {
			resJobs, cur, expectM, err := batchClient.DBGet(context.Background(),
				&db_api.BatchQuery{
					BaseQuery: db_api.BaseQuery{
						Expired: true,
					},
				}, true, cursor, nBatchesRmv*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			for _, resJob := range resJobs {
				tJob := batchesRmv[resJob.ID]
				if resJob.ID != tJob.ID {
					t.Fatalf("Mismatch id %s != %s", resJob.ID, tJob.ID)
				}
				if resJob.TenantID != tJob.TenantID {
					t.Fatalf("Mismatch TenantID %s != %s", resJob.TenantID, tJob.TenantID)
				}
				if resJob.Expiry != tJob.Expiry {
					t.Fatalf("Mismatch expiry %d != %d", resJob.Expiry, tJob.Expiry)
				}
				if !bytes.Equal(resJob.Spec, tJob.Spec) {
					t.Fatalf("Mismatch spec %s != %s", resJob.Spec, tJob.Spec)
				}
				if !bytes.Equal(resJob.Status, tJob.Status) {
					t.Fatalf("Mismatch status %s != %s", resJob.Spec, tJob.Spec)
				}
				if !maps.Equal(resJob.Tags, tJob.Tags) {
					t.Fatalf("Mismatch tags %v != %v", resJob.Tags, tJob.Tags)
				}
				nRet++
			}
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
			resJobs, cur, expectM, err := batchClient.DBGet(context.Background(),
				&db_api.BatchQuery{
					BaseQuery: db_api.BaseQuery{
						IDs: batchIDs,
					},
				}, true, cursor, nBatches*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			for _, resJob := range resJobs {
				tJob := batches[resJob.ID]
				if resJob.ID != tJob.ID {
					t.Fatalf("Mismatch id %s != %s", resJob.ID, tJob.ID)
				}
				if resJob.TenantID != tJob.TenantID {
					t.Fatalf("Mismatch TenantID %s != %s", resJob.TenantID, tJob.TenantID)
				}
				if resJob.Expiry != tJob.Expiry {
					t.Fatalf("Mismatch expiry %d != %d", resJob.Expiry, tJob.Expiry)
				}
				if !bytes.Equal(resJob.Spec, tJob.Spec) {
					t.Fatalf("Mismatch spec %s != %s", resJob.Spec, tJob.Spec)
				}
				if !bytes.Equal(resJob.Status, tJob.Status) {
					t.Fatalf("Mismatch status %s != %s", resJob.Spec, tJob.Spec)
				}
				if !maps.Equal(resJob.Tags, tJob.Tags) {
					t.Fatalf("Mismatch tags %v != %v", resJob.Tags, tJob.Tags)
				}
				nRet++
			}
			expectMore = expectM
			cursor = cur
		}
		if nRet != nBatches {
			t.Fatalf("Invalid number of items %d != %d", nRet, nBatchesRmv)
		}

		// Get by tenant.
		expectMore = true
		nRet, cursor = 0, 0
		for expectMore {
			resJobs, cur, expectM, err := batchClient.DBGet(context.Background(),
				&db_api.BatchQuery{
					BaseQuery: db_api.BaseQuery{
						TenantID: "Tnt2",
					},
				}, true, cursor, nBatchesRmv*2)
			if err != nil {
				t.Fatalf("Failed to get items: %v", err)
			}
			for _, resJob := range resJobs {
				tJob := batchesRmv[resJob.ID]
				if resJob.ID != tJob.ID {
					t.Fatalf("Mismatch id %s != %s", resJob.ID, tJob.ID)
				}
				if resJob.TenantID != tJob.TenantID {
					t.Fatalf("Mismatch TenantID %s != %s", resJob.TenantID, tJob.TenantID)
				}
				if resJob.Expiry != tJob.Expiry {
					t.Fatalf("Mismatch expiry %d != %d", resJob.Expiry, tJob.Expiry)
				}
				if !bytes.Equal(resJob.Spec, tJob.Spec) {
					t.Fatalf("Mismatch spec %s != %s", resJob.Spec, tJob.Spec)
				}
				if !bytes.Equal(resJob.Status, tJob.Status) {
					t.Fatalf("Mismatch status %s != %s", resJob.Spec, tJob.Spec)
				}
				if !maps.Equal(resJob.Tags, tJob.Tags) {
					t.Fatalf("Mismatch tags %v != %v", resJob.Tags, tJob.Tags)
				}
				nRet++
			}
			expectMore = expectM
			cursor = cur
		}
		if nRet != nBatchesRmv {
			t.Fatalf("Invalid number of items %d != %d", nRet, nBatchesRmv)
		}

		// Get by tags.
		// expectMore = true
		// nRet, cursor = 0, 0
		// for expectMore {
		// 	resJobs, cur, expectM, err := batchClient.DBGet(context.Background(),
		// 		&db_api.BatchQuery{
		// 			BaseQuery: db_api.BaseQuery{
		// 				TagSelectors:    db_api.Tags{tagKey1: tagVal1, tagKey3: tagVal3},
		// 				TagsLogicalCond: db_api.LogicalCondAnd,
		// 			},
		// 		}, true, cursor, nBatches*2)
		// 	if err != nil {
		// 		t.Fatalf("Failed to get items: %v", err)
		// 	}
		// 	for _, resJob := range resJobs {
		// 		tJob := batches[resJob.ID]
		// 		if resJob.ID != tJob.ID {
		// 			t.Fatalf("Mismatch id %s != %s", resJob.ID, tJob.ID)
		// 		}
		// 		if resJob.TenantID != tJob.TenantID {
		// 			t.Fatalf("Mismatch TenantID %s != %s", resJob.TenantID, tJob.TenantID)
		// 		}
		// 		if resJob.Expiry != tJob.Expiry {
		// 			t.Fatalf("Mismatch expiry %d != %d", resJob.Expiry, tJob.Expiry)
		// 		}
		// 		if !bytes.Equal(resJob.Spec, tJob.Spec) {
		// 			t.Fatalf("Mismatch spec %s != %s", resJob.Spec, tJob.Spec)
		// 		}
		// 		if !bytes.Equal(resJob.Status, tJob.Status) {
		// 			t.Fatalf("Mismatch status %s != %s", resJob.Spec, tJob.Spec)
		// 		}
		// 		if !maps.Equal(resJob.Tags, tJob.Tags) {
		// 			t.Fatalf("Mismatch tags %v != %v", resJob.Tags, tJob.Tags)
		// 		}
		// 		nRet++
		// 	}
		// 	expectMore = expectM
		// 	cursor = cur
		// }
		// if nRet != nBatches {
		// 	t.Fatalf("Invalid number of items %d != %d", nRet, nBatchesRmv)
		// }

	})

}
