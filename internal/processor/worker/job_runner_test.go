package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	mockdb "github.com/llm-d-incubation/batch-gateway/internal/database/mock"
	mockfiles "github.com/llm-d-incubation/batch-gateway/internal/files_store/mock"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	"github.com/llm-d-incubation/batch-gateway/internal/util/clientset"
)

type errEventClient struct {
	db.BatchEventChannelClient
	err error
}

func (c *errEventClient) ECConsumerGetChannel(ctx context.Context, ID string) (*db.BatchEventsChan, error) {
	return nil, c.err
}

// errPQClient is a BatchPriorityQueueClient whose PQEnqueue always returns err.
type errPQClient struct {
	db.BatchPriorityQueueClient
	err error
}

func (c *errPQClient) PQEnqueue(ctx context.Context, _ *db.BatchJobPriority) error { return c.err }

func TestRunJob_EventWatcherError_ReturnsSafely(t *testing.T) {
	cfg := config.NewConfig()
	cfg.NumWorkers = 1
	p := mustNewProcessor(t, cfg, &clientset.Clientset{
		Event: &errEventClient{err: errors.New("event unavailable")},
	})

	if !p.acquire(context.Background()) {
		t.Fatalf("expected token acquire before runJob")
	}
	p.wg.Add(1)

	p.runJob(testLoggerCtx(t), &jobExecutionParams{
		updater: NewStatusUpdater(newMockBatchDBClient(), mockdb.NewMockBatchStatusClient(), 86400),
		jobItem: &db.BatchItem{BaseIndexes: db.BaseIndexes{ID: "job-1", TenantID: "tenantA"}},
		jobInfo: &batch_types.JobInfo{JobID: "job-1"},
	})
}

func TestRunJob_EventWatcherAndReEnqueueBothFail_MarksJobFailed(t *testing.T) {
	ctx := testLoggerCtx(t)

	dbClient := newMockBatchDBClient()
	statusClient := mockdb.NewMockBatchStatusClient()
	pqClient := &errPQClient{err: errors.New("queue unavailable")}

	cfg := config.NewConfig()
	cfg.NumWorkers = 1
	p := mustNewProcessor(t, cfg, &clientset.Clientset{
		BatchDB: dbClient,
		Status:  statusClient,
		Queue:   pqClient,
		Event:   &errEventClient{err: errors.New("event unavailable")},
	})

	jobItem := &db.BatchItem{
		BaseIndexes: db.BaseIndexes{ID: "job-stuck", TenantID: "tenantA"},
		BaseContents: db.BaseContents{
			Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusValidating}),
		},
	}
	if err := dbClient.DBStore(ctx, jobItem); err != nil {
		t.Fatalf("DBStore: %v", err)
	}

	if !p.acquire(context.Background()) {
		t.Fatalf("expected token acquire before runJob")
	}
	p.wg.Add(1)

	p.runJob(ctx, &jobExecutionParams{
		updater: NewStatusUpdater(dbClient, statusClient, 86400),
		jobItem: jobItem,
		jobInfo: &batch_types.JobInfo{JobID: "job-stuck"},
		task:    &db.BatchJobPriority{ID: "job-stuck"},
	})

	items, _, _, err := dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{"job-stuck"}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}

	var updated openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &updated); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if updated.Status != openai.BatchStatusFailed {
		t.Fatalf("expected failed status, got %s", updated.Status)
	}
}

func TestRunJob_PreProcessError_HandlesFailedStatus(t *testing.T) {
	ctx := testLoggerCtx(t)

	cfg := config.NewConfig()
	cfg.NumWorkers = 1
	cfg.WorkDir = t.TempDir()
	dbClient := newMockBatchDBClient()
	statusClient := mockdb.NewMockBatchStatusClient()
	eventClient := mockdb.NewMockBatchEventChannelClient()
	p := mustNewProcessor(t, cfg, &clientset.Clientset{
		BatchDB: dbClient,
		Status:  statusClient,
		Event:   eventClient,
	})

	jobItem := &db.BatchItem{
		BaseIndexes: db.BaseIndexes{ID: "job-fail", TenantID: "tenantA"},
		BaseContents: db.BaseContents{
			Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress}),
		},
	}
	if err := dbClient.DBStore(ctx, jobItem); err != nil {
		t.Fatalf("DBStore job item: %v", err)
	}

	// Empty InputFileID forces preProcessJob to fail and runJob to handle as failed.
	jobInfo := &batch_types.JobInfo{
		JobID: "job-fail",
		BatchJob: &openai.Batch{
			ID: "job-fail",
			BatchSpec: openai.BatchSpec{
				InputFileID: "",
			},
			BatchStatusInfo: openai.BatchStatusInfo{Status: openai.BatchStatusInProgress},
		},
		TenantID: "tenantA",
	}

	if !p.acquire(context.Background()) {
		t.Fatalf("expected token acquire before runJob")
	}
	p.wg.Add(1)
	p.runJob(ctx, &jobExecutionParams{
		updater: NewStatusUpdater(dbClient, statusClient, 86400),
		jobItem: jobItem,
		jobInfo: jobInfo,
		task: &db.BatchJobPriority{
			ID:  "job-fail",
			SLO: time.Now().Add(1 * time.Hour),
		},
	})

	items, _, _, err := dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{"job-fail"}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet updated item: err=%v len=%d", err, len(items))
	}

	var updated openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &updated); err != nil {
		t.Fatalf("unmarshal updated status: %v", err)
	}
	if updated.Status != openai.BatchStatusFailed {
		t.Fatalf("expected failed status, got %s", updated.Status)
	}
}

