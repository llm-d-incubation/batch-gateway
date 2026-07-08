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
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	dbredis "github.com/llm-d/llm-d-batch-gateway/internal/database/redis"
)

// Mirrors of the unexported key names in the redis package, for direct
// store manipulation in sabotage tests.
const (
	queueKey    = "llmd_batch:queue:priority"
	inFlightKey = "llmd_batch:inflight"
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

	t.Run("corrupt queue member is dropped with an error, without claiming", func(t *testing.T) {
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

				if _, err := minirds.ZAdd(queueKey, 1, tc.member); err != nil {
					t.Fatalf("ZAdd() err=%v", err)
				}

				task, err := exch.PQDequeueAndClaim(ctx, "proc-1")
				if err == nil {
					t.Fatalf("PQDequeueAndClaim() err=nil, want error (task=%v)", task)
				}

				// The corrupt member must stay dropped: restoring it would put
				// it back at the queue head and starve everything behind it.
				members, _ := minirds.ZMembers(queueKey)
				if len(members) != 0 {
					t.Fatalf("queue members=%v, want empty (corrupt member dropped)", members)
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

	t.Run("valid job behind corrupt member is claimed on the next call", func(t *testing.T) {
		t.Parallel()
		minirds, exch := newExchangeClient(t)
		ctx := context.Background()

		// Corrupt member at the queue head, valid job behind it.
		if _, err := minirds.ZAdd(queueKey, 1, "not-json"); err != nil {
			t.Fatalf("ZAdd() err=%v", err)
		}
		if err := exch.PQEnqueue(ctx, &db_api.BatchJobPriority{
			ID:  "job-behind",
			SLO: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("PQEnqueue() err=%v", err)
		}

		if _, err := exch.PQDequeueAndClaim(ctx, "proc-1"); err == nil {
			t.Fatal("first PQDequeueAndClaim() err=nil, want corrupt-member error")
		}

		task, err := exch.PQDequeueAndClaim(ctx, "proc-1")
		if err != nil {
			t.Fatalf("second PQDequeueAndClaim() err=%v, want valid job", err)
		}
		if task == nil || task.ID != "job-behind" {
			t.Fatalf("second PQDequeueAndClaim() task=%v, want job-behind", task)
		}
	})

	t.Run("claim write failure does not lose the job", func(t *testing.T) {
		t.Parallel()
		minirds, exch := newExchangeClient(t)
		ctx := context.Background()

		if err := exch.PQEnqueue(ctx, &db_api.BatchJobPriority{
			ID:  "job-1",
			SLO: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("PQEnqueue() err=%v", err)
		}

		// Sabotage the in-flight key so HSET fails with WRONGTYPE.
		if err := minirds.Set(inFlightKey, "not-a-hash"); err != nil {
			t.Fatalf("Set() err=%v", err)
		}

		task, err := exch.PQDequeueAndClaim(ctx, "proc-1")
		if err == nil {
			t.Fatalf("PQDequeueAndClaim() err=nil, want claim-write error (task=%v)", task)
		}

		// The job must be restored to the queue, not lost between the stores.
		queued, err := exch.PQGetIDs(ctx)
		if err != nil {
			t.Fatalf("PQGetIDs() err=%v", err)
		}
		if !queued["job-1"] {
			t.Fatal("job lost: not in queue after failed claim write")
		}
	})
}
