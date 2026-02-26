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
	// "bytes"
	"context"
	// "maps"

	// "maps"
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
		t.Fatalf("Failed to create batch redis client: %v", err)
	}
	exchClient, err := dbredis.NewExchangeDBClientRedis(ctx, baseClient, nil, 0)
	if err != nil {
		t.Fatalf("Failed to create batch redis client: %v", err)
	}
	return baseClient, batchClient, fileClient, exchClient
}

func TestRedisClient(t *testing.T) {

	//redisUrl := os.Getenv("REDIS_URL") TBD
	redisUrl := "redis://localhost:6379"
	redisCaCert := os.Getenv("REDIS_CACERT_PATH")
	var (
		minirds *miniredis.Miniredis
		tagKey1 string = "key-tag-1"
		tagKey2 string = "key-tag-2"
		// tagKey3 string = "key-tag-3"
		tagVal1 string = "val-tag-1"
		tagVal2 string = "val-tag-2"
		// tagVal3 string = "val-tag-3"
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

	t.Run("db operations", func(t *testing.T) {
		baseClient, batchClient, _, _ := setupRedisClients(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			baseClient.Close()
		})

		// nBatches := 20
		nBatchesRmv := 10
		var wg sync.WaitGroup
		_, batchesRmv := make(map[string]*db_api.BatchItem), make(map[string]*db_api.BatchItem)
		for i := 0; i < nBatchesRmv; i++ {
			batchID := uuid.New().String()
			batch := &db_api.BatchItem{
				BaseIndexes: db_api.BaseIndexes{
					ID:       batchID,
					TenantID: "T1",
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
		// var jobIDs []string
		// for i := 0; i < nJobs; i++ {
		// 	jobID := uuid.New().String()
		// 	job := &db_api.BatchItem{
		// 		ID:     jobID,
		// 		Expiry: time.Now().Add(time.Hour).Unix(),
		// 		Tags:   map[string]string{tagKey1: tagVal1, tagKey2: tagVal2},
		// 		Spec:   []byte("spec"),
		// 		Status: []byte("status"),
		// 	}
		// 	jobs[jobID] = job
		// 	jobIDs = append(jobIDs, jobID)
		// 	wg.Add(1)
		// 	go func() {
		// 		defer wg.Done()
		// 		ID, err := dbClient.DBStore(context.Background(), job)
		// 		if err != nil {
		// 			t.Fatalf("Failed to store item: %v", err)
		// 		}
		// 		if ID != job.ID {
		// 			t.Fatalf("IDs mismatch %s != %s", ID, jobID)
		// 		}
		// 	}()
		// }
		// wg.Wait()
		// time.Sleep(3 * time.Second) // To pass the expiry time of the short expiry jobs.

		// resJobs, _, _, err := dbClient.DBGet(context.Background(),
		// 	&db_api.BatchDBQuery{
		// 		Expired: true,
		// 	}, true, 0, nJobsRmv*2)
		// if err != nil {
		// 	t.Fatalf("Failed to get items: %v", err)
		// }
		// if len(resJobs) != nJobsRmv {
		// 	t.Fatalf("Invalid number of items %d != %d", len(resJobs), nJobsRmv)
		// }
		// for _, resJob := range resJobs {
		// 	tJob := jobsRmv[resJob.ID]
		// 	if resJob.ID != tJob.ID {
		// 		t.Fatalf("Mismatch id %s != %s", resJob.ID, tJob.ID)
		// 	}
		// 	if resJob.Expiry != tJob.Expiry {
		// 		t.Fatalf("Mismatch expiry %d != %d", resJob.Expiry, tJob.Expiry)
		// 	}
		// 	if !bytes.Equal(resJob.Spec, tJob.Spec) {
		// 		t.Fatalf("Mismatch spec %s != %s", resJob.Spec, tJob.Spec)
		// 	}
		// 	if !bytes.Equal(resJob.Status, tJob.Status) {
		// 		t.Fatalf("Mismatch status %s != %s", resJob.Spec, tJob.Spec)
		// 	}
		// 	if !maps.Equal(resJob.Tags, tJob.Tags) {
		// 		t.Fatalf("Mismatch tags %v != %v", resJob.Tags, tJob.Tags)
		// 	}
		// }

		// resJobs, _, _, err = dbClient.DBGet(context.Background(),
		// 	&db_api.BatchDBQuery{
		// 		IDs: jobIDs,
		// 	}, true, 0, nJobs*2)
		// if err != nil {
		// 	t.Fatalf("Failed to get items: %v", err)
		// }
		// if len(resJobs) != nJobs {
		// 	t.Fatalf("Invalid number of items %d != %d", len(resJobs), nJobs)
		// }
		// for _, resJob := range resJobs {
		// 	tJob := jobs[resJob.ID]
		// 	if resJob.ID != tJob.ID {
		// 		t.Fatalf("Mismatch id %s != %s", resJob.ID, tJob.ID)
		// 	}
		// 	// if !resJob.SLO.Equal(tJob.SLO) {
		// 	// 	t.Fatalf("Mismatch slo %s != %s", resJob.SLO, tJob.SLO)
		// 	// }
		// 	if !bytes.Equal(resJob.Spec, tJob.Spec) {
		// 		t.Fatalf("Mismatch spec %s != %s", resJob.Spec, tJob.Spec)
		// 	}
		// 	if !bytes.Equal(resJob.Status, tJob.Status) {
		// 		t.Fatalf("Mismatch status %s != %s", resJob.Spec, tJob.Spec)
		// 	}
		// 	// if !slices.Equal(resJob.Tags, tJob.Tags) { TBD
		// 	// 	t.Fatalf("Mismatch tags %s != %s", resJob.Spec, tJob.Spec)
		// 	// }
		// }
	})

}