// TestRunJob_WithCancelRequested_ReachesPreProcess verifies that runJob with a properly
// initialized cancelRequested field proceeds past the event watcher setup and into
// preProcessJob without panicking.
func TestRunJob_WithCancelRequested_ReachesPreProcess(t *testing.T) {
	cfg := config.NewConfig()
	cfg.NumWorkers = 1
	cfg.WorkDir = t.TempDir()

	dbClient := newMockBatchDBClient()
	statusClient := mockdb.NewMockBatchStatusClient()
	eventClient := mockdb.NewMockBatchEventChannelClient()
	p := mustNewProcessor(t, cfg, &clientset.Clientset{
		BatchDB: dbClient,
		FileDB:  newMockFileDBClient(),
		Status:  statusClient,
		Event:   eventClient,
		File:    mockfiles.NewMockBatchFilesClient(),
	})

	ctx := testLoggerCtx(t)

	jobItem := &db.BatchItem{
		BaseIndexes: db.BaseIndexes{ID: "job-contract", TenantID: "tenantA"},
		BaseContents: db.BaseContents{
			Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress}),
		},
	}
	if err := dbClient.DBStore(ctx, jobItem); err != nil {
		t.Fatalf("DBStore: %v", err)
	}

	// InputFileID is set so preProcessJob proceeds past the empty-check and reaches
	// the cancelRequested.Load() call. Without cancelRequested initialized, this panics.
	jobInfo := &batch_types.JobInfo{
		JobID: "job-contract",
		BatchJob: &openai.Batch{
			ID: "job-contract",
			BatchSpec: openai.BatchSpec{
				InputFileID: "file-123",
			},
			BatchStatusInfo: openai.BatchStatusInfo{Status: openai.BatchStatusInProgress},
		},
		TenantID: "tenantA",
	}

	if !p.acquire(context.Background()) {
		t.Fatalf("expected token acquire before runJob")
	}
	p.wg.Add(1)

	var cancelRequested atomic.Bool
	p.runJob(ctx, &jobExecutionParams{
		updater:         NewStatusUpdater(dbClient, statusClient, 86400),
		jobItem:         jobItem,
		jobInfo:         jobInfo,
		cancelRequested: &cancelRequested,
		task: &db.BatchJobPriority{
			ID:  "job-contract",
			SLO: time.Now().Add(1 * time.Hour),
		},
	})

	// preProcessJob will fail (file doesn't exist on disk) and handleJobError marks it failed.
	// The key assertion: we reached handleFailed (not a silent panic recovery).
	items, _, _, err := dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{"job-contract"}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}

	var updated openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &updated); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if updated.Status != openai.BatchStatusFailed {
		t.Fatalf("expected failed status (preprocess error handled), got %s", updated.Status)
	}
}

func TestHandleFailed_DBUpdateError_ReturnsError(t *testing.T) {
	updateErr := errors.New("db update failed")
	dbClient := &dbUpdateErrWrapper{
		inner: newMockBatchDBClient(),
		err:   updateErr,
	}
	updater := NewStatusUpdater(dbClient, mockdb.NewMockBatchStatusClient(), 86400)

	p := mustNewProcessor(t, config.NewConfig(), &clientset.Clientset{})
	err := p.handleFailed(testLoggerCtx(t), updater, &db.BatchItem{
		BaseIndexes: db.BaseIndexes{ID: "job-1", TenantID: "tenantA"},
		BaseContents: db.BaseContents{
			Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress}),
		},
	}, nil, nil)
	if !errors.Is(err, updateErr) {
		t.Fatalf("expected update error, got %v", err)
	}
}

// --- handlePanicRecovery tests ---

