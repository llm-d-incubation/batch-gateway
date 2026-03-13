package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	mockdb "github.com/llm-d-incubation/batch-gateway/internal/database/mock"
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

	p.runJob(testLoggerCtx(), &jobExecutionParams{
		updater: NewStatusUpdater(newMockBatchDBClient(), mockdb.NewMockBatchStatusClient(), 86400),
		jobItem: &db.BatchItem{BaseIndexes: db.BaseIndexes{ID: "job-1", TenantID: "tenantA"}},
		jobInfo: &batch_types.JobInfo{JobID: "job-1"},
	})
}

func TestRunJob_EventWatcherAndReEnqueueBothFail_MarksJobFailed(t *testing.T) {
	ctx := testLoggerCtx()

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
	ctx := testLoggerCtx()

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

func TestHandleFailed_DBUpdateError_ReturnsError(t *testing.T) {
	updateErr := errors.New("db update failed")
	dbClient := &dbUpdateErrWrapper{
		inner: newMockBatchDBClient(),
		err:   updateErr,
	}
	updater := NewStatusUpdater(dbClient, mockdb.NewMockBatchStatusClient(), 86400)

	p := mustNewProcessor(t, config.NewConfig(), &clientset.Clientset{})
	err := p.handleFailed(testLoggerCtx(), updater, &db.BatchItem{
		BaseIndexes: db.BaseIndexes{ID: "job-1", TenantID: "tenantA"},
		BaseContents: db.BaseContents{
			Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress}),
		},
	}, nil)
	if !errors.Is(err, updateErr) {
		t.Fatalf("expected update error, got %v", err)
	}
}
