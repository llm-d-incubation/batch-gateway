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

package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	db "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	"github.com/llm-d/llm-d-batch-gateway/internal/database/mock"
	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d/llm-d-batch-gateway/internal/shared/types"
)

const testInterval = 60 * time.Minute

func sloTag(slo time.Time) db.Tags {
	return db.Tags{batch_types.TagSLO: fmt.Sprintf("%d", slo.UnixMicro())}
}

func futureSLO() time.Time {
	return time.Now().Add(24 * time.Hour)
}

func expiredSLO() time.Time {
	return time.Now().Add(-1 * time.Hour)
}

func newTestBatchItem(id string, status openai.BatchStatus, tags db.Tags) *db.BatchItem {
	statusBytes, _ := json.Marshal(openai.BatchStatusInfo{Status: status})
	return &db.BatchItem{
		BaseIndexes: db.BaseIndexes{
			ID:   id,
			Tags: tags,
		},
		BaseContents: db.BaseContents{
			Status: statusBytes,
		},
	}
}

func newTestReconciler(
	t *testing.T,
	batchDB db.BatchDBClient,
	queue db.BatchPriorityQueueClient,
	inflight db.InFlightClient,
) (*Reconciler, chan *Result) {
	t.Helper()
	resultCh := make(chan *Result, 1)
	r, err := NewReconciler(batchDB, queue, inflight, testInterval, false, func(res *Result) {
		resultCh <- res
	})
	if err != nil {
		t.Fatalf("failed to create reconciler: %v", err)
	}
	return r, resultCh
}

func storeItems(t *testing.T, batchDB db.BatchDBClient, items ...*db.BatchItem) {
	t.Helper()
	ctx := context.Background()
	for _, item := range items {
		if err := batchDB.DBStore(ctx, item); err != nil {
			t.Fatalf("failed to store item %s: %v", item.ID, err)
		}
	}
}

func TestTriageOrphan(t *testing.T) {
	ctx := context.Background()

	t.Run("cancelling transitions to cancelled", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := newTestBatchItem("job-1", openai.BatchStatusCancelling, sloTag(futureSLO()))
		storeItems(t, batchDB, item)

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.Cancelled != 1 {
			t.Errorf("expected 1 cancelled, got %d", result.Cancelled)
		}
		assertJobStatus(t, batchDB, "job-1", openai.BatchStatusCancelled)
	})

	t.Run("validating with expired SLO transitions to expired", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := newTestBatchItem("job-1", openai.BatchStatusValidating, sloTag(expiredSLO()))
		storeItems(t, batchDB, item)

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.Expired != 1 {
			t.Errorf("expected 1 expired, got %d", result.Expired)
		}
		assertJobStatus(t, batchDB, "job-1", openai.BatchStatusExpired)
	})

	t.Run("validating with future SLO is re-enqueued", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := newTestBatchItem("job-1", openai.BatchStatusValidating, sloTag(futureSLO()))
		storeItems(t, batchDB, item)

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.ReEnqueued != 1 {
			t.Errorf("expected 1 re-enqueued, got %d", result.ReEnqueued)
		}

		queuedIDs, _ := queue.PQGetIDs(ctx)
		if !queuedIDs["job-1"] {
			t.Error("expected job-1 to be in queue after re-enqueue")
		}
	})

	t.Run("in_progress with expired SLO transitions to expired", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := newTestBatchItem("job-1", openai.BatchStatusInProgress, sloTag(expiredSLO()))
		storeItems(t, batchDB, item)

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.Expired != 1 {
			t.Errorf("expected 1 expired, got %d", result.Expired)
		}
		assertJobStatus(t, batchDB, "job-1", openai.BatchStatusExpired)
	})

	t.Run("in_progress with future SLO transitions to failed", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := newTestBatchItem("job-1", openai.BatchStatusInProgress, sloTag(futureSLO()))
		storeItems(t, batchDB, item)

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.Failed != 1 {
			t.Errorf("expected 1 failed, got %d", result.Failed)
		}
		assertJobStatus(t, batchDB, "job-1", openai.BatchStatusFailed)
	})

	t.Run("finalizing with expired SLO transitions to expired", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := newTestBatchItem("job-1", openai.BatchStatusFinalizing, sloTag(expiredSLO()))
		storeItems(t, batchDB, item)

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.Expired != 1 {
			t.Errorf("expected 1 expired, got %d", result.Expired)
		}
	})

	t.Run("finalizing with future SLO transitions to failed", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := newTestBatchItem("job-1", openai.BatchStatusFinalizing, sloTag(futureSLO()))
		storeItems(t, batchDB, item)

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.Failed != 1 {
			t.Errorf("expected 1 failed, got %d", result.Failed)
		}
	})
}