func TestHandlePanicRecovery_BeforeInProgress_MarksFailed(t *testing.T) {
	ctx := testLoggerCtx(t)
	dbClient := newMockBatchDBClient()
	statusClient := mockdb.NewMockBatchStatusClient()

	jobItem := &db.BatchItem{
		BaseIndexes:  db.BaseIndexes{ID: "job-panic-pre", TenantID: "tenantA"},
		BaseContents: db.BaseContents{Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusValidating})},
	}
	if err := dbClient.DBStore(ctx, jobItem); err != nil {
		t.Fatalf("DBStore: %v", err)
	}

	p := mustNewProcessor(t, config.NewConfig(), &clientset.Clientset{BatchDB: dbClient, Status: statusClient})
	p.handlePanicRecovery(ctx, &jobExecutionParams{
		updater: NewStatusUpdater(dbClient, statusClient, 86400),
		jobItem: jobItem,
		jobInfo: &batch_types.JobInfo{JobID: "job-panic-pre"},
	}, false, nil)

	assertJobStatus(t, dbClient, "job-panic-pre", openai.BatchStatusFailed)
}

func TestHandlePanicRecovery_AfterInProgress_WithCounts_MarksFailed(t *testing.T) {
	ctx := testLoggerCtx(t)
	dbClient := newMockBatchDBClient()
	statusClient := mockdb.NewMockBatchStatusClient()

	jobItem := &db.BatchItem{
		BaseIndexes:  db.BaseIndexes{ID: "job-panic-partial", TenantID: "tenantA"},
		BaseContents: db.BaseContents{Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress})},
	}
	if err := dbClient.DBStore(ctx, jobItem); err != nil {
		t.Fatalf("DBStore: %v", err)
	}

	counts := &openai.BatchRequestCounts{Total: 10, Completed: 3, Failed: 0}
	p := mustNewProcessor(t, config.NewConfig(), &clientset.Clientset{BatchDB: dbClient, Status: statusClient})
	p.handlePanicRecovery(ctx, &jobExecutionParams{
		updater: NewStatusUpdater(dbClient, statusClient, 86400),
		jobItem: jobItem,
		jobInfo: &batch_types.JobInfo{JobID: "job-panic-partial"},
	}, true, counts)

	assertJobStatus(t, dbClient, "job-panic-partial", openai.BatchStatusFailed)
}

func TestHandlePanicRecovery_AfterInProgress_NilCounts_MarksFailed(t *testing.T) {
	ctx := testLoggerCtx(t)
	dbClient := newMockBatchDBClient()
	statusClient := mockdb.NewMockBatchStatusClient()

	jobItem := &db.BatchItem{
		BaseIndexes:  db.BaseIndexes{ID: "job-panic-nocounts", TenantID: "tenantA"},
		BaseContents: db.BaseContents{Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress})},
	}
	if err := dbClient.DBStore(ctx, jobItem); err != nil {
		t.Fatalf("DBStore: %v", err)
	}

	p := mustNewProcessor(t, config.NewConfig(), &clientset.Clientset{BatchDB: dbClient, Status: statusClient})
	p.handlePanicRecovery(ctx, &jobExecutionParams{
		updater: NewStatusUpdater(dbClient, statusClient, 86400),
		jobItem: jobItem,
		jobInfo: &batch_types.JobInfo{JobID: "job-panic-nocounts"},
	}, true, nil)

	assertJobStatus(t, dbClient, "job-panic-nocounts", openai.BatchStatusFailed)
}

func TestHandlePanicRecovery_CancelledContext_StillMarksFailed(t *testing.T) {
	ctx, cancel := context.WithCancel(testLoggerCtx(t))
	cancel()

	dbClient := newMockBatchDBClient()
	statusClient := mockdb.NewMockBatchStatusClient()

	jobItem := &db.BatchItem{
		BaseIndexes:  db.BaseIndexes{ID: "job-panic-cancelled-ctx", TenantID: "tenantA"},
		BaseContents: db.BaseContents{Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress})},
	}
	if err := dbClient.DBStore(context.Background(), jobItem); err != nil {
		t.Fatalf("DBStore: %v", err)
	}

	p := mustNewProcessor(t, config.NewConfig(), &clientset.Clientset{BatchDB: dbClient, Status: statusClient})
	p.handlePanicRecovery(ctx, &jobExecutionParams{
		updater: NewStatusUpdater(dbClient, statusClient, 86400),
		jobItem: jobItem,
		jobInfo: &batch_types.JobInfo{JobID: "job-panic-cancelled-ctx"},
	}, true, nil)

	assertJobStatus(t, dbClient, "job-panic-cancelled-ctx", openai.BatchStatusFailed)
}

