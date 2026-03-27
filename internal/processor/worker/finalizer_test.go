package worker

import (
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	mockdb "github.com/llm-d-incubation/batch-gateway/internal/database/mock"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/converter"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	"github.com/llm-d-incubation/batch-gateway/internal/util/clientset"
)

// --- resolveOutputExpiration ---

func TestResolveOutputExpiration_UserTagOverridesConfig(t *testing.T) {
	cfg := config.NewConfig()
	cfg.DefaultOutputExpirationSeconds = 7776000 // 90 days
	p := mustNewProcessor(t, cfg, validProcessorClients())

	now := int64(1000000)
	tags := db.Tags{batch_types.TagOutputExpiresAfterSeconds: "3600"}

	got := p.resolveOutputExpiration(now, tags)
	want := now + 3600
	if got != want {
		t.Fatalf("resolveOutputExpiration = %d, want %d (user tag should override config)", got, want)
	}
}

func TestResolveOutputExpiration_FallsBackToConfig(t *testing.T) {
	cfg := config.NewConfig()
	cfg.DefaultOutputExpirationSeconds = 86400
	p := mustNewProcessor(t, cfg, validProcessorClients())

	now := int64(1000000)
	tags := db.Tags{}

	got := p.resolveOutputExpiration(now, tags)
	want := now + 86400
	if got != want {
		t.Fatalf("resolveOutputExpiration = %d, want %d (should fall back to config)", got, want)
	}
}

func TestResolveOutputExpiration_ZeroWhenNeitherSet(t *testing.T) {
	cfg := config.NewConfig()
	cfg.DefaultOutputExpirationSeconds = 0
	p := mustNewProcessor(t, cfg, validProcessorClients())

	now := int64(1000000)
	tags := db.Tags{}

	got := p.resolveOutputExpiration(now, tags)
	if got != 0 {
		t.Fatalf("resolveOutputExpiration = %d, want 0 (no expiration)", got)
	}
}

func TestResolveOutputExpiration_InvalidTagFallsBackToConfig(t *testing.T) {
	cfg := config.NewConfig()
	cfg.DefaultOutputExpirationSeconds = 86400
	p := mustNewProcessor(t, cfg, validProcessorClients())

	now := int64(1000000)
	tags := db.Tags{batch_types.TagOutputExpiresAfterSeconds: "not-a-number"}

	got := p.resolveOutputExpiration(now, tags)
	want := now + 86400
	if got != want {
		t.Fatalf("resolveOutputExpiration = %d, want %d (invalid tag should fall back to config)", got, want)
	}
}

func TestResolveOutputExpiration_ZeroTagFallsBackToConfig(t *testing.T) {
	cfg := config.NewConfig()
	cfg.DefaultOutputExpirationSeconds = 86400
	p := mustNewProcessor(t, cfg, validProcessorClients())

	now := int64(1000000)
	tags := db.Tags{batch_types.TagOutputExpiresAfterSeconds: "0"}

	got := p.resolveOutputExpiration(now, tags)
	want := now + 86400
	if got != want {
		t.Fatalf("resolveOutputExpiration = %d, want %d (zero tag should fall back to config)", got, want)
	}
}

func TestResolveOutputExpiration_NilTags(t *testing.T) {
	cfg := config.NewConfig()
	cfg.DefaultOutputExpirationSeconds = 86400
	p := mustNewProcessor(t, cfg, validProcessorClients())

	now := int64(1000000)

	got := p.resolveOutputExpiration(now, nil)
	want := now + 86400
	if got != want {
		t.Fatalf("resolveOutputExpiration = %d, want %d (nil tags should fall back to config)", got, want)
	}
}

// --- executionProgress ---

func TestExecutionProgress_RecordAndCounts(t *testing.T) {
	updater := NewStatusUpdater(newMockBatchDBClient(), mockdb.NewMockBatchStatusClient(), 86400)
	ep := &executionProgress{
		total:   10,
		updater: updater,
		jobID:   "job-1",
	}

	ctx := testLoggerCtx(t)
	ep.record(ctx, true)
	ep.record(ctx, true)
	ep.record(ctx, false)

	counts := ep.counts()
	if counts.Total != 10 {
		t.Fatalf("Total = %d, want 10", counts.Total)
	}
	if counts.Completed != 2 {
		t.Fatalf("Completed = %d, want 2", counts.Completed)
	}
	if counts.Failed != 1 {
		t.Fatalf("Failed = %d, want 1", counts.Failed)
	}
}

// --- uploadFileAndStoreFileRecord ---