func TestSkipNonOrphans(t *testing.T) {
	ctx := context.Background()

	t.Run("job in queue is not treated as orphan", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		slo := futureSLO()
		item := newTestBatchItem("job-1", openai.BatchStatusValidating, sloTag(slo))
		storeItems(t, batchDB, item)

		_ = queue.PQEnqueue(ctx, &db.BatchJobPriority{ID: "job-1", SLO: slo})

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.ReEnqueued != 0 || result.Failed != 0 || result.Expired != 0 || result.Cancelled != 0 {
			t.Errorf("expected no actions for queued job, got %+v", result)
		}
	})

	t.Run("job with fresh in-flight entry is not treated as orphan", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := newTestBatchItem("job-1", openai.BatchStatusInProgress, sloTag(futureSLO()))
		storeItems(t, batchDB, item)

		_ = inflight.InFlightSet(ctx, "job-1", "processor-1")

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.Failed != 0 {
			t.Errorf("expected no failures for fresh in-flight job, got %d", result.Failed)
		}
	})
}

func TestStaleInflightCleanup(t *testing.T) {
	ctx := context.Background()

	t.Run("removes in-flight entry for job not in non-terminal set", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		// Stale in-flight entry for a job that no longer appears in the
		// non-terminal query (e.g. it already reached a terminal state
		// or was deleted, but its in-flight entry was not cleaned up).
		_ = inflight.InFlightSet(ctx, "job-stale", "processor-1")

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.StaleCleanup != 1 {
			t.Errorf("expected 1 stale cleanup, got %d", result.StaleCleanup)
		}

		entries, _ := inflight.InFlightGetAll(ctx)
		if _, ok := entries["job-stale"]; ok {
			t.Error("expected stale in-flight entry to be removed")
		}
	})

	t.Run("preserves in-flight entry for non-terminal job", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := newTestBatchItem("job-active", openai.BatchStatusInProgress, sloTag(futureSLO()))
		storeItems(t, batchDB, item)

		// Fresh in-flight entry — should NOT be cleaned up (it's non-terminal
		// and recently seen, so it's treated as actively processing).
		_ = inflight.InFlightSet(ctx, "job-active", "processor-1")

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.StaleCleanup != 0 {
			t.Errorf("expected 0 stale cleanup, got %d", result.StaleCleanup)
		}

		entries, _ := inflight.InFlightGetAll(ctx)
		if _, ok := entries["job-active"]; !ok {
			t.Error("expected in-flight entry to be preserved for non-terminal job")
		}
	})
}

func TestCASConflict(t *testing.T) {
	ctx := context.Background()

	t.Run("CAS conflict is counted as conflict not error", func(t *testing.T) {
		batchDB := &casConflictBatchDB{}
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.Conflicts != 1 {
			t.Errorf("expected 1 conflict from CAS, got %d", result.Conflicts)
		}
		if result.Errors != 0 {
			t.Errorf("expected 0 errors (CAS is a conflict, not an error), got %d", result.Errors)
		}
		if result.Cancelled != 0 {
			t.Errorf("expected 0 cancelled (CAS failed), got %d", result.Cancelled)
		}
	})
}

