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

package inference

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/llm-d/llm-d-async/api"
	"github.com/llm-d/llm-d-async/producer"
	"github.com/redis/go-redis/v9"
)

func TestAsyncSharedClient_Submit_roundtrip(t *testing.T) {
	t.Run("enqueues request and returns result via GetResult", func(t *testing.T) {
		mr := miniredis.RunT(t)
		poolName := "test-pool"
		reqQueue := asyncQueuePrefix + "requests:" + poolName
		resultQueue := asyncQueuePrefix + "results:" + poolName

		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		defer func() { _ = rdb.Close() }()

		p, err := producer.NewRedisSortedSetProducer(
			producer.RedisSortedSetConfig{
				RequestQueueName: asyncQueuePrefix + "requests:" + poolName,
				ResultQueueName:  asyncQueuePrefix + "results:" + poolName,
			},
			producer.WithRedisClient(rdb),
		)
		if err != nil {
			t.Fatalf("NewRedisSortedSetProducer: %v", err)
		}
		defer func() { _ = p.Close() }()

		client := newAsyncSharedClient(p, time.Second, testLogger(t))

		go func() {
			time.Sleep(50 * time.Millisecond)
			data, _ := json.Marshal(api.ResultMessage{
				ID:         "req-1",
				StatusCode: 200,
				Payload:    `{"choices":[{"text":"hello"}]}`,
			})
			if _, lpushErr := mr.Lpush(resultQueue, string(data)); lpushErr != nil {
				t.Errorf("Lpush: %v", lpushErr)
			}
		}()

		if submitErr := client.Submit(context.Background(), &GenerateRequest{
			RequestID: "req-1",
			Endpoint:  "/v1/completions",
			Params:    map[string]any{"model": "test-model", "prompt": "hello"},
		}); submitErr != nil {
			t.Fatalf("Submit() error: %s", submitErr.Message)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		resp, getErr := client.GetResult(ctx)
		if getErr != nil {
			t.Fatalf("GetResult error: %v", getErr)
		}

		if resp.RequestID != "req-1" {
			t.Errorf("RequestID = %q, want %q", resp.RequestID, "req-1")
		}
		if resp.StatusCode != 200 {
			t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
		}
		if resp.Response == nil {
			t.Fatal("expected non-nil Response")
		}

		members, zErr := rdb.ZRange(context.Background(), reqQueue, 0, -1).Result()
		if zErr != nil {
			t.Fatalf("ZRange: %v", zErr)
		}
		if len(members) != 1 {
			t.Fatalf("expected 1 member in request queue, got %d", len(members))
		}

		var envelope map[string]json.RawMessage
		if uErr := json.Unmarshal([]byte(members[0]), &envelope); uErr != nil {
			t.Fatalf("unmarshal enqueued request: %v", uErr)
		}
		var data api.RequestMessage
		if uErr := json.Unmarshal(envelope["data"], &data); uErr != nil {
			t.Fatalf("unmarshal data field: %v", uErr)
		}
		if data.ID != "req-1" {
			t.Errorf("enqueued request ID = %q, want %q", data.ID, "req-1")
		}
	})

	t.Run("propagates HTTP error status code from ResultMessage", func(t *testing.T) {
		mr := miniredis.RunT(t)
		poolName := "test-pool-403"
		resultQueue := asyncQueuePrefix + "results:" + poolName

		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		defer func() { _ = rdb.Close() }()

		p, err := producer.NewRedisSortedSetProducer(
			producer.RedisSortedSetConfig{
				RequestQueueName: asyncQueuePrefix + "requests:" + poolName,
				ResultQueueName:  resultQueue,
			},
			producer.WithRedisClient(rdb),
		)
		if err != nil {
			t.Fatalf("NewRedisSortedSetProducer: %v", err)
		}
		defer func() { _ = p.Close() }()

		client := newAsyncSharedClient(p, time.Second, testLogger(t))

		data, _ := json.Marshal(api.ResultMessage{
			ID:         "req-403",
			StatusCode: 403,
			Payload:    "",
		})
		if _, lpushErr := mr.Lpush(resultQueue, string(data)); lpushErr != nil {
			t.Fatalf("Lpush: %v", lpushErr)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		resp, getErr := client.GetResult(ctx)
		if getErr != nil {
			t.Fatalf("GetResult error: %v", getErr)
		}
		if resp.RequestID != "req-403" {
			t.Errorf("RequestID = %q, want %q", resp.RequestID, "req-403")
		}
		if resp.StatusCode != 403 {
			t.Errorf("StatusCode = %d, want 403", resp.StatusCode)
		}
		if len(resp.Response) != 0 {
			t.Errorf("Response = %q, want empty", resp.Response)
		}
	})
}
