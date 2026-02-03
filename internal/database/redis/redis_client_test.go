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

func setupRedisClient(t *testing.T, redisUrl, redisCaCert string) *dbredis.BatchDSClientRedis {
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
	db_rds, err := dbredis.NewBatchDSClientRedis(context.Background(), cfg, 0, "testTable")
	if err != nil {
		t.Fatalf("Failed to create db redis client: %v", err)
	}
	return db_rds
}

func TestRedisClient(t *testing.T) {

	redisUrl := os.Getenv("REDIS_URL")
	redisCaCert := os.Getenv("REDIS_CACERT_PATH")
	var (
		minirds *miniredis.Miniredis
		//testTagVal1    string              = "test-tag-1"
		//testTagVal2    string              = "test-tag-2"
		tagKey1 string = "key-tag-1"
		tagKey2 string = "key-tag-2"
		tagVal1 string = "val-tag-1"
		tagVal2 string = "val-tag-2"
		//tagVal3        string              = "dif-tag-3"
	)

	// Setup: start miniredis if no external redis URL is provided.
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

	t.Run("creates client", func(t *testing.T) {
		dbClient := setupRedisClient(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			dbClient.Close()
		})
		t.Logf("Memory address of db redis client: %p", dbClient)
		if dbClient == nil {
			t.Fatal("Expected db redis client to be non-nil")
		}
	})

	t.Run("db operations", func(t *testing.T) {
		dbClient := setupRedisClient(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			dbClient.Close()
		})

		nJobs := 40
		nJobsRmv := 10
		var wg sync.WaitGroup
		jobs := make(map[string]*db_api.BatchItem)
		for i := 0; i < nJobsRmv; i++ {
			jobID := uuid.New().String()
			job := &db_api.BatchItem{
				ID: jobID,
				// SLO:    time.Now().Add(time.Hour),
				// TTL:    1,
				Tags:   map[string]string{tagKey1: tagVal1, tagKey2: tagVal2},
				Spec:   []byte("spec"),
				Status: []byte("status"),
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				ID, err := dbClient.DBStore(context.Background(), job)
				if err != nil {
					t.Fatalf("Failed to store item: %v", err)
				}
				if ID != job.ID {
					t.Fatalf("IDs mismatch %s != %s", ID, jobID)
				}
			}()
		}
		wg.Wait()
		var jobIDs []string
		for i := 0; i < nJobs; i++ {
			jobID := uuid.New().String()
			job := &db_api.BatchItem{
				ID: jobID,
				// SLO:    time.Now().Add(time.Hour),
				// TTL:    10000,
				Tags:   map[string]string{tagKey1: tagVal1, tagKey1: tagVal2},
				Spec:   []byte("spec"),
				Status: []byte("status"),
			}
			jobs[jobID] = job
			jobIDs = append(jobIDs, jobID)
			wg.Add(1)
			go func() {
				defer wg.Done()
				ID, err := dbClient.DBStore(context.Background(), job)
				if err != nil {
					t.Fatalf("Failed to store item: %v", err)
				}
				if ID != job.ID {
					t.Fatalf("IDs mismatch %s != %s", ID, jobID)
				}
			}()
		}
		wg.Wait()
		time.Sleep(1 * time.Second) // To make sure the short ttl jobs get expired.

		resJobs, _, _, err := dbClient.DBGet(context.Background(),
			&db_api.BatchDBQuery{
				IDs: jobIDs,
			}, true, 0, nJobs*2)
		if err != nil {
			t.Fatalf("Failed to get items: %v", err)
		}
		if len(resJobs) != nJobs {
			t.Fatalf("Invalid number of items %d != %d", len(resJobs), nJobs)
		}
		for _, resJob := range resJobs {
			tJob := jobs[resJob.ID]
			if resJob.ID != tJob.ID {
				t.Fatalf("Mismatch id %s != %s", resJob.ID, tJob.ID)
			}
			// if !resJob.SLO.Equal(tJob.SLO) {
			// 	t.Fatalf("Mismatch slo %s != %s", resJob.SLO, tJob.SLO)
			// }
			if !bytes.Equal(resJob.Spec, tJob.Spec) {
				t.Fatalf("Mismatch spec %s != %s", resJob.Spec, tJob.Spec)
			}
			if !bytes.Equal(resJob.Status, tJob.Status) {
				t.Fatalf("Mismatch status %s != %s", resJob.Spec, tJob.Spec)
			}
			// if !slices.Equal(resJob.Tags, tJob.Tags) { TBD
			// 	t.Fatalf("Mismatch tags %s != %s", resJob.Spec, tJob.Spec)
			// }
		}
	})

}
