package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	mockdb "github.com/llm-d-incubation/batch-gateway/internal/database/mock"
	mockfiles "github.com/llm-d-incubation/batch-gateway/internal/files_store/mock"
	"github.com/llm-d-incubation/batch-gateway/internal/inference"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	"github.com/llm-d-incubation/batch-gateway/internal/util/clientset"
)

// ---------------------------------------------------------------------------
// Configurable inference mock
// ---------------------------------------------------------------------------

type mockInferenceClient struct {
	generateFn func(ctx context.Context, req *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError)
}

func (m *mockInferenceClient) Generate(ctx context.Context, req *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
	if m.generateFn != nil {
		return m.generateFn(ctx, req)
	}
	return &inference.GenerateResponse{
		RequestID: "server-req-1",
		Response:  []byte(`{"choices":[{"message":{"content":"hello"}}]}`),
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers: write binary plan file and model map for Phase 2
// ---------------------------------------------------------------------------

func writePlanFile(t *testing.T, dir, safeModelID string, entries []planEntry) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll plans dir: %v", err)
	}
	path := filepath.Join(dir, safeModelID+".plan")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create plan file: %v", err)
	}
	defer f.Close()
	for _, e := range entries {
		buf := e.marshalBinary()
		if _, err := f.Write(buf[:]); err != nil {
			t.Fatalf("write plan entry: %v", err)
		}
	}
}

func writeModelMap(t *testing.T, jobRootDir string, mm modelMapFile) {
	t.Helper()
	data, err := json.Marshal(mm)
	if err != nil {
		t.Fatalf("marshal model map: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jobRootDir, modelMapFileName), data, 0o644); err != nil {
		t.Fatalf("write model map: %v", err)
	}
}

// writeInputJSONL writes request lines and returns the bytes (including trailing newlines)
// so the caller can compute plan entry offsets.
func writeInputJSONL(t *testing.T, path string, requests []batch_types.Request) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, r := range requests {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write input file: %v", err)
	}
	return buf.Bytes()
}

// planEntriesFromLines computes plan entries from the raw input bytes (one entry per line).
func planEntriesFromLines(raw []byte) []planEntry {
	var entries []planEntry
	offset := int64(0)
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		length := uint32(len(line) + 1) // include trailing '\n'
		entries = append(entries, planEntry{Offset: offset, Length: length})
		offset += int64(length)
	}
	return entries
}

// testProcessorEnv holds the processor and its mock clients for test inspection.
type testProcessorEnv struct {
	p        *Processor
	dbClient db.BatchDBClient
	pqClient db.BatchPriorityQueueClient
	updater  *StatusUpdater
}

// newTestProcessorEnv creates a Processor wired with mock clients.
// The returned env exposes the shared dbClient and pqClient for seeding and verification.
func newTestProcessorEnv(t *testing.T, cfg *config.ProcessorConfig, inferClient inference.Client) *testProcessorEnv {
	t.Helper()

	dbClient := newMockBatchDBClient()
	pqClient := mockdb.NewMockBatchPriorityQueueClient()
	statusClient := mockdb.NewMockBatchStatusClient()

	p := NewProcessor(cfg, &clientset.Clientset{
		BatchDB:   dbClient,
		FileDB:    newMockFileDBClient(),
		File:      mockfiles.NewMockBatchFilesClient(),
		Queue:     pqClient,
		Status:    statusClient,
		Event:     mockdb.NewMockBatchEventChannelClient(),
		Inference: inference.NewSingleClientResolver(inferClient),
	})
	p.poller = NewPoller(pqClient, dbClient)

	return &testProcessorEnv{
		p:        p,
		dbClient: dbClient,
		pqClient: pqClient,
		updater:  NewStatusUpdater(dbClient, statusClient, 86400),
	}
}