func TestRunLoop(t *testing.T) {
	t.Run("stops on context cancel", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		ran := make(chan struct{}, 1)
		r, err := NewReconciler(batchDB, queue, inflight, testInterval, false, func(*Result) {
			select {
			case ran <- struct{}{}:
			default:
			}
		})
		if err != nil {
			t.Fatalf("failed to create reconciler: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan error, 1)
		go func() { done <- r.RunLoop(ctx) }()

		<-ran
		cancel()

		if err := <-done; err != nil && err != context.Canceled {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestTriageEdgeCases(t *testing.T) {
	ctx := context.Background()

	t.Run("validating orphan without SLO tag errors", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := newTestBatchItem("job-1", openai.BatchStatusValidating, db.Tags{})
		storeItems(t, batchDB, item)

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.Errors != 1 {
			t.Errorf("expected 1 error for missing SLO, got %d", result.Errors)
		}
		if result.ReEnqueued != 0 {
			t.Errorf("expected 0 re-enqueued, got %d", result.ReEnqueued)
		}
	})

	t.Run("validating orphan with corrupt SLO tag errors", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := newTestBatchItem("job-1", openai.BatchStatusValidating, db.Tags{batch_types.TagSLO: "not-a-number"})
		storeItems(t, batchDB, item)

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.Errors != 1 {
			t.Errorf("expected 1 error for corrupt SLO, got %d", result.Errors)
		}
		if result.ReEnqueued != 0 {
			t.Errorf("expected 0 re-enqueued, got %d", result.ReEnqueued)
		}
	})

	t.Run("malformed status JSON errors", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := &db.BatchItem{
			BaseIndexes:  db.BaseIndexes{ID: "job-1", Tags: sloTag(futureSLO())},
			BaseContents: db.BaseContents{Status: []byte(`{{invalid json`)},
		}
		storeItems(t, batchDB, item)

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.Errors != 1 {
			t.Errorf("expected 1 error for malformed status, got %d", result.Errors)
		}
	})

	t.Run("stale in-flight entry triggers triage", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := newTestBatchItem("job-1", openai.BatchStatusInProgress, sloTag(futureSLO()))
		storeItems(t, batchDB, item)

		_ = inflight.InFlightSet(ctx, "job-1", "processor-1")
		// Backdate the LastSeen to make it stale (older than the reconciler interval).
		staleTime := time.Now().Add(-2 * testInterval).Unix()
		inflight.SetLastSeen("job-1", staleTime)

		r, resultCh := newTestReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.Failed != 1 {
			t.Errorf("expected 1 failed for stale in-flight job, got %d", result.Failed)
		}
		assertJobStatus(t, batchDB, "job-1", openai.BatchStatusFailed)
	})
}

func TestNewReconcilerValidation(t *testing.T) {
	batchDB := newMockBatchDB()
	queue := mock.NewMockBatchPriorityQueueClient()
	inflight := mock.NewMockInFlightClient()

	t.Run("nil batchDB", func(t *testing.T) {
		_, err := NewReconciler(nil, queue, inflight, testInterval, false, nil)
		if err == nil {
			t.Fatal("expected error for nil batchDB")
		}
	})

	t.Run("nil queue", func(t *testing.T) {
		_, err := NewReconciler(batchDB, nil, inflight, testInterval, false, nil)
		if err == nil {
			t.Fatal("expected error for nil queue")
		}
	})

	t.Run("nil inflight", func(t *testing.T) {
		_, err := NewReconciler(batchDB, queue, nil, testInterval, false, nil)
		if err == nil {
			t.Fatal("expected error for nil inflight")
		}
	})

	t.Run("zero interval", func(t *testing.T) {
		_, err := NewReconciler(batchDB, queue, inflight, 0, false, nil)
		if err == nil {
			t.Fatal("expected error for zero interval")
		}
	})

	t.Run("negative interval", func(t *testing.T) {
		_, err := NewReconciler(batchDB, queue, inflight, -time.Minute, false, nil)
		if err == nil {
			t.Fatal("expected error for negative interval")
		}
	})
}

func newTestDryRunReconciler(
	t *testing.T,
	batchDB db.BatchDBClient,
	queue db.BatchPriorityQueueClient,
	inflight db.InFlightClient,
) (*Reconciler, chan *Result) {
	t.Helper()
	resultCh := make(chan *Result, 1)
	r, err := NewReconciler(batchDB, queue, inflight, testInterval, true, func(res *Result) {
		resultCh <- res
	})
	if err != nil {
		t.Fatalf("failed to create dry-run reconciler: %v", err)
	}
	return r, resultCh
}

func TestDryRun(t *testing.T) {
	ctx := context.Background()

	t.Run("transition is counted but DB is not mutated", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := newTestBatchItem("job-1", openai.BatchStatusCancelling, sloTag(futureSLO()))
		storeItems(t, batchDB, item)

		r, resultCh := newTestDryRunReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.Cancelled != 1 {
			t.Errorf("expected 1 cancelled, got %d", result.Cancelled)
		}
		assertJobStatus(t, batchDB, "job-1", openai.BatchStatusCancelling)
	})

	t.Run("re-enqueue is counted but queue is not mutated", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		item := newTestBatchItem("job-1", openai.BatchStatusValidating, sloTag(futureSLO()))
		storeItems(t, batchDB, item)

		r, resultCh := newTestDryRunReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.ReEnqueued != 1 {
			t.Errorf("expected 1 re-enqueued, got %d", result.ReEnqueued)
		}

		queuedIDs, _ := queue.PQGetIDs(ctx)
		if queuedIDs["job-1"] {
			t.Error("expected job-1 NOT to be in queue in dry-run mode")
		}
	})

	t.Run("stale cleanup is counted but in-flight entry is preserved", func(t *testing.T) {
		batchDB := newMockBatchDB()
		queue := mock.NewMockBatchPriorityQueueClient()
		inflight := mock.NewMockInFlightClient()

		_ = inflight.InFlightSet(ctx, "job-stale", "processor-1")

		r, resultCh := newTestDryRunReconciler(t, batchDB, queue, inflight)
		r.run(ctx)

		result := <-resultCh
		if result.StaleCleanup != 1 {
			t.Errorf("expected 1 stale cleanup, got %d", result.StaleCleanup)
		}

		entries, _ := inflight.InFlightGetAll(ctx)
		if _, ok := entries["job-stale"]; !ok {
			t.Error("expected stale in-flight entry to be preserved in dry-run mode")
		}
	})
}

