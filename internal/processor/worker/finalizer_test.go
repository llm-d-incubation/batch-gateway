package worker

import (
	"context"
	"testing"
	"time"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	mockdb "github.com/llm-d-incubation/batch-gateway/internal/database/mock"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
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

	ctx := testLoggerCtx()
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

// --- storeFileRecord ---

func TestStoreOutputFileRecord_Success(t *testing.T) {
	cfg := config.NewConfig()
	cfg.DefaultOutputExpirationSeconds = 86400
	fileDB := newMockFileDBClient()
	p := mustNewProcessor(t, cfg, &clientset.Clientset{
		FileDB: fileDB,
	})

	ctx := testLoggerCtx()
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

// --- uploadOutputFile retry ---

func TestUploadOutputFile_RetriesAndSucceeds(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.UploadRetry = config.RetryConfig{
		MaxRetries:     3,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     10 * time.Millisecond,
	}

	mock := &failNTimesFilesClient{failCount: 2}
	p := mustNewProcessor(t, cfg, &clientset.Clientset{File: mock})

	jobInfo := setupJobWithOutputFile(t, cfg, "job-retry", "tenant-1")
	ctx := testLoggerCtx()

	size, err := p.uploadOutputFile(ctx, jobInfo, "output.jsonl")
	if err != nil {
		t.Fatalf("uploadOutputFile returned error: %v", err)
	}
	if size != 42 {
		t.Fatalf("size = %d, want 42", size)
	}
	if mock.calls != 3 {
		t.Fatalf("expected 3 Store calls (1 initial + 2 retries), got %d", mock.calls)
	}
}

func TestUploadOutputFile_ExhaustsRetries(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.UploadRetry = config.RetryConfig{
		MaxRetries:     2,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     10 * time.Millisecond,
	}

	mock := &failNTimesFilesClient{failCount: 100}
	p := mustNewProcessor(t, cfg, &clientset.Clientset{File: mock})

	jobInfo := setupJobWithOutputFile(t, cfg, "job-exhaust", "tenant-1")
	ctx := testLoggerCtx()

	_, err := p.uploadOutputFile(ctx, jobInfo, "output.jsonl")
	if err == nil {
		t.Fatalf("expected error after exhausting retries")
	}
	if mock.calls != 3 {
		t.Fatalf("expected 3 Store calls (1 initial + 2 retries), got %d", mock.calls)
	}
}

func TestUploadOutputFile_ContextCancelledDuringRetry(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.UploadRetry = config.RetryConfig{
		MaxRetries:     5,
		InitialBackoff: 1 * time.Hour,
		MaxBackoff:     1 * time.Hour,
	}

	mock := &failNTimesFilesClient{failCount: 100}
	p := mustNewProcessor(t, cfg, &clientset.Clientset{File: mock})

	jobInfo := setupJobWithOutputFile(t, cfg, "job-cancel", "tenant-1")
	ctx, cancel := context.WithCancel(testLoggerCtx())
	cancel()

	_, err := p.uploadOutputFile(ctx, jobInfo, "output.jsonl")
	if err == nil {
		t.Fatalf("expected error on cancelled context")
	}
}