// setupPhase2Job creates a complete job directory with input file, plan files, and model map.
func setupPhase2Job(
	t *testing.T,
	cfg *config.ProcessorConfig,
	inferClient inference.Client,
	requests []batch_types.Request,
	modelToSafe map[string]string,
) (*testProcessorEnv, *batch_types.JobInfo) {
	t.Helper()

	env := newTestProcessorEnv(t, cfg, inferClient)

	jobID := "test-job"
	tenantID := "tenant-1"

	jobRootDir, err := env.p.jobRootDir(jobID, tenantID)
	if err != nil {
		t.Fatalf("jobRootDir: %v", err)
	}
	if err := os.MkdirAll(jobRootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	inputPath := filepath.Join(jobRootDir, "input.jsonl")
	rawInput := writeInputJSONL(t, inputPath, requests)

	allEntries := planEntriesFromLines(rawInput)

	safeToModel := make(map[string]string, len(modelToSafe))
	modelEntries := make(map[string][]planEntry)
	for model, safe := range modelToSafe {
		safeToModel[safe] = model
	}

	for i, req := range requests {
		safe := modelToSafe[req.Body["model"].(string)]
		modelEntries[safe] = append(modelEntries[safe], allEntries[i])
	}

	plansDir := filepath.Join(jobRootDir, "plans")
	for safe, entries := range modelEntries {
		writePlanFile(t, plansDir, safe, entries)
	}

	writeModelMap(t, jobRootDir, modelMapFile{
		ModelToSafe: modelToSafe,
		SafeToModel: safeToModel,
		LineCount:   int64(len(requests)),
	})

	jobInfo := &batch_types.JobInfo{
		JobID:    jobID,
		TenantID: tenantID,
	}

	return env, jobInfo
}

// seedDBJob stores a BatchItem in the DB so the updater can find and update it.
func seedDBJob(t *testing.T, dbClient db.BatchDBClient, jobID string) *db.BatchItem {
	t.Helper()
	statusInfo := openai.BatchStatusInfo{Status: openai.BatchStatusInProgress}
	statusBytes, _ := json.Marshal(statusInfo)
	item := &db.BatchItem{
		BaseIndexes:  db.BaseIndexes{ID: jobID, Tags: db.Tags{}},
		BaseContents: db.BaseContents{Status: statusBytes},
	}
	if err := dbClient.DBStore(context.Background(), item); err != nil {
		t.Fatalf("seed DB job: %v", err)
	}
	return item
}

// =====================================================================
// Tests: executeOneRequest
// =====================================================================

func TestExecuteOneRequest_Success(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, req *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			return &inference.GenerateResponse{
				RequestID: "srv-123",
				Response:  []byte(`{"result":"ok"}`),
			}, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "req-1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1", "prompt": "hi"}},
	}
	env, jobInfo := setupPhase2Job(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, err := os.Open(inputPath)
	if err != nil {
		t.Fatalf("open input: %v", err)
	}
	defer inputFile.Close()

	jobRootDir, _ := env.p.jobRootDir(jobInfo.JobID, jobInfo.TenantID)
	entries := planEntriesFromLines(mustReadFile(t, filepath.Join(jobRootDir, "input.jsonl")))

	ctx := testLoggerCtx()
	result, err := env.p.executeOneRequest(ctx, inputFile, entries[0], "m1", nil)
	if err != nil {
		t.Fatalf("executeOneRequest error: %v", err)
	}
	if result.CustomID != "req-1" {
		t.Fatalf("CustomID = %q, want %q", result.CustomID, "req-1")
	}
	if result.Error != nil {
		t.Fatalf("expected no error in output line, got %+v", result.Error)
	}
	if result.Response == nil {
		t.Fatalf("expected response in output line")
	}
	if result.Response.StatusCode != 200 {
		t.Fatalf("StatusCode = %d, want 200", result.Response.StatusCode)
	}
	if result.Response.RequestID != "srv-123" {
		t.Fatalf("RequestID = %q, want %q", result.Response.RequestID, "srv-123")
	}
}