func TestTerminalStatusesSync(t *testing.T) {
	// Verify that every status returned by TerminalStatuses() is actually final,
	// and every final status is included in TerminalStatuses().
	terminalSet := make(map[openai.BatchStatus]bool)
	for _, s := range openai.TerminalStatuses() {
		if !s.IsTerminal() {
			t.Errorf("TerminalStatuses() contains %q which is not IsTerminal()", s)
		}
		terminalSet[s] = true
	}

	allStatuses := []openai.BatchStatus{
		openai.BatchStatusValidating,
		openai.BatchStatusFailed,
		openai.BatchStatusInProgress,
		openai.BatchStatusFinalizing,
		openai.BatchStatusCompleted,
		openai.BatchStatusExpired,
		openai.BatchStatusCancelling,
		openai.BatchStatusCancelled,
	}
	for _, s := range allStatuses {
		if s.IsTerminal() && !terminalSet[s] {
			t.Errorf("status %q is IsTerminal() but missing from TerminalStatuses()", s)
		}
	}
}

// --- Helpers ---

func assertJobStatus(t *testing.T, batchDB db.BatchDBClient, jobID string, expected openai.BatchStatus) {
	t.Helper()
	items, _, _, err := batchDB.DBGet(context.Background(),
		&db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, false, 0, 10)
	if err != nil {
		t.Fatalf("failed to get job %s: %v", jobID, err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item for %s, got %d", jobID, len(items))
	}
	var info openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &info); err != nil {
		t.Fatalf("failed to unmarshal status: %v", err)
	}
	if info.Status != expected {
		t.Errorf("expected status %s for job %s, got %s", expected, jobID, info.Status)
	}
}

// newMockBatchDB creates a mock batch DB that always returns all items for NonTerminal queries.
func newMockBatchDB() *mock.MockDBClient[db.BatchItem, db.BatchQuery] {
	return mock.NewMockDBClient[db.BatchItem, db.BatchQuery](
		func(item *db.BatchItem) string { return item.ID },
		func(query *db.BatchQuery) *db.BaseQuery { return &query.BaseQuery },
	)
}

// casConflictBatchDB is a minimal mock that always returns ErrConflict on DBUpdate.
type casConflictBatchDB struct{}

func (c *casConflictBatchDB) DBStore(_ context.Context, _ *db.BatchItem) error { return nil }
func (c *casConflictBatchDB) DBGet(_ context.Context, _ *db.BatchQuery, _ bool, _, _ int) ([]*db.BatchItem, int, bool, error) {
	item := newTestBatchItem("job-cas", openai.BatchStatusCancelling, sloTag(futureSLO()))
	return []*db.BatchItem{item}, 1, false, nil
}
func (c *casConflictBatchDB) DBUpdate(_ context.Context, _ *db.BatchItem, _ []byte) error {
	return fmt.Errorf("DBUpdate: %w", db.ErrConflict)
}
func (c *casConflictBatchDB) DBDelete(_ context.Context, _ []string) ([]string, error) {
	return nil, nil
}
func (c *casConflictBatchDB) Close() error { return nil }
func (c *casConflictBatchDB) GetContext(_ context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
	return context.Background(), func() {}
}