func TestHandlePanicRecovery_PartialFails_FallbackSucceeds(t *testing.T) {
	ctx := testLoggerCtx(t)
	dbClient := &dbUpdateFailOnceWrapper{inner: newMockBatchDBClient(), failCount: 1}
	statusClient := mockdb.NewMockBatchStatusClient()

	jobItem := &db.BatchItem{
		BaseIndexes:  db.BaseIndexes{ID: "job-panic-fallback", TenantID: "tenantA"},
		BaseContents: db.BaseContents{Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress})},
	}
	if err := dbClient.DBStore(ctx, jobItem); err != nil {
		t.Fatalf("DBStore: %v", err)
	}

	counts := &openai.BatchRequestCounts{Total: 10, Completed: 3, Failed: 0}
	p := mustNewProcessor(t, config.NewConfig(), &clientset.Clientset{BatchDB: dbClient, Status: statusClient})
	p.handlePanicRecovery(ctx, &jobExecutionParams{
		updater: NewStatusUpdater(dbClient, statusClient, 86400),
		jobItem: jobItem,
		jobInfo: &batch_types.JobInfo{JobID: "job-panic-fallback"},
	}, true, counts)

	assertJobStatus(t, dbClient, "job-panic-fallback", openai.BatchStatusFailed)
}

func TestHandlePanicRecovery_NilParams_DoesNotPanic(t *testing.T) {
	ctx := testLoggerCtx(t)
	p := mustNewProcessor(t, config.NewConfig(), &clientset.Clientset{})
	p.handlePanicRecovery(ctx, nil, false, nil)
	p.handlePanicRecovery(ctx, &jobExecutionParams{}, false, nil)
}

// dbBlockingUpdateWrapper blocks DBUpdate until its context is cancelled,
// simulating an unreachable database.
type dbBlockingUpdateWrapper struct {
	inner db.BatchDBClient
}

func (d *dbBlockingUpdateWrapper) DBStore(ctx context.Context, item *db.BatchItem) error {
	return d.inner.DBStore(ctx, item)
}
func (d *dbBlockingUpdateWrapper) DBGet(ctx context.Context, query *db.BatchQuery, includeStatic bool, start, limit int) ([]*db.BatchItem, int, bool, error) {
	return d.inner.DBGet(ctx, query, includeStatic, start, limit)
}
func (d *dbBlockingUpdateWrapper) DBUpdate(ctx context.Context, _ *db.BatchItem) error {
	<-ctx.Done()
	return ctx.Err()
}
func (d *dbBlockingUpdateWrapper) DBDelete(ctx context.Context, IDs []string) ([]string, error) {
	return d.inner.DBDelete(ctx, IDs)
}
func (d *dbBlockingUpdateWrapper) GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parentCtx, timeLimit)
}
func (d *dbBlockingUpdateWrapper) Close() error {
	return d.inner.Close()
}

// TestHandlePanicRecovery_BlockingDB_ReturnsWithinTimeout verifies that
// handlePanicRecovery returns within a bounded time even when the DB is
// unreachable (blocks forever), ensuring wg.Done() and release() are not
// starved.
func TestHandlePanicRecovery_BlockingDB_ReturnsWithinTimeout(t *testing.T) {
	// Use a short timeout so the test completes quickly in CI.
	orig := panicRecoveryTimeout
	panicRecoveryTimeout = 500 * time.Millisecond
	t.Cleanup(func() { panicRecoveryTimeout = orig })

	ctx := testLoggerCtx(t)
	blockingDB := &dbBlockingUpdateWrapper{inner: newMockBatchDBClient()}
	statusClient := mockdb.NewMockBatchStatusClient()

	jobItem := &db.BatchItem{
		BaseIndexes:  db.BaseIndexes{ID: "job-panic-block", TenantID: "tenantA"},
		BaseContents: db.BaseContents{Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress})},
	}
	if err := blockingDB.DBStore(ctx, jobItem); err != nil {
		t.Fatalf("DBStore: %v", err)
	}

	p := mustNewProcessor(t, config.NewConfig(), &clientset.Clientset{BatchDB: blockingDB, Status: statusClient})

	done := make(chan struct{})
	go func() {
		p.handlePanicRecovery(ctx, &jobExecutionParams{
			updater: NewStatusUpdater(blockingDB, statusClient, 86400),
			jobItem: jobItem,
			jobInfo: &batch_types.JobInfo{JobID: "job-panic-block"},
		}, false, nil)
		close(done)
	}()

	select {
	case <-done:
		// handlePanicRecovery returned — the timeout worked.
	case <-time.After(5 * time.Second):
		t.Fatal("handlePanicRecovery blocked beyond panicRecoveryTimeout; timeout not applied")
	}
}

func assertJobStatus(t *testing.T, dbClient db.BatchDBClient, jobID string, want openai.BatchStatus) {
	t.Helper()
	items, _, _, err := dbClient.DBGet(context.Background(), &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet %s: err=%v len=%d", jobID, err, len(items))
	}
	var status openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &status); err != nil {
		t.Fatalf("unmarshal status for %s: %v", jobID, err)
	}
	if status.Status != want {
		t.Fatalf("job %s: expected status %s, got %s", jobID, want, status.Status)
	}
}
