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

// Test for the redis client utilities.

package redis_test

import (
	"context"
	"os"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/llm-d-incubation/batch-gateway/internal/util/redis"
	utls "github.com/llm-d-incubation/batch-gateway/internal/util/tls"
	gredis "github.com/redis/go-redis/v9"
)

func setupRedisClient(t *testing.T, redisUrl, redisCaCert string) *gredis.Client {
	t.Helper()
	cfg := &redis.RedisClientConfig{
		Url:         redisUrl,
		ServiceName: "test-service",
	}
	if redisCaCert != "" {
		cfg.EnableTLS = true
		cfg.Certificates = &utls.Certificates{
			CaCertFile: redisCaCert,
		}
	}
	rds, err := redis.NewRedisClient(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Failed to create redis client: %v", err)
	}
	return rds
}

func TestRedisClient(t *testing.T) {
	redisUrl := os.Getenv("REDIS_URL")
	redisCaCert := os.Getenv("REDIS_CACERT_PATH")

	var minirds *miniredis.Miniredis

	// Setup: start miniredis if no external redis URL is provided
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
		rds := setupRedisClient(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = rds.Close()
		})
		t.Logf("Memory address of redis client: %p", rds)
		if rds == nil {
			t.Fatal("Expected redis client to be non-nil")
		}
	})

	t.Run("basic operations", func(t *testing.T) {
		rds := setupRedisClient(t, redisUrl, redisCaCert)
		t.Cleanup(func() {
			_ = rds.Close()
		})

		_, err := rds.Set(context.Background(), "k1", "v1", -1).Result()
		if err != nil {
			t.Fatalf("Failed to set key: %v", err)
		}

		val, err := rds.Get(context.Background(), "k1").Result()
		if err != nil {
			t.Fatalf("Failed to get key: %v", err)
		}
		if val != "v1" {
			t.Errorf("Expected value 'v1', got '%s'", val)
		}

		_, err = rds.Del(context.Background(), "k1").Result()
		if err != nil {
			t.Fatalf("Failed to delete key: %v", err)
		}
	})

	t.Run("negative case", func(t *testing.T) {
		cfgInv := &redis.RedisClientConfig{
			Url:         "redis://invalid-url",
			ServiceName: "test-service",
		}
		rdsInv, errInv := redis.NewRedisClient(context.Background(), cfgInv)
		if errInv == nil {
			t.Fatal("Expected error when creating redis client with invalid URL, got nil")
		}
		if rdsInv != nil {
			t.Errorf("Expected redis client to be nil with invalid URL, got %v", rdsInv)
		}
	})
}