func TestExecuteOneRequest_InferenceError(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			return nil, &inference.ClientError{
				Category: inference.ErrCategoryServer,
				Message:  "backend unavailable",
			}
		},
	}

	requests := []batch_types.Request{
		{CustomID: "req-err", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupPhase2Job(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, _ := os.Open(inputPath)
	defer inputFile.Close()

	jobRootDir, _ := env.p.jobRootDir(jobInfo.JobID, jobInfo.TenantID)
	entries := planEntriesFromLines(mustReadFile(t, filepath.Join(jobRootDir, "input.jsonl")))

	ctx := testLoggerCtx()
	result, err := env.p.executeOneRequest(ctx, inputFile, entries[0], "m1", nil)
	if err != nil {
		t.Fatalf("executeOneRequest should not return error for inference failure, got: %v", err)
	}
	if result.Error == nil {
		t.Fatalf("expected error field in output line")
	}
	if result.Error.Code != string(inference.ErrCategoryServer) {
		t.Fatalf("error code = %q, want %q", result.Error.Code, inference.ErrCategoryServer)
	}
	if result.Response != nil {
		t.Fatalf("expected nil response on inference error")
	}
}

func TestExecuteOneRequest_NilResponse(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			return nil, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "req-nil", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupPhase2Job(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, _ := os.Open(inputPath)
	defer inputFile.Close()

	jobRootDir, _ := env.p.jobRootDir(jobInfo.JobID, jobInfo.TenantID)
	entries := planEntriesFromLines(mustReadFile(t, filepath.Join(jobRootDir, "input.jsonl")))

	ctx := testLoggerCtx()
	result, err := env.p.executeOneRequest(ctx, inputFile, entries[0], "m1", nil)
	if err != nil {
		t.Fatalf("executeOneRequest should not return error, got: %v", err)
	}
	if result.Error == nil {
		t.Fatalf("expected error field for nil response")
	}
	if result.Error.Code != string(inference.ErrCategoryServer) {
		t.Fatalf("error code = %q, want %q", result.Error.Code, inference.ErrCategoryServer)
	}
}

func TestExecuteOneRequest_BadJSONResponse(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			return &inference.GenerateResponse{
				RequestID: "srv-bad",
				Response:  []byte(`{not valid json`),
			}, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "req-bad-json", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupPhase2Job(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, _ := os.Open(inputPath)
	defer inputFile.Close()

	jobRootDir, _ := env.p.jobRootDir(jobInfo.JobID, jobInfo.TenantID)
	entries := planEntriesFromLines(mustReadFile(t, filepath.Join(jobRootDir, "input.jsonl")))

	ctx := testLoggerCtx()
	result, err := env.p.executeOneRequest(ctx, inputFile, entries[0], "m1", nil)
	if err != nil {
		t.Fatalf("executeOneRequest should not return error, got: %v", err)
	}
	if result.Error == nil {
		t.Fatalf("expected error field for bad JSON response")
	}
	if result.Error.Code != string(inference.ErrCategoryParse) {
		t.Fatalf("error code = %q, want %q", result.Error.Code, inference.ErrCategoryParse)
	}
}

func TestExecuteOneRequest_BadOffset(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	requests := []batch_types.Request{
		{CustomID: "req-1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupPhase2Job(t, cfg, &mockInferenceClient{}, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, _ := os.Open(inputPath)
	defer inputFile.Close()

	badEntry := planEntry{Offset: 99999, Length: 10}
	ctx := testLoggerCtx()
	_, err := env.p.executeOneRequest(ctx, inputFile, badEntry, "m1", nil)
	if err == nil {
		t.Fatalf("expected error for bad offset")
	}
}

// =====================================================================
// Tests: processModel
// =====================================================================

func TestProcessModel_Success(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	var callCount atomic.Int32
	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			callCount.Add(1)
			return &inference.GenerateResponse{RequestID: "srv", Response: []byte(`{"ok":true}`)}, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "b", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "c", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupPhase2Job(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, _ := os.Open(inputPath)
	defer inputFile.Close()

	plansDir, _ := env.p.jobPlansDir(jobInfo.JobID, jobInfo.TenantID)

	var buf bytes.Buffer
	writer := bufio.NewWriter(&buf)
	cancelReq := &atomic.Bool{}

	progress := &executionProgress{
		total:   int64(len(requests)),
		updater: env.updater,
		jobID:   jobInfo.JobID,
	}

	var errBuf bytes.Buffer
	writers := &outputWriters{output: writer, errors: bufio.NewWriter(&errBuf)}

	ctx := testLoggerCtx()
	err := env.p.processModel(ctx, inputFile, plansDir, "m1", "m1", writers, cancelReq, progress, nil)
	if err != nil {
		t.Fatalf("processModel error: %v", err)
	}

	if err := writer.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if int(callCount.Load()) != len(requests) {
		t.Fatalf("inference calls = %d, want %d", callCount.Load(), len(requests))
	}

	counts := progress.counts()
	if counts.Completed != int64(len(requests)) {
		t.Fatalf("completed = %d, want %d", counts.Completed, len(requests))
	}

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte{'\n'})
	if len(lines) != len(requests) {
		t.Fatalf("output lines = %d, want %d", len(lines), len(requests))
	}
}

func TestProcessModel_CancelRequested(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupPhase2Job(t, cfg, &mockInferenceClient{}, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, _ := os.Open(inputPath)
	defer inputFile.Close()

	plansDir, _ := env.p.jobPlansDir(jobInfo.JobID, jobInfo.TenantID)

	var buf bytes.Buffer
	writer := bufio.NewWriter(&buf)
	cancelReq := &atomic.Bool{}
	cancelReq.Store(true)

	progress := &executionProgress{
		total:   1,
		updater: env.updater,
		jobID:   jobInfo.JobID,
	}

	var errBuf bytes.Buffer
	writers := &outputWriters{output: writer, errors: bufio.NewWriter(&errBuf)}

	ctx := testLoggerCtx()
	err := env.p.processModel(ctx, inputFile, plansDir, "m1", "m1", writers, cancelReq, progress, nil)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected ErrCancelled, got: %v", err)
	}
}

func TestProcessModel_InferenceFatalError(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			return &inference.GenerateResponse{RequestID: "srv", Response: []byte(`{"ok":true}`)}, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "b", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupPhase2Job(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, _ := os.Open(inputPath)
	inputFile.Close() // close early so ReadAt fails

	plansDir, _ := env.p.jobPlansDir(jobInfo.JobID, jobInfo.TenantID)

	var buf bytes.Buffer
	writer := bufio.NewWriter(&buf)
	cancelReq := &atomic.Bool{}

	progress := &executionProgress{
		total:   int64(len(requests)),
		updater: env.updater,
		jobID:   jobInfo.JobID,
	}

	var errBuf bytes.Buffer
	writers := &outputWriters{output: writer, errors: bufio.NewWriter(&errBuf)}

	ctx := testLoggerCtx()
	err := env.p.processModel(ctx, inputFile, plansDir, "m1", "m1", writers, cancelReq, progress, nil)
	if err == nil {
		t.Fatalf("expected error from closed input file")
	}
}

func TestProcessModel_ContextCancelledDuringDispatch(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.GlobalConcurrency = 1
	cfg.PerModelMaxConcurrency = 1

	started := make(chan struct{})
	block := make(chan struct{})
	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			close(started)
			<-block
			return &inference.GenerateResponse{RequestID: "srv", Response: []byte(`{"ok":true}`)}, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "b", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupPhase2Job(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, _ := os.Open(inputPath)
	defer inputFile.Close()

	plansDir, _ := env.p.jobPlansDir(jobInfo.JobID, jobInfo.TenantID)

	var buf bytes.Buffer
	writer := bufio.NewWriter(&buf)
	cancelReq := &atomic.Bool{}

	progress := &executionProgress{
		total:   int64(len(requests)),
		updater: env.updater,
		jobID:   jobInfo.JobID,
	}

	ctx, cancel := context.WithCancel(testLoggerCtx())

	var errBuf bytes.Buffer
	writers := &outputWriters{output: writer, errors: bufio.NewWriter(&errBuf)}

	done := make(chan error, 1)
	go func() {
		done <- env.p.processModel(ctx, inputFile, plansDir, "m1", "m1", writers, cancelReq, progress, nil)
	}()

	<-started
	cancel()
	close(block)

	err := <-done
	if err == nil {
		t.Fatalf("expected error on context cancellation")
	}
}

// =====================================================================
// Tests: executeJob
// =====================================================================

func TestExecuteJob_SingleModel(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	mock := &mockInferenceClient{}
	requests := []batch_types.Request{
		{CustomID: "r1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "r2", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupPhase2Job(t, cfg, mock, requests, map[string]string{"m1": "m1"})
	cancelReq := &atomic.Bool{}

	ctx := testLoggerCtx()
	counts, err := env.p.executeJob(ctx, env.updater, jobInfo, cancelReq)
	if err != nil {
		t.Fatalf("executeJob error: %v", err)
	}
	if counts.Total != 2 {
		t.Fatalf("Total = %d, want 2", counts.Total)
	}
	if counts.Completed+counts.Failed != 2 {
		t.Fatalf("Completed+Failed = %d, want 2", counts.Completed+counts.Failed)
	}

	outputPath, _ := env.p.jobOutputFilePath(jobInfo.JobID, jobInfo.TenantID)
	outBytes, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	outputLines := bytes.Split(bytes.TrimSpace(outBytes), []byte{'\n'})
	if len(outputLines) != 2 {
		t.Fatalf("output lines = %d, want 2", len(outputLines))
	}
}

func TestExecuteJob_MultipleModels(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	var callCount atomic.Int32
	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			callCount.Add(1)
			return &inference.GenerateResponse{RequestID: "srv", Response: []byte(`{"ok":true}`)}, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "b", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m2"}},
		{CustomID: "c", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "d", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m2"}},
	}
	env, jobInfo := setupPhase2Job(t, cfg, mock, requests, map[string]string{"m1": "m1", "m2": "m2"})
	cancelReq := &atomic.Bool{}

	ctx := testLoggerCtx()
	counts, err := env.p.executeJob(ctx, env.updater, jobInfo, cancelReq)
	if err != nil {
		t.Fatalf("executeJob error: %v", err)
	}
	if counts.Total != 4 {
		t.Fatalf("Total = %d, want 4", counts.Total)
	}
	if int(callCount.Load()) != 4 {
		t.Fatalf("inference calls = %d, want 4", callCount.Load())
	}
}

func TestExecuteJob_ContextCancelled(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	mock := &mockInferenceClient{
		generateFn: func(ctx context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			<-ctx.Done()
			return nil, &inference.ClientError{Category: inference.ErrCategoryServer, Message: "cancelled"}
		},
	}

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupPhase2Job(t, cfg, mock, requests, map[string]string{"m1": "m1"})
	cancelReq := &atomic.Bool{}

	ctx, cancel := context.WithCancel(testLoggerCtx())
	cancel()

	_, err := env.p.executeJob(ctx, env.updater, jobInfo, cancelReq)
	if err == nil {
		t.Fatalf("expected error on cancelled context")
	}
}

func TestExecuteJob_UserCancelFlag(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupPhase2Job(t, cfg, &mockInferenceClient{}, requests, map[string]string{"m1": "m1"})

	cancelReq := &atomic.Bool{}
	cancelReq.Store(true)

	ctx := testLoggerCtx()
	_, err := env.p.executeJob(ctx, env.updater, jobInfo, cancelReq)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected ErrCancelled, got: %v", err)
	}
}

// =====================================================================
// Tests: finalizeJob
// =====================================================================

func TestFinalizeJob_Success(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DefaultOutputExpirationSeconds = 86400
	cfg.UploadRetry = config.RetryConfig{
		MaxRetries:     1,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     1 * time.Millisecond,
	}

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	jobID := "finalize-job"
	tenantID := "tenant-1"
	jobInfo := &batch_types.JobInfo{JobID: jobID, TenantID: tenantID}

	jobDir, _ := env.p.jobRootDir(jobID, tenantID)
	os.MkdirAll(jobDir, 0o755)
	outputPath, _ := env.p.jobOutputFilePath(jobID, tenantID)
	os.WriteFile(outputPath, []byte(`{"id":"batch_req_1","custom_id":"r1","response":{"status_code":200}}`+"\n"), 0o644)

	dbJob := seedDBJob(t, env.dbClient, jobID)
	counts := &openai.BatchRequestCounts{Total: 1, Completed: 1, Failed: 0}

	ctx := testLoggerCtx()
	err := env.p.finalizeJob(ctx, env.updater, dbJob, jobInfo, counts)
	if err != nil {
		t.Fatalf("finalizeJob error: %v", err)
	}

	items, _, _, err := env.dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	var statusInfo openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &statusInfo); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if statusInfo.Status != openai.BatchStatusCompleted {
		t.Fatalf("status = %s, want %s", statusInfo.Status, openai.BatchStatusCompleted)
	}
	if statusInfo.OutputFileID == "" {
		t.Fatalf("expected OutputFileID to be set")
	}
}

func TestFinalizeJob_UploadFailure(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.UploadRetry = config.RetryConfig{
		MaxRetries:     0,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     1 * time.Millisecond,
	}

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})
	env.p.clients.File = &failNTimesFilesClient{failCount: 100}

	jobID := "finalize-fail"
	tenantID := "tenant-1"
	jobInfo := &batch_types.JobInfo{JobID: jobID, TenantID: tenantID}

	jobDir, _ := env.p.jobRootDir(jobID, tenantID)
	os.MkdirAll(jobDir, 0o755)
	outputPath, _ := env.p.jobOutputFilePath(jobID, tenantID)
	os.WriteFile(outputPath, []byte("output\n"), 0o644)

	dbJob := seedDBJob(t, env.dbClient, jobID)
	counts := &openai.BatchRequestCounts{Total: 1, Completed: 1}

	ctx := testLoggerCtx()
	err := env.p.finalizeJob(ctx, env.updater, dbJob, jobInfo, counts)
	if err == nil {
		t.Fatalf("expected error from upload failure")
	}
}

// =====================================================================
// Tests: error file separation
// =====================================================================

// TestExecuteJob_SeparatesSuccessAndErrors verifies that successful responses
// are written to output.jsonl and failed responses are written to error.jsonl.
func TestExecuteJob_SeparatesSuccessAndErrors(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	var callCount atomic.Int32
	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			if callCount.Add(1)%2 == 1 {
				return &inference.GenerateResponse{RequestID: "srv", Response: []byte(`{"ok":true}`)}, nil
			}
			return nil, &inference.ClientError{Category: inference.ErrCategoryServer, Message: "mock error"}
		},
	}

	requests := []batch_types.Request{
		{CustomID: "r1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "r2", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupPhase2Job(t, cfg, mock, requests, map[string]string{"m1": "m1"})
	cancelReq := &atomic.Bool{}

	ctx := testLoggerCtx()
	counts, err := env.p.executeJob(ctx, env.updater, jobInfo, cancelReq)
	if err != nil {
		t.Fatalf("executeJob error: %v", err)
	}
	if counts.Completed != 1 || counts.Failed != 1 {
		t.Fatalf("counts: completed=%d failed=%d, want completed=1 failed=1", counts.Completed, counts.Failed)
	}

	outputPath, _ := env.p.jobOutputFilePath(jobInfo.JobID, jobInfo.TenantID)
	outputLines := readNonEmptyJSONLLines(t, outputPath)
	if len(outputLines) != 1 {
		t.Fatalf("output.jsonl lines = %d, want 1", len(outputLines))
	}
	var outLine outputLine
	if err := json.Unmarshal(outputLines[0], &outLine); err != nil {
		t.Fatalf("unmarshal output line: %v", err)
	}
	if outLine.Response == nil || outLine.Error != nil {
		t.Fatalf("output line: want response set and error nil, got response=%v error=%v", outLine.Response, outLine.Error)
	}

	errorPath, _ := env.p.jobErrorFilePath(jobInfo.JobID, jobInfo.TenantID)
	errorLines := readNonEmptyJSONLLines(t, errorPath)
	if len(errorLines) != 1 {
		t.Fatalf("error.jsonl lines = %d, want 1", len(errorLines))
	}
	var errLine outputLine
	if err := json.Unmarshal(errorLines[0], &errLine); err != nil {
		t.Fatalf("unmarshal error line: %v", err)
	}
	if errLine.Error == nil || errLine.Response != nil {
		t.Fatalf("error line: want error set and response nil, got response=%v error=%v", errLine.Response, errLine.Error)
	}
}

// TestFinalizeJob_EmptyOutputFile_OutputFileIDOmitted verifies that when the output
// file is empty (all requests failed), output_file_id is omitted per the OpenAI spec.
func TestFinalizeJob_EmptyOutputFile_OutputFileIDOmitted(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DefaultOutputExpirationSeconds = 86400
	cfg.UploadRetry = config.RetryConfig{
		MaxRetries:     0,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     1 * time.Millisecond,
	}

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	jobID := "finalize-empty-output"
	tenantID := "tenant-1"
	jobInfo := &batch_types.JobInfo{JobID: jobID, TenantID: tenantID}

	jobDir, _ := env.p.jobRootDir(jobID, tenantID)
	os.MkdirAll(jobDir, 0o755)
	outputPath, _ := env.p.jobOutputFilePath(jobID, tenantID)
	os.WriteFile(outputPath, []byte{}, 0o644)
	errorPath, _ := env.p.jobErrorFilePath(jobID, tenantID)
	os.WriteFile(errorPath, []byte(`{"id":"batch_req_1","custom_id":"r1","error":{"code":"server_error","message":"fail"}}`+"\n"), 0o644)

	dbJob := seedDBJob(t, env.dbClient, jobID)
	counts := &openai.BatchRequestCounts{Total: 1, Completed: 0, Failed: 1}

	ctx := testLoggerCtx()
	if err := env.p.finalizeJob(ctx, env.updater, dbJob, jobInfo, counts); err != nil {
		t.Fatalf("finalizeJob error: %v", err)
	}

	items, _, _, err := env.dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	var statusInfo openai.BatchStatusInfo
	json.Unmarshal(items[0].Status, &statusInfo)
	if statusInfo.OutputFileID != "" {
		t.Errorf("OutputFileID = %q, want empty (output file was empty)", statusInfo.OutputFileID)
	}
	if statusInfo.ErrorFileID == "" {
		t.Errorf("ErrorFileID should be set when error file has content")
	}
}

// TestFinalizeJob_EmptyErrorFile_ErrorFileIDOmitted verifies that when the error
// file is empty (no requests failed), error_file_id is omitted per the OpenAI spec.
func TestFinalizeJob_EmptyErrorFile_ErrorFileIDOmitted(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DefaultOutputExpirationSeconds = 86400
	cfg.UploadRetry = config.RetryConfig{
		MaxRetries:     0,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     1 * time.Millisecond,
	}

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	jobID := "finalize-empty-error"
	tenantID := "tenant-1"
	jobInfo := &batch_types.JobInfo{JobID: jobID, TenantID: tenantID}

	jobDir, _ := env.p.jobRootDir(jobID, tenantID)
	os.MkdirAll(jobDir, 0o755)
	outputPath, _ := env.p.jobOutputFilePath(jobID, tenantID)
	os.WriteFile(outputPath, []byte(`{"id":"batch_req_1","custom_id":"r1","response":{"status_code":200}}`+"\n"), 0o644)
	errorPath, _ := env.p.jobErrorFilePath(jobID, tenantID)
	os.WriteFile(errorPath, []byte{}, 0o644)

	dbJob := seedDBJob(t, env.dbClient, jobID)
	counts := &openai.BatchRequestCounts{Total: 1, Completed: 1, Failed: 0}

	ctx := testLoggerCtx()
	if err := env.p.finalizeJob(ctx, env.updater, dbJob, jobInfo, counts); err != nil {
		t.Fatalf("finalizeJob error: %v", err)
	}

	items, _, _, err := env.dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	var statusInfo openai.BatchStatusInfo
	json.Unmarshal(items[0].Status, &statusInfo)
	if statusInfo.OutputFileID == "" {
		t.Errorf("OutputFileID should be set when output file has content")
	}
	if statusInfo.ErrorFileID != "" {
		t.Errorf("ErrorFileID = %q, want empty (error file was empty)", statusInfo.ErrorFileID)
	}
}

// =====================================================================
// Tests: handleJobError (routing branches)
// =====================================================================

func TestHandleJobError_ErrCancelled(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	dbJob := seedDBJob(t, env.dbClient, "job-cancel")

	ctx := testLoggerCtx()
	env.p.handleJobError(ctx, ErrCancelled, dbJob, env.updater, nil)

	items, _, _, err := env.dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{"job-cancel"}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	var got openai.BatchStatusInfo
	json.Unmarshal(items[0].Status, &got)
	if got.Status != openai.BatchStatusCancelled {
		t.Fatalf("status = %s, want %s", got.Status, openai.BatchStatusCancelled)
	}
}

func TestHandleJobError_ContextCanceled_ReEnqueues(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	dbJob := seedDBJob(t, env.dbClient, "job-ctx")
	task := &db.BatchJobPriority{ID: "job-ctx"}

	ctx := testLoggerCtx()
	env.p.handleJobError(ctx, context.Canceled, dbJob, env.updater, task)

	tasks, err := env.pqClient.PQDequeue(ctx, 0, 10)
	if err != nil {
		t.Fatalf("PQDequeue: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatalf("expected re-enqueued task, got none")
	}
}

func TestHandleJobError_DeadlineExceeded_ReEnqueues(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	dbJob := seedDBJob(t, env.dbClient, "job-deadline")
	task := &db.BatchJobPriority{ID: "job-deadline"}

	ctx := testLoggerCtx()
	env.p.handleJobError(ctx, context.DeadlineExceeded, dbJob, env.updater, task)

	tasks, err := env.pqClient.PQDequeue(ctx, 0, 10)
	if err != nil {
		t.Fatalf("PQDequeue: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatalf("expected re-enqueued task, got none")
	}
}

func TestHandleJobError_ContextCanceled_NilTask(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	dbJob := seedDBJob(t, env.dbClient, "job-ctx-nil")

	ctx := testLoggerCtx()
	// task is nil — should not panic, and job status should remain unchanged
	env.p.handleJobError(ctx, context.Canceled, dbJob, env.updater, nil)

	items, _, _, err := env.dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{"job-ctx-nil"}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	var got openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &got); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if got.Status != openai.BatchStatusInProgress {
		t.Fatalf("status = %s, want %s (unchanged)", got.Status, openai.BatchStatusInProgress)
	}
}

func TestHandleJobError_Default_MarksFailed(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	dbJob := seedDBJob(t, env.dbClient, "job-fail")

	ctx := testLoggerCtx()
	env.p.handleJobError(ctx, errors.New("some error"), dbJob, env.updater, nil)

	items, _, _, err := env.dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{"job-fail"}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	var got openai.BatchStatusInfo
	json.Unmarshal(items[0].Status, &got)
	if got.Status != openai.BatchStatusFailed {
		t.Fatalf("status = %s, want %s", got.Status, openai.BatchStatusFailed)
	}
}

// =====================================================================
// Tests: cleanupJobArtifacts
// =====================================================================

func TestCleanupJobArtifacts_RemovesDirectory(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	p := NewProcessor(cfg, validProcessorClients())

	jobDir, _ := p.jobRootDir("cleanup-job", "tenant-1")
	os.MkdirAll(filepath.Join(jobDir, "plans"), 0o755)
	os.WriteFile(filepath.Join(jobDir, "input.jsonl"), []byte("data"), 0o644)

	ctx := testLoggerCtx()
	p.cleanupJobArtifacts(ctx, "cleanup-job", "tenant-1")

	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("expected job directory to be removed, stat err: %v", err)
	}
}

// =====================================================================
// Tests: storeFileRecord error path
// =====================================================================

func TestStoreOutputFileRecord_DBError(t *testing.T) {
	cfg := config.NewConfig()
	cfg.DefaultOutputExpirationSeconds = 86400

	failDB := &dbStoreErrFileClient{err: errors.New("db write failed")}
	p := NewProcessor(cfg, &clientset.Clientset{FileDB: failDB})

	ctx := testLoggerCtx()
	err := p.storeFileRecord(ctx, "file_x", "output.jsonl", "tenant-1", 100, db.Tags{})
	if err == nil {
		t.Fatalf("expected error from DB failure")
	}
}

type dbStoreErrFileClient struct {
	db.FileDBClient
	err error
}

func (d *dbStoreErrFileClient) DBStore(_ context.Context, _ *db.FileItem) error {
	return d.err
}

// ---------------------------------------------------------------------------
// small helper
// ---------------------------------------------------------------------------

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return b
}

// readNonEmptyJSONLLines reads a JSONL file and returns non-empty lines as byte slices.
func readNonEmptyJSONLLines(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var lines [][]byte
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}
