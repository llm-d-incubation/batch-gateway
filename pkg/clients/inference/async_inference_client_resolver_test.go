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
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestNewAsyncResolver(t *testing.T) {
	t.Run("creates per-model clients", func(t *testing.T) {
		mr := miniredis.RunT(t)

		cfg := AsyncClientConfig{
			RedisURL:          "redis://" + mr.Addr(),
			Models:            map[string]string{"model-a": "pool-a", "model-b": "pool-b"},
			ResultPollTimeout: time.Second,
		}

		r, err := NewAsyncResolver(cfg, testLogger(t))
		if err != nil {
			t.Fatalf("NewAsyncResolver: %v", err)
		}

		if got := r.ClientFor("model-a"); got == nil {
			t.Fatal("expected non-nil client for model-a")
		}
		if got := r.ClientFor("model-b"); got == nil {
			t.Fatal("expected non-nil client for model-b")
		}
		if got := r.ClientFor("unknown"); got != nil {
			t.Fatalf("expected nil for unknown model, got %v", got)
		}
	})

	t.Run("returns nil for unknown model", func(t *testing.T) {
		mr := miniredis.RunT(t)

		r, err := NewAsyncResolver(AsyncClientConfig{
			RedisURL:          "redis://" + mr.Addr(),
			Models:            map[string]string{"model-a": "pool-a"},
			ResultPollTimeout: time.Second,
		}, testLogger(t))
		if err != nil {
			t.Fatalf("NewAsyncResolver: %v", err)
		}

		if got := r.ClientFor("unknown"); got != nil {
			t.Fatalf("expected nil for unknown model, got %v", got)
		}
	})

	t.Run("rejects duplicate pool mapping", func(t *testing.T) {
		mr := miniredis.RunT(t)

		_, err := NewAsyncResolver(AsyncClientConfig{
			RedisURL:          "redis://" + mr.Addr(),
			Models:            map[string]string{"model-a": "shared-pool", "model-b": "shared-pool"},
			ResultPollTimeout: time.Second,
		}, testLogger(t))
		if err == nil {
			t.Fatal("expected error for duplicate pool mapping")
		}
	})

	t.Run("invalid Redis URL returns error", func(t *testing.T) {
		_, err := NewAsyncResolver(AsyncClientConfig{
			RedisURL:          "not-a-url",
			Models:            map[string]string{"model-a": "pool-a"},
			ResultPollTimeout: time.Second,
		}, testLogger(t))
		if err == nil {
			t.Fatal("expected error for invalid Redis URL")
		}
	})

	t.Run("close releases resources", func(t *testing.T) {
		mr := miniredis.RunT(t)

		r, err := NewAsyncResolver(AsyncClientConfig{
			RedisURL:          "redis://" + mr.Addr(),
			Models:            map[string]string{"model-a": "pool-a"},
			ResultPollTimeout: time.Second,
		}, testLogger(t))
		if err != nil {
			t.Fatalf("NewAsyncResolver: %v", err)
		}

		if err := r.Close(); err != nil {
			t.Fatalf("Close() returned error: %v", err)
		}
	})

	t.Run("each ClientFor call returns a fresh client", func(t *testing.T) {
		mr := miniredis.RunT(t)

		r, err := NewAsyncResolver(AsyncClientConfig{
			RedisURL:          "redis://" + mr.Addr(),
			Models:            map[string]string{"model-a": "pool-a"},
			ResultPollTimeout: time.Second,
		}, testLogger(t))
		if err != nil {
			t.Fatalf("NewAsyncResolver: %v", err)
		}

		client1 := r.ClientFor("model-a")
		client2 := r.ClientFor("model-a")
		if client1 == client2 {
			t.Fatal("expected fresh client per ClientFor call")
		}
	})
}