func TestUploadFileAndStoreFileRecord_StorageKeyAndDBFilename(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DefaultOutputExpirationSeconds = 86400

	mock := &failNTimesFilesClient{failCount: 0}
	fileDB := newMockFileDBClient()
	batchDB := newMockBatchDBClient()

	clients := &clientset.Clientset{
		File:    mock,
		FileDB:  fileDB,
		BatchDB: batchDB,
	}
	p := mustNewProcessor(t, cfg, clients)

	jobID := "job-storage-key"
	tenantID := "tenant-1"
	jobInfo := setupJobWithOutputFile(t, cfg, jobID, tenantID)
	ctx := testLoggerCtx(t)
	dbJob := seedDBJob(t, batchDB, jobID)

	fileID, err := p.uploadFileAndStoreFileRecord(ctx, jobInfo, dbJob, metrics.FileTypeOutput)
	if err != nil {
		t.Fatalf("uploadFileAndStoreFileRecord returned error: %v", err)
	}
	if fileID == "" {
		t.Fatal("expected non-empty fileID")
	}

	// Verify storage key uses FileStorageName format: <fileID>.jsonl
	wantStorageKey := fileID + ".jsonl"
	if mock.lastFileName != wantStorageKey {
		t.Errorf("storage key = %q, want %q", mock.lastFileName, wantStorageKey)
	}

	// Verify DB filename preserves the batch output format
	items, _, _, err := fileDB.DBGet(ctx, &db.FileQuery{BaseQuery: db.BaseQuery{IDs: []string{fileID}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	fileObj, err := converter.DBItemToFile(items[0])
	if err != nil {
		t.Fatalf("DBItemToFile: %v", err)
	}
	wantDBFilename := "batch_output_" + jobID + ".jsonl"
	if fileObj.Filename != wantDBFilename {
		t.Errorf("DB filename = %q, want %q", fileObj.Filename, wantDBFilename)
	}
}

// --- storeFileRecord ---

func TestStoreOutputFileRecord_Success(t *testing.T) {
	cfg := config.NewConfig()
	cfg.DefaultOutputExpirationSeconds = 86400
	fileDB := newMockFileDBClient()
	p := mustNewProcessor(t, cfg, &clientset.Clientset{
		FileDB: fileDB,
	})

	ctx := testLoggerCtx(t)
	tags := db.Tags{batch_types.TagOutputExpiresAfterSeconds: "3600"}

	err := p.storeFileRecord(ctx, "file_abc", "output.jsonl", "tenant-1", 1024, tags)
	if err != nil {
		t.Fatalf("storeFileRecord returned error: %v", err)
	}

	items, _, _, err := fileDB.DBGet(ctx, &db.FileQuery{BaseQuery: db.BaseQuery{IDs: []string{"file_abc"}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}

	if items[0].ID != "file_abc" {
		t.Fatalf("stored file ID = %q, want %q", items[0].ID, "file_abc")
	}
	if items[0].Purpose != string(openai.FileObjectPurposeBatchOutput) {
		t.Fatalf("stored purpose = %q, want %q", items[0].Purpose, openai.FileObjectPurposeBatchOutput)
	}
}

func TestFinalizeJob_CancelRequested_FinalizesCancelled(t *testing.T) {
	ctx := testLoggerCtx(t)
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	dbClient := newSpyBatchDB(newMockBatchDBClient())
	statusClient := mockdb.NewMockBatchStatusClient()

	jobID := "job-late-cancel"
	tenantID := "tenant-1"

	// Simulate a late cancel that was already persisted by the API server,
	// while the worker still has a stale in-memory in_progress job item.
	dbJob := seedDBJob(t, dbClient, jobID)
	// We need to set TenantID and update so DBGet can find it by TenantID
	dbJob.TenantID = tenantID
	dbJob.Status = mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusCancelling})
	// DBStore again to ensure it's indexed correctly with TenantID in the mock DB
	if err := dbClient.DBStore(ctx, dbJob); err != nil {
		t.Fatalf("DBStore: %v", err)
	}

	// Double check it's actually there
	jobsCheck, _, _, errCheck := dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}, TenantID: tenantID}}, true, 0, 1)
	if errCheck != nil || len(jobsCheck) == 0 {
		t.Fatalf("Failed to seed job for test: %v", errCheck)
	}

	clients := &clientset.Clientset{
		BatchDB: dbClient,
		FileDB:  newMockFileDBClient(),
		File:    &failNTimesFilesClient{failCount: 0},
		Status:  statusClient,
		Queue:   mockdb.NewMockBatchPriorityQueueClient(),
	}
	p := mustNewProcessor(t, cfg, clients)
	p.poller = NewPoller(clients.Queue, dbClient)

	updater := NewStatusUpdater(dbClient, statusClient, 86400)

	// Setup job dir manually to reuse the pre-seeded DB client
	jobDir, _ := p.jobRootDir(jobID, tenantID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	outputPath, _ := p.jobOutputFilePath(jobID, tenantID)
	if err := os.WriteFile(outputPath, []byte("test output\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	jobInfo := &batch_types.JobInfo{JobID: jobID, TenantID: tenantID}
	counts := &openai.BatchRequestCounts{Total: 1, Completed: 1, Failed: 0}

	// The worker's in-memory job state is stale and still says in_progress.
	staleJob := &db.BatchItem{
		BaseIndexes: db.BaseIndexes{ID: jobID, TenantID: tenantID},
		BaseContents: db.BaseContents{
			Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress}),
		},
	}

	// Simulate that the worker observed the cancel request before writing the final status.
	var cancelRequested atomic.Bool
	cancelRequested.Store(true)

	err := p.finalizeJob(ctx, updater, staleJob, jobInfo, counts, &cancelRequested)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected ErrCancelled, got: %v", err)
	}

	// Verify that the final status written to the DB is cancelled, not completed.
	jobsFinal, _, _, err := dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, true, 0, 1)
	if err != nil || len(jobsFinal) == 0 {
		t.Fatalf("DBGet failed: err=%v len=%d", err, len(jobsFinal))
	}

	var finalStatus openai.BatchStatusInfo
	if err := json.Unmarshal(jobsFinal[0].Status, &finalStatus); err != nil {
		t.Fatalf("Unmarshal status: %v", err)
	}

	if finalStatus.Status != openai.BatchStatusCancelled {
		t.Fatalf("expected final status to be %s, got %s", openai.BatchStatusCancelled, finalStatus.Status)
	}
}
