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

package redis_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	dbredis "github.com/llm-d/llm-d-batch-gateway/internal/database/redis"
)

func TestPQDequeueAndClaim(t *testing.T) {
	newExchangeClient := func(t *testing.T) (*miniredis.Miniredis, *dbredis.ExchangeDBClientRedis) {
		t.Helper()
		minirds := miniredis.NewMiniRedis()
		if err := minirds.Start(); err != nil {
			t.Fatalf("failed to start miniredis: %v", err)
		}
		t.Cleanup(minirds.Close)
		baseClient, _, _, exchClient := setupRedisDSClients(t, "redis://"+minirds.Addr(), "")
		t.Cleanup(func() { _ = baseClient.Close() })
		return minirds, exchClient
	}

	t.Run("empty queue returns nil without claiming", func(t *testing.T) {
		t.Parallel()
		_, exch := newExchangeClient(t)
		ctx := context.Background()

		task, err := exch.PQDequeueAndClaim(ctx, "proc-1")
		if err != nil {
			t.Fatalf("PQDequeueAndClaim() err=%v, want nil", err)
		}
		if task != nil {
			t.Fatalf("PQDequeueAndClaim() task=%v, want nil", task)
		}

		entries, err := exch.InFlightGetAll(ctx)
		if err != nil {
			t.Fatalf("InFlightGetAll() err=%v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("in-flight entries=%d, want 0", len(entries))
		}
	})

	t.Run("empty processorID is rejected", func(t *testing.T) {
		t.Parallel()
		_, exch := newExchangeClient(t)

		if _, err := exch.PQDequeueAndClaim(context.Background(), ""); err == nil {
			t.Fatal("PQDequeueAndClaim(\"\") err=nil, want error")
		}
	})

	t.Run("claim removes from queue and records owner in one step", func(t *testing.T) {
		t.Parallel()
		_, exch := newExchangeClient(t)
		ctx := context.Background()

		if err := exch.PQEnqueue(ctx, &db_api.BatchJobPriority{
			ID:  "job-1",
			SLO: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("PQEnqueue() err=%v", err)
		}

		before := time.Now().Unix()
		task, err := exch.PQDequeueAndClaim(ctx, "proc-1")
		if err != nil {
			t.Fatalf("PQDequeueAndClaim() err=%v", err)
		}
		if task == nil || task.ID != "job-1" {
			t.Fatalf("PQDequeueAndClaim() task=%v, want job-1", task)
		}

		queued, err := exch.PQGetIDs(ctx)
		if err != nil {
			t.Fatalf("PQGetIDs() err=%v", err)
		}
		if queued["job-1"] {
			t.Fatal("claimed job still present in queue")
		}

		entries, err := exch.InFlightGetAll(ctx)
		if err != nil {
			t.Fatalf("InFlightGetAll() err=%v", err)
		}
		entry, ok := entries["job-1"]
		if !ok {
			t.Fatal("claimed job has no in-flight entry")
		}
		if entry.ProcessorID != "proc-1" {
			t.Fatalf("in-flight owner=%q, want proc-1", entry.ProcessorID)
		}
		if entry.LastSeen < before || entry.LastSeen > time.Now().Unix() {
			t.Fatalf("in-flight LastSeen=%d out of expected range", entry.LastSeen)
		}
	})

	t.Run("claims follow SLO priority order", func(t *testing.T) {
		t.Parallel()
		_, exch := newExchangeClient(t)
		ctx := context.Background()

		now := time.Now()
		// Enqueue out of priority order: the earliest SLO must come out first.
		for _, job := range []struct {
			id  string
			slo time.Time
		}{
			{id: "job-late", slo: now.Add(3 * time.Hour)},
			{id: "job-early", slo: now.Add(1 * time.Hour)},
			{id: "job-mid", slo: now.Add(2 * time.Hour)},
		} {
			if err := exch.PQEnqueue(ctx, &db_api.BatchJobPriority{ID: job.id, SLO: job.slo}); err != nil {
				t.Fatalf("PQEnqueue(%s) err=%v", job.id, err)
			}
		}

		wantOrder := []string{"job-early", "job-mid", "job-late"}
		for _, want := range wantOrder {
			task, err := exch.PQDequeueAndClaim(ctx, "proc-1")
			if err != nil {
				t.Fatalf("PQDequeueAndClaim() err=%v", err)
			}
			if task == nil || task.ID != want {
				t.Fatalf("PQDequeueAndClaim() task=%v, want %s", task, want)
			}
		}
	})

	t.Run("corrupt queue member returns error without claiming", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			member string
		}{
			{name: "malformed JSON", member: "not-json"},
			{name: "empty ID", member: `{"id":""}`},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				minirds, exch := newExchangeClient(t)
				ctx := context.Background()

				// Discover the (unexported) queue key by enqueueing a real job,
				// then replace its contents with the corrupt member.
				if err := exch.PQEnqueue(ctx, &db_api.BatchJobPriority{
					ID:  "job-probe",
					SLO: time.Now().Add(time.Hour),
				}); err != nil {
					t.Fatalf("PQEnqueue() err=%v", err)
				}
				var queueKey string
				for _, key := range minirds.Keys() {
					if strings.Contains(key, "priority") {
						queueKey = key
						break
					}
				}
				if queueKey == "" {
					t.Fatal("could not find priority queue key in miniredis")
				}
				minirds.Del(queueKey)
				if _, err := minirds.ZAdd(queueKey, 1, tc.member); err != nil {
					t.Fatalf("ZAdd() err=%v", err)
				}

				task, err := exch.PQDequeueAndClaim(ctx, "proc-1")
				if err == nil {
					t.Fatalf("PQDequeueAndClaim() err=nil, want error (task=%v)", task)
				}

				members, err := minirds.ZMembers(queueKey)
				if err != nil {
					t.Fatalf("ZMembers() err=%v", err)
				}
				if len(members) != 1 || members[0] != tc.member {
					t.Fatalf("queue members=%v, want [%s]", members, tc.member)
				}

				entries, err := exch.InFlightGetAll(ctx)
				if err != nil {
					t.Fatalf("InFlightGetAll() err=%v", err)
				}
				if len(entries) != 0 {
					t.Fatalf("corrupt member produced in-flight entries: %v", entries)
				}
			})
		}
	})
}
