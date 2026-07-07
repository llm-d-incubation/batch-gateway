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

package regression

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	dbredis "github.com/llm-d/llm-d-batch-gateway/internal/database/redis"
	uredis "github.com/llm-d/llm-d-batch-gateway/internal/util/redis"
)

// Past bug guard: job pickup used to be two separate Redis operations — pop
// from the priority queue, then write the in-flight claim. A processor crash
// between the two left the job in neither store. PQDequeueAndClaim does both
// in one Lua script; these tests pin that invariant.
func TestRegression_AtomicDequeueClaim(t *testing.T) {
	newExchangeClient := func(t *testing.T) *dbredis.ExchangeDBClientRedis {
		t.Helper()
		minirds := miniredis.NewMiniRedis()
		if err := minirds.Start(); err != nil {
			t.Fatalf("failed to start miniredis: %v", err)
		}
		t.Cleanup(minirds.Close)
		exch, err := dbredis.NewExchangeDBClientRedis(context.Background(), nil,
			&uredis.RedisClientConfig{
				Url:         "redis://" + minirds.Addr(),
				ServiceName: "regression-test",
			}, 0)
		if err != nil {
			t.Fatalf("failed to create exchange client: %v", err)
		}
		t.Cleanup(func() { _ = exch.Close() })
		return exch
	}

	t.Run("ClaimIsAtomicPair", func(t *testing.T) {
		// Once a claim returns, both effects are visible: gone from the
		// queue, owned in the in-flight hash.
		exch := newExchangeClient(t)
		ctx := context.Background()

		if err := exch.PQEnqueue(ctx, &db_api.BatchJobPriority{
			ID:  "job-atomic",
			SLO: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("PQEnqueue() err=%v", err)
		}

		task, err := exch.PQDequeueAndClaim(ctx, "proc-A")
		if err != nil {
			t.Fatalf("PQDequeueAndClaim() err=%v", err)
		}
		if task == nil || task.ID != "job-atomic" {
			t.Fatalf("PQDequeueAndClaim() task=%v, want job-atomic", task)
		}

		queued, err := exch.PQGetIDs(ctx)
		if err != nil {
			t.Fatalf("PQGetIDs() err=%v", err)
		}
		inflight, err := exch.InFlightGetAll(ctx)
		if err != nil {
			t.Fatalf("InFlightGetAll() err=%v", err)
		}

		if queued["job-atomic"] {
			t.Error("claimed job must not remain in the queue")
		}
		entry, ok := inflight["job-atomic"]
		if !ok {
			t.Fatal("claimed job must have an in-flight entry the moment the claim returns")
		}
		if entry.ProcessorID != "proc-A" {
			t.Errorf("in-flight owner=%q, want proc-A", entry.ProcessorID)
		}
	})

	t.Run("JobNeverInvisibleUnderConcurrentClaims", func(t *testing.T) {
		// While claimers race to drain the queue, an observer must find every
		// job in at least one of the two stores. Queue is read first: a job
		// that left the queue before the read is already claimed, and claims
		// are never deleted here, so the in-flight read that follows sees it.
		exch := newExchangeClient(t)
		ctx := context.Background()

		const nJobs = 60
		const nClaimers = 8

		allIDs := make(map[string]bool, nJobs)
		for i := 0; i < nJobs; i++ {
			id := fmt.Sprintf("job-%03d", i)
			allIDs[id] = true
			if err := exch.PQEnqueue(ctx, &db_api.BatchJobPriority{
				ID:  id,
				SLO: time.Now().Add(time.Duration(i+1) * time.Minute),
			}); err != nil {
				t.Fatalf("PQEnqueue(%s) err=%v", id, err)
			}
		}

		done := make(chan struct{})
		var observerErr error
		var observerWG sync.WaitGroup
		observerWG.Add(1)
		go func() {
			defer observerWG.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				queued, err := exch.PQGetIDs(ctx)
				if err != nil {
					observerErr = fmt.Errorf("observer PQGetIDs: %w", err)
					return
				}
				inflight, err := exch.InFlightGetAll(ctx)
				if err != nil {
					observerErr = fmt.Errorf("observer InFlightGetAll: %w", err)
					return
				}
				for id := range allIDs {
					if !queued[id] {
						if _, claimed := inflight[id]; !claimed {
							observerErr = fmt.Errorf("job %s invisible: not in queue and not in-flight", id)
							return
						}
					}
				}
			}
		}()

		type claim struct {
			jobID       string
			processorID string
		}
		claimsCh := make(chan claim, nJobs)
		var claimerWG sync.WaitGroup
		for c := 0; c < nClaimers; c++ {
			processorID := fmt.Sprintf("proc-%d", c)
			claimerWG.Add(1)
			go func() {
				defer claimerWG.Done()
				for {
					task, err := exch.PQDequeueAndClaim(ctx, processorID)
					if err != nil {
						t.Errorf("PQDequeueAndClaim(%s) err=%v", processorID, err)
						return
					}
					if task == nil {
						return // queue drained
					}
					claimsCh <- claim{jobID: task.ID, processorID: processorID}
				}
			}()
		}
		claimerWG.Wait()
		close(done)
		observerWG.Wait()
		close(claimsCh)

		if observerErr != nil {
			t.Fatalf("invariant violated: %v", observerErr)
		}

		// Exactly-once: every job claimed by exactly one claimer.
		claimedBy := make(map[string]string, nJobs)
		for cl := range claimsCh {
			if prev, dup := claimedBy[cl.jobID]; dup {
				t.Fatalf("job %s claimed twice: by %s and %s", cl.jobID, prev, cl.processorID)
			}
			claimedBy[cl.jobID] = cl.processorID
		}
		if len(claimedBy) != nJobs {
			t.Fatalf("claimed %d jobs, want %d", len(claimedBy), nJobs)
		}

		// Ownership: every in-flight entry names the claimer that took the job.
		inflight, err := exch.InFlightGetAll(ctx)
		if err != nil {
			t.Fatalf("InFlightGetAll() err=%v", err)
		}
		for id, owner := range claimedBy {
			entry, ok := inflight[id]
			if !ok {
				t.Fatalf("claimed job %s missing from in-flight hash", id)
			}
			if entry.ProcessorID != owner {
				t.Fatalf("job %s in-flight owner=%q, want claimer %q", id, entry.ProcessorID, owner)
			}
		}
	})
}
