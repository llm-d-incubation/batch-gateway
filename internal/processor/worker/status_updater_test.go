package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	mockdb "github.com/llm-d-incubation/batch-gateway/internal/database/mock"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
)

type errStatusClient struct {
	db.BatchStatusClient
	err error
}

func (c *errStatusClient) StatusSet(ctx context.Context, ID string, TTL int, data []byte) error {
	return c.err
}

type dbUpdateErrWrapper struct {
	inner db.BatchDBClient
	err   error
}

func (d *dbUpdateErrWrapper) DBStore(ctx context.Context, item *db.BatchItem) error {
	return d.inner.DBStore(ctx, item)
}
func (d *dbUpdateErrWrapper) DBGet(ctx context.Context, query *db.BatchQuery, includeStatic bool, start, limit int) ([]*db.BatchItem, int, bool, error) {
	return d.inner.DBGet(ctx, query, includeStatic, start, limit)
}
func (d *dbUpdateErrWrapper) DBUpdate(ctx context.Context, item *db.BatchItem) error {
	return d.err
}
func (d *dbUpdateErrWrapper) DBDelete(ctx context.Context, IDs []string) ([]string, error) {
	return d.inner.DBDelete(ctx, IDs)
}
func (d *dbUpdateErrWrapper) GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parentCtx, timeLimit)
}
func (d *dbUpdateErrWrapper) Close() error {
	return d.inner.Close()
}

// dbUpdateFailOnceWrapper fails the first N DBUpdate calls, then delegates to inner.
type dbUpdateFailOnceWrapper struct {
	inner     db.BatchDBClient
	failCount int
	calls     int
}

func (d *dbUpdateFailOnceWrapper) DBStore(ctx context.Context, item *db.BatchItem) error {
	return d.inner.DBStore(ctx, item)
}
func (d *dbUpdateFailOnceWrapper) DBGet(ctx context.Context, query *db.BatchQuery, includeStatic bool, start, limit int) ([]*db.BatchItem, int, bool, error) {
	return d.inner.DBGet(ctx, query, includeStatic, start, limit)
}
func (d *dbUpdateFailOnceWrapper) DBUpdate(ctx context.Context, item *db.BatchItem) error {
	d.calls++
	if d.calls <= d.failCount {
		return fmt.Errorf("simulated DB update failure (call %d)", d.calls)
	}
	return d.inner.DBUpdate(ctx, item)
}
func (d *dbUpdateFailOnceWrapper) DBDelete(ctx context.Context, IDs []string) ([]string, error) {
	return d.inner.DBDelete(ctx, IDs)
}
func (d *dbUpdateFailOnceWrapper) GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parentCtx, timeLimit)
}
func (d *dbUpdateFailOnceWrapper) Close() error {
	return d.inner.Close()
}

func TestUpdateProgressCounts_NilCounts_ReturnsError(t *testing.T) {
	updater := NewStatusUpdater(newMockBatchDBClient(), mockdb.NewMockBatchStatusClient(), 86400)

	if err := updater.UpdateProgressCounts(context.Background(), "job-1", nil); err == nil {
		t.Fatalf("expected error for nil requestCounts")
	}
}

func TestUpdateProgressCounts_StatusSetError_ReturnsError(t *testing.T) {
	statusErr := errors.New("status set failed")
	updater := NewStatusUpdater(newMockBatchDBClient(), &errStatusClient{err: statusErr}, 86400)

	err := updater.UpdateProgressCounts(context.Background(), "job-1", &openai.BatchRequestCounts{Total: 1})
	if !errors.Is(err, statusErr) {
		t.Fatalf("expected status client error, got %v", err)
	}
}

func TestUpdateProgressCounts_Success_WritesPayload(t *testing.T) {
	statusClient := mockdb.NewMockBatchStatusClient()
	updater := NewStatusUpdater(newMockBatchDBClient(), statusClient, 86400)

	if err := updater.UpdateProgressCounts(context.Background(), "job-1", &openai.BatchRequestCounts{
		Total: 10, Completed: 7, Failed: 3,
	}); err != nil {
		t.Fatalf("UpdateProgressCounts: %v", err)
	}

	data, err := statusClient.StatusGet(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("StatusGet: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("expected payload written to status client")
	}
}

func TestUpdatePersistentStatus_InputValidationErrors(t *testing.T) {
	updater := NewStatusUpdater(newMockBatchDBClient(), mockdb.NewMockBatchStatusClient(), 86400)

	if err := updater.UpdatePersistentStatus(context.Background(), nil, openai.BatchStatusFailed, nil, nil); err == nil {
		t.Fatalf("expected error for nil dbJob")
	}

	err := updater.UpdatePersistentStatus(context.Background(), &db.BatchItem{
		BaseIndexes: db.BaseIndexes{ID: "job-1"},
	}, openai.BatchStatusFailed, nil, nil)
	if err == nil {
		t.Fatalf("expected error for empty dbJob.Status")
	}
}

func TestUpdatePersistentStatus_UnmarshalError(t *testing.T) {
	updater := NewStatusUpdater(newMockBatchDBClient(), mockdb.NewMockBatchStatusClient(), 86400)

	err := updater.UpdatePersistentStatus(context.Background(), &db.BatchItem{
		BaseIndexes: db.BaseIndexes{ID: "job-1"},
		BaseContents: db.BaseContents{
			Status: []byte("{invalid"),
		},
	}, openai.BatchStatusFailed, nil, nil)
	if err == nil {
		t.Fatalf("expected unmarshal error")
	}
}

func TestUpdatePersistentStatus_DBUpdateError(t *testing.T) {
	updateErr := errors.New("db update failed")
	dbClient := &dbUpdateErrWrapper{
		inner: newMockBatchDBClient(),
		err:   updateErr,
	}
	updater := NewStatusUpdater(dbClient, mockdb.NewMockBatchStatusClient(), 86400)

	err := updater.UpdatePersistentStatus(context.Background(), &db.BatchItem{
		BaseIndexes: db.BaseIndexes{ID: "job-1"},
		BaseContents: db.BaseContents{
			Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress}),
		},
	}, openai.BatchStatusFailed, nil, nil)
	if !errors.Is(err, updateErr) {
		t.Fatalf("expected db update error, got %v", err)
	}
}

func TestUpdatePersistentStatus_Success(t *testing.T) {
	ctx := context.Background()
	dbClient := newMockBatchDBClient()
	updater := NewStatusUpdater(dbClient, mockdb.NewMockBatchStatusClient(), 86400)
	jobID := "job-update-success"

	seed := &db.BatchItem{
		BaseIndexes: db.BaseIndexes{ID: jobID},
		BaseContents: db.BaseContents{
			Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress}),
		},
	}
	if err := dbClient.DBStore(ctx, seed); err != nil {
		t.Fatalf("DBStore seed: %v", err)
	}

	if err := updater.UpdatePersistentStatus(ctx, seed, openai.BatchStatusFailed, nil, nil); err != nil {
		t.Fatalf("UpdatePersistentStatus: %v", err)
	}

	items, _, _, err := dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet updated item: err=%v len=%d", err, len(items))
	}

	var got openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &got); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if got.Status != openai.BatchStatusFailed {
		t.Fatalf("expected status failed, got %s", got.Status)
	}
}
