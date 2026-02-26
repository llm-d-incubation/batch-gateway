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
)

type errEventClient struct {
	db.BatchEventChannelClient
	err error
}

func (c *errEventClient) ECConsumerGetChannel(ctx context.Context, ID string) (*db.BatchEventsChan, error) {
	return nil, c.err
}

func TestRunJob_EventWatcherError_ReturnsSafely(t *testing.T) {
	cfg := config.NewConfig()
	cfg.NumWorkers = 1
	p := NewProcessor(cfg, &ProcessorClients{
		event: &errEventClient{err: errors.New("event unavailable")},
	})

	if !p.acquire(context.Background()) {
		t.Fatalf("expected token acquire before runJob")
	}
	p.wg.Add(1)

	p.runJob(
		testLoggerCtx(),
		NewStatusUpdater(newMockBatchDBClient(), mockdb.NewMockBatchStatusClient()),
		&db.BatchItem{BaseIndexes: db.BaseIndexes{ID: "job-1", TenantID: "tenantA"}},
		&batch_types.JobInfo{JobID: "job-1"},
		nil,
	)
}

func TestRunJob_PreProcessError_HandlesFailedStatus(t *testing.T) {
	ctx := testLoggerCtx()

	cfg := config.NewConfig()
	cfg.NumWorkers = 1
	cfg.WorkDir = t.TempDir()
	dbClient := newMockBatchDBClient()
	statusClient := mockdb.NewMockBatchStatusClient()
	eventClient := mockdb.NewMockBatchEventChannelClient()
	p := NewProcessor(cfg, &ProcessorClients{
		database: dbClient,
		status:   statusClient,
		event:    eventClient,
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
	p.runJob(ctx, NewStatusUpdater(dbClient, statusClient), jobItem, jobInfo, &db.BatchJobPriority{
		ID:  "job-fail",
		SLO: time.Now().Add(1 * time.Hour),
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
	updater := NewStatusUpdater(dbClient, mockdb.NewMockBatchStatusClient())

	p := NewProcessor(config.NewConfig(), &ProcessorClients{})
	err := p.handleFailed(testLoggerCtx(), &db.BatchItem{
		BaseIndexes: db.BaseIndexes{ID: "job-1", TenantID: "tenantA"},
		BaseContents: db.BaseContents{
			Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress}),
		},
	}, updater)
	if !errors.Is(err, updateErr) {
		t.Fatalf("expected update error, got %v", err)
	}
}
