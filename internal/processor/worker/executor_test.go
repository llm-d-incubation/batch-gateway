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
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	"github.com/llm-d-incubation/batch-gateway/internal/util/clientset"
	httpclient "github.com/llm-d-incubation/batch-gateway/pkg/clients/http"
	"github.com/llm-d-incubation/batch-gateway/pkg/clients/inference"
)

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
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

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
				Category: httpclient.ErrCategoryServer,
				Message:  "backend unavailable",
			}
		},
	}

	requests := []batch_types.Request{
		{CustomID: "req-err", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

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
	if result.Error.Code != string(httpclient.ErrCategoryServer) {
		t.Fatalf("error code = %q, want %q", result.Error.Code, httpclient.ErrCategoryServer)
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
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

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
	if result.Error.Code != string(httpclient.ErrCategoryServer) {
		t.Fatalf("error code = %q, want %q", result.Error.Code, httpclient.ErrCategoryServer)
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
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

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
	if result.Error.Code != string(httpclient.ErrCategoryParse) {
		t.Fatalf("error code = %q, want %q", result.Error.Code, httpclient.ErrCategoryParse)
	}
}

func TestExecuteOneRequest_BadOffset(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	requests := []batch_types.Request{
		{CustomID: "req-1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, &mockInferenceClient{}, requests, map[string]string{"m1": "m1"})

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
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

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
	err := env.p.processModel(ctx, ctx, inputFile, plansDir, "m1", "m1", writers, cancelReq, progress, nil)
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

// TestProcessModel_CancelStopsDispatch verifies that when the context is cancelled
// and cancelRequested is set (matching the real watchCancel flow), processModel stops
// dispatch via context cancellation and drains undispatched entries as batch_cancelled
// using the cancelRequested flag to determine the drain reason.
func TestProcessModel_CancelStopsDispatch(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, &mockInferenceClient{}, requests, map[string]string{"m1": "m1"})

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
	errWriter := bufio.NewWriter(&errBuf)
	writers := &outputWriters{output: writer, errors: errWriter}

	// Cancel context to simulate the real flow: watchCancel cancels inferCtx (which
	// propagates to execCtx passed to processModel) AND sets cancelRequested.
	ctx, cancel := context.WithCancel(testLoggerCtx())
	cancel()

	err := env.p.processModel(ctx, ctx, inputFile, plansDir, "m1", "m1", writers, cancelReq, progress, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	// Verify that undispatched entry was drained as batch_cancelled (reason from cancelRequested).
	if flushErr := errWriter.Flush(); flushErr != nil {
		t.Fatalf("flush error writer: %v", flushErr)
	}
	errLines := bytes.Split(bytes.TrimSpace(errBuf.Bytes()), []byte{'\n'})
	if len(errLines) != 1 {
		t.Fatalf("expected 1 drain entry in error output, got %d", len(errLines))
	}
	var drainEntry outputLine
	if unmarshalErr := json.Unmarshal(errLines[0], &drainEntry); unmarshalErr != nil {
		t.Fatalf("unmarshal drain entry: %v", unmarshalErr)
	}
	if drainEntry.Error == nil || drainEntry.Error.Code != batch_types.ErrCodeBatchCancelled {
		t.Fatalf("expected error code %s, got %+v", batch_types.ErrCodeBatchCancelled, drainEntry.Error)
	}
}

// TestProcessModel_CancelWritesInFlightToErrorFile verifies that when cancelRequested
// is set while an inference request is in-flight, the completed result is overwritten
// as batch_cancelled and written to the error file (not silently dropped).
func TestProcessModel_CancelWritesInFlightToErrorFile(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.GlobalConcurrency = 10
	cfg.PerModelMaxConcurrency = 10

	cancelReq := &atomic.Bool{}

	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			// Simulate cancel arriving while the request is in-flight:
			// set the flag after inference "completes" but before the
			// goroutine checks cancelRequested.
			cancelReq.Store(true)
			return &inference.GenerateResponse{RequestID: "srv", Response: []byte(`{"ok":true}`)}, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "inflight-1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, _ := os.Open(inputPath)
	defer inputFile.Close()

	plansDir, _ := env.p.jobPlansDir(jobInfo.JobID, jobInfo.TenantID)

	var outBuf, errBuf bytes.Buffer
	outWriter := bufio.NewWriter(&outBuf)
	errWriter := bufio.NewWriter(&errBuf)
	writers := &outputWriters{output: outWriter, errors: errWriter}

	progress := &executionProgress{
		total:   1,
		updater: env.updater,
		jobID:   jobInfo.JobID,
	}

	ctx := testLoggerCtx()
	_ = env.p.processModel(ctx, ctx, inputFile, plansDir, "m1", "m1", writers, cancelReq, progress, nil)

	if flushErr := outWriter.Flush(); flushErr != nil {
		t.Fatalf("flush output: %v", flushErr)
	}
	if flushErr := errWriter.Flush(); flushErr != nil {
		t.Fatalf("flush error: %v", flushErr)
	}

	// Output file should be empty — cancelled requests go to error file.
	if outBuf.Len() > 0 {
		t.Errorf("expected empty output file, got: %s", outBuf.String())
	}

	// Error file should have exactly 1 entry with batch_cancelled.
	errContent := bytes.TrimSpace(errBuf.Bytes())
	if len(errContent) == 0 {
		t.Fatal("expected cancelled entry in error file, got empty")
	}
	errLines := bytes.Split(errContent, []byte{'\n'})
	if len(errLines) != 1 {
		t.Fatalf("expected 1 error line, got %d", len(errLines))
	}

	var entry outputLine
	if err := json.Unmarshal(errLines[0], &entry); err != nil {
		t.Fatalf("unmarshal error line: %v", err)
	}
	if entry.CustomID != "inflight-1" {
		t.Errorf("custom_id = %q, want %q", entry.CustomID, "inflight-1")
	}
	if entry.Error == nil || entry.Error.Code != batch_types.ErrCodeBatchCancelled {
		t.Fatalf("expected error code %s, got %+v", batch_types.ErrCodeBatchCancelled, entry.Error)
	}
	if entry.Response != nil {
		t.Errorf("expected nil response for cancelled entry, got %+v", entry.Response)
	}

	counts := progress.counts()
	if counts.Failed != 1 {
		t.Errorf("failed count = %d, want 1", counts.Failed)
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
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

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
	err := env.p.processModel(ctx, ctx, inputFile, plansDir, "m1", "m1", writers, cancelReq, progress, nil)
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
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

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
		done <- env.p.processModel(ctx, ctx, inputFile, plansDir, "m1", "m1", writers, cancelReq, progress, nil)
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
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})
	cancelReq := &atomic.Bool{}

	ctx := testLoggerCtx()
	counts, err := env.p.executeJob(ctx, ctx, ctx, &jobExecutionParams{
		updater:         env.updater,
		jobInfo:         jobInfo,
		cancelRequested: cancelReq,
	})
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
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1", "m2": "m2"})
	cancelReq := &atomic.Bool{}

	ctx := testLoggerCtx()
	counts, err := env.p.executeJob(ctx, ctx, ctx, &jobExecutionParams{
		updater:         env.updater,
		jobInfo:         jobInfo,
		cancelRequested: cancelReq,
	})
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
			return nil, &inference.ClientError{Category: httpclient.ErrCategoryServer, Message: "cancelled"}
		},
	}

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})
	cancelReq := &atomic.Bool{}

	ctx, cancel := context.WithCancel(testLoggerCtx())
	cancel()

	_, err := env.p.executeJob(ctx, ctx, ctx, &jobExecutionParams{
		updater:         env.updater,
		jobInfo:         jobInfo,
		cancelRequested: cancelReq,
	})
	if err == nil {
		t.Fatalf("expected error on cancelled context")
	}
}

// TestExecuteJob_UserCancelFlag verifies that when inferCtx is cancelled and cancelRequested
// is set (matching the real watchCancel flow), executeJob returns ErrCancelled. Context
// cancellation stops dispatch; cancelRequested is used in the error-handling path to
// return the correct sentinel error.
func TestExecuteJob_UserCancelFlag(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, &mockInferenceClient{}, requests, map[string]string{"m1": "m1"})

	cancelReq := &atomic.Bool{}
	cancelReq.Store(true)

	ctx := testLoggerCtx()
	inferCtx, inferCancel := context.WithCancel(ctx)
	inferCancel()

	_, err := env.p.executeJob(ctx, ctx, inferCtx, &jobExecutionParams{
		updater:         env.updater,
		jobInfo:         jobInfo,
		cancelRequested: cancelReq,
	})
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected ErrCancelled, got: %v", err)
	}
}

// TestExecuteJob_CancelFlagSetAfterAllRequestsComplete verifies that if the cancel flag is set
// after all requests have already been dispatched and completed successfully (i.e. context
// cancellation never interrupted dispatch), executeJob still returns ErrCancelled rather than
// nil, preventing the job from being finalized as "completed".
func TestExecuteJob_CancelFlagSetAfterAllRequestsComplete(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	cancelReq := &atomic.Bool{}

	// The mock sets cancelRequested=true only after the inference call returns, simulating
	// the race where the cancel event arrives while (or just after) the last request completes.
	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			cancelReq.Store(true)
			return &inference.GenerateResponse{RequestID: "srv", Response: []byte(`{"ok":true}`)}, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	ctx := testLoggerCtx()
	_, err := env.p.executeJob(ctx, ctx, ctx, &jobExecutionParams{
		updater:         env.updater,
		jobInfo:         jobInfo,
		cancelRequested: cancelReq,
	})
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected ErrCancelled when cancel flag set after all requests complete, got: %v", err)
	}
}

// TestExecuteJob_InferCtxCancel_AbortsInflightRequests verifies that cancelling inferCtx
// aborts in-flight inference requests. The mock blocks until it sees context cancellation,
// simulating a long-running inference call that should be interrupted.
func TestExecuteJob_InferCtxCancel_AbortsInflightRequests(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	inferStarted := make(chan struct{})
	mock := &mockInferenceClient{
		generateFn: func(ctx context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			close(inferStarted)
			// Block until context is cancelled (simulates slow inference)
			<-ctx.Done()
			return nil, &inference.ClientError{
				Category: httpclient.ErrCategoryServer,
				Message:  "context cancelled",
				RawError: ctx.Err(),
			}
		},
	}

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	cancelReq := &atomic.Bool{}
	ctx := testLoggerCtx()
	inferCtx, inferCancelFn := context.WithCancel(ctx)

	type result struct {
		counts *openai.BatchRequestCounts
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		counts, err := env.p.executeJob(ctx, ctx, inferCtx, &jobExecutionParams{
			updater:         env.updater,
			jobInfo:         jobInfo,
			cancelRequested: cancelReq,
		})
		resCh <- result{counts, err}
	}()

	<-inferStarted
	cancelReq.Store(true)
	inferCancelFn()

	select {
	case res := <-resCh:
		if !errors.Is(res.err, ErrCancelled) {
			t.Fatalf("expected ErrCancelled, got: %v", res.err)
		}
		if res.counts == nil {
			t.Fatal("expected non-nil counts")
		}
		if res.counts.Total != 1 {
			t.Errorf("Total = %d, want 1", res.counts.Total)
		}
		if res.counts.Completed != 0 {
			t.Errorf("Completed = %d, want 0 (request was aborted)", res.counts.Completed)
		}
		if res.counts.Failed != 1 {
			t.Errorf("Failed = %d, want 1 (aborted request counted as failed)", res.counts.Failed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executeJob did not return within 5s after inferCtx cancellation")
	}
}

// TestExecuteJob_SLOExpiredBeforeDispatch verifies that when the SLO deadline has already
// passed before execution begins, executeJob returns ErrExpired immediately with the total
// request count and no output/error files are written (early-exit fast path).
func TestExecuteJob_SLOExpiredBeforeDispatch(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	requests := []batch_types.Request{
		{CustomID: "r1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "r2", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "r3", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, &mockInferenceClient{}, requests, map[string]string{"m1": "m1"})
	cancelReq := &atomic.Bool{}

	ctx := testLoggerCtx()
	// SLO deadline already in the past: early check fires before any files are opened.
	sloCtx, cancel := context.WithDeadline(ctx, time.Now().Add(-1*time.Second))
	defer cancel()

	counts, err := env.p.executeJob(ctx, sloCtx, sloCtx, &jobExecutionParams{
		updater:         env.updater,
		jobInfo:         jobInfo,
		cancelRequested: cancelReq,
	})
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("expected ErrExpired, got: %v", err)
	}
	if counts == nil {
		t.Fatal("expected non-nil counts")
	}
	// Early exit: total is known from the model map, but no requests were dispatched or drained.
	if counts.Total != 3 {
		t.Fatalf("Total = %d, want 3", counts.Total)
	}
	if counts.Completed != 0 {
		t.Fatalf("Completed = %d, want 0", counts.Completed)
	}
	if counts.Failed != 0 {
		t.Fatalf("Failed = %d, want 0 (no drain on early exit)", counts.Failed)
	}

	// No output or error files are written on early exit: files are only opened after the SLO check.
	outputPath, _ := env.p.jobOutputFilePath(jobInfo.JobID, jobInfo.TenantID)
	errorPath, _ := env.p.jobErrorFilePath(jobInfo.JobID, jobInfo.TenantID)
	if _, statErr := os.Stat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("output.jsonl should not exist on early SLO exit, got stat err: %v", statErr)
	}
	if _, statErr := os.Stat(errorPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("error.jsonl should not exist on early SLO exit, got stat err: %v", statErr)
	}
}

// TestExecuteJob_SLOExpiredDuringDispatch verifies that when the SLO deadline fires while
// requests are being dispatched, completed requests are preserved in the output file,
// undispatched requests are drained to the error file as batch_expired, and executeJob
// returns ErrExpired with accurate partial counts.
//
// This exercises the full context-cancellation chain for SLO expiry:
//
//	sloCtx (WithDeadline) → inferCtx (WithCancel) → execCtx (WithCancel)
//	         DeadlineExceeded       Canceled                Canceled
//
// checkAbortCondition sees Canceled on execCtx to stop dispatch;
// processModel's drain switch checks sloCtx.Err() == DeadlineExceeded to select batch_expired.
func TestExecuteJob_SLOExpiredDuringDispatch(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.GlobalConcurrency = 1
	cfg.PerModelMaxConcurrency = 1

	// The mock blocks until the context is cancelled (SLO deadline fires).
	// Concurrency = 1, so the first request holds the semaphore while blocking,
	// preventing the second request from being dispatched. When the deadline fires,
	// semaphore.Acquire returns an error and the dispatch loop exits.
	mock := &mockInferenceClient{
		generateFn: func(ctx context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			<-ctx.Done()
			return &inference.GenerateResponse{RequestID: "srv", Response: []byte(`{"ok":true}`)}, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "r1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "r2", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "r3", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})
	cancelReq := &atomic.Bool{}

	ctx := testLoggerCtx()
	// Use context.WithDeadline so sloCtx.Err() returns DeadlineExceeded (matching real code).
	sloCtx, sloCancel := context.WithDeadline(ctx, time.Now().Add(100*time.Millisecond))
	defer sloCancel()

	type result struct {
		counts *openai.BatchRequestCounts
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		counts, err := env.p.executeJob(ctx, sloCtx, sloCtx, &jobExecutionParams{
			updater:         env.updater,
			jobInfo:         jobInfo,
			cancelRequested: cancelReq,
		})
		resCh <- result{counts, err}
	}()

	select {
	case res := <-resCh:
		if !errors.Is(res.err, ErrExpired) {
			t.Fatalf("expected ErrExpired, got: %v", res.err)
		}
		if res.counts == nil {
			t.Fatal("expected non-nil counts")
		}
		if res.counts.Total != 3 {
			t.Errorf("Total = %d, want 3", res.counts.Total)
		}
		// r1 was dispatched and completed (mock returns success after ctx cancellation);
		// r2, r3 were never dispatched and drained as batch_expired.
		if res.counts.Completed != 1 {
			t.Errorf("Completed = %d, want 1", res.counts.Completed)
		}
		if res.counts.Failed != 2 {
			t.Errorf("Failed = %d, want 2 (undispatched drained as expired)", res.counts.Failed)
		}

		// Verify the error file contains batch_expired entries for undispatched requests.
		errorPath, _ := env.p.jobErrorFilePath(jobInfo.JobID, jobInfo.TenantID)
		errLines := readNonEmptyJSONLLines(t, errorPath)
		if len(errLines) != 2 {
			t.Fatalf("error.jsonl lines = %d, want 2", len(errLines))
		}
		for i, line := range errLines {
			var entry outputLine
			if err := json.Unmarshal(line, &entry); err != nil {
				t.Fatalf("unmarshal error line %d: %v", i, err)
			}
			if entry.Error == nil || entry.Error.Code != batch_types.ErrCodeBatchExpired {
				t.Errorf("error line %d: expected code %s, got %+v", i, batch_types.ErrCodeBatchExpired, entry.Error)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executeJob did not return within 5s")
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
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	outputPath, _ := env.p.jobOutputFilePath(jobID, tenantID)
	if err := os.WriteFile(outputPath, []byte(`{"id":"batch_req_1","custom_id":"r1","response":{"status_code":200}}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dbJob := seedDBJob(t, env.dbClient, jobID)
	counts := &openai.BatchRequestCounts{Total: 1, Completed: 1, Failed: 0}

	ctx := testLoggerCtx()
	var cancelRequested atomic.Bool
	err := env.p.finalizeJob(ctx, env.updater, dbJob, jobInfo, counts, &cancelRequested)
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
	env.p.files.storage = &failNTimesFilesClient{failCount: 100}

	jobID := "finalize-fail"
	tenantID := "tenant-1"
	jobInfo := &batch_types.JobInfo{JobID: jobID, TenantID: tenantID}

	jobDir, _ := env.p.jobRootDir(jobID, tenantID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	outputPath, _ := env.p.jobOutputFilePath(jobID, tenantID)
	if err := os.WriteFile(outputPath, []byte("output\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dbJob := seedDBJob(t, env.dbClient, jobID)
	counts := &openai.BatchRequestCounts{Total: 1, Completed: 1}

	ctx := testLoggerCtx()
	var cancelRequested atomic.Bool
	err := env.p.finalizeJob(ctx, env.updater, dbJob, jobInfo, counts, &cancelRequested)
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
			return nil, &inference.ClientError{Category: httpclient.ErrCategoryServer, Message: "mock error"}
		},
	}

	requests := []batch_types.Request{
		{CustomID: "r1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "r2", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})
	cancelReq := &atomic.Bool{}

	ctx := testLoggerCtx()
	counts, err := env.p.executeJob(ctx, ctx, ctx, &jobExecutionParams{
		updater:         env.updater,
		jobInfo:         jobInfo,
		cancelRequested: cancelReq,
	})
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
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	outputPath, _ := env.p.jobOutputFilePath(jobID, tenantID)
	if err := os.WriteFile(outputPath, []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	errorPath, _ := env.p.jobErrorFilePath(jobID, tenantID)
	if err := os.WriteFile(errorPath, []byte(`{"id":"batch_req_1","custom_id":"r1","error":{"code":"server_error","message":"fail"}}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dbJob := seedDBJob(t, env.dbClient, jobID)
	counts := &openai.BatchRequestCounts{Total: 1, Completed: 0, Failed: 1}

	ctx := testLoggerCtx()
	var cancelRequested atomic.Bool
	if err := env.p.finalizeJob(ctx, env.updater, dbJob, jobInfo, counts, &cancelRequested); err != nil {
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
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	outputPath, _ := env.p.jobOutputFilePath(jobID, tenantID)
	if err := os.WriteFile(outputPath, []byte(`{"id":"batch_req_1","custom_id":"r1","response":{"status_code":200}}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	errorPath, _ := env.p.jobErrorFilePath(jobID, tenantID)
	if err := os.WriteFile(errorPath, []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dbJob := seedDBJob(t, env.dbClient, jobID)
	counts := &openai.BatchRequestCounts{Total: 1, Completed: 1, Failed: 0}

	ctx := testLoggerCtx()
	var cancelRequested atomic.Bool
	if err := env.p.finalizeJob(ctx, env.updater, dbJob, jobInfo, counts, &cancelRequested); err != nil {
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
	env.p.handleJobError(ctx, &jobExecutionParams{
		updater: env.updater,
		jobItem: dbJob,
	}, ErrCancelled)

	items, _, _, err := env.dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{"job-cancel"}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	var got openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &got); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
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
	env.p.handleJobError(ctx, &jobExecutionParams{
		updater: env.updater,
		jobItem: dbJob,
		task:    task,
	}, context.Canceled)

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
	env.p.handleJobError(ctx, &jobExecutionParams{
		updater: env.updater,
		jobItem: dbJob,
		task:    task,
	}, context.DeadlineExceeded)

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
	env.p.handleJobError(ctx, &jobExecutionParams{
		updater: env.updater,
		jobItem: dbJob,
	}, context.Canceled)

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
	env.p.handleJobError(ctx, &jobExecutionParams{
		updater: env.updater,
		jobItem: dbJob,
	}, errors.New("some error"))

	items, _, _, err := env.dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{"job-fail"}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	var got openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &got); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if got.Status != openai.BatchStatusFailed {
		t.Fatalf("status = %s, want %s", got.Status, openai.BatchStatusFailed)
	}
}

// =====================================================================
// Tests: handleCancelled / handleFailedWithPartial / handleFailed
// with partial output
// =====================================================================

func TestHandleCancelled_Execution_UploadsPartialOutput(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	jobID := "job-cancel-partial"
	tenantID := "tenant__tenantA"
	dbJob := &db.BatchItem{
		BaseIndexes:  db.BaseIndexes{ID: jobID, TenantID: tenantID, Tags: db.Tags{}},
		BaseContents: db.BaseContents{Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusCancelling})},
	}
	if err := env.dbClient.DBStore(context.Background(), dbJob); err != nil {
		t.Fatalf("DBStore: %v", err)
	}

	createPartialOutputFiles(t, env.p, jobID, tenantID)

	jobInfo := &batch_types.JobInfo{JobID: jobID, TenantID: tenantID}
	counts := &openai.BatchRequestCounts{Total: 5, Completed: 3, Failed: 2}

	ctx := testLoggerCtx()
	if err := env.p.handleCancelled(ctx, &jobExecutionParams{
		updater:       env.updater,
		jobItem:       dbJob,
		jobInfo:       jobInfo,
		requestCounts: counts,
	}); err != nil {
		t.Fatalf("handleCancelled: %v", err)
	}

	items, _, _, err := env.dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	var got openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Status != openai.BatchStatusCancelled {
		t.Fatalf("status = %s, want cancelled", got.Status)
	}
	if got.RequestCounts.Total != 5 || got.RequestCounts.Completed != 3 || got.RequestCounts.Failed != 2 {
		t.Fatalf("request_counts = %+v, want {5,3,2}", got.RequestCounts)
	}
	if got.OutputFileID == "" {
		t.Fatal("expected output_file_id to be set")
	}
	if got.ErrorFileID == "" {
		t.Fatal("expected error_file_id to be set")
	}
}

func TestHandleFailedWithPartial_Execution_UploadsPartialOutput(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	jobID := "job-fail-partial"
	tenantID := "tenant__tenantA"
	dbJob := &db.BatchItem{
		BaseIndexes:  db.BaseIndexes{ID: jobID, TenantID: tenantID, Tags: db.Tags{}},
		BaseContents: db.BaseContents{Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress})},
	}
	if err := env.dbClient.DBStore(context.Background(), dbJob); err != nil {
		t.Fatalf("DBStore: %v", err)
	}

	createPartialOutputFiles(t, env.p, jobID, tenantID)

	jobInfo := &batch_types.JobInfo{JobID: jobID, TenantID: tenantID}
	counts := &openai.BatchRequestCounts{Total: 10, Completed: 7, Failed: 3}

	ctx := testLoggerCtx()
	if err := env.p.handleFailedWithPartial(ctx, env.updater, dbJob, jobInfo, counts); err != nil {
		t.Fatalf("handleFailedWithPartial: %v", err)
	}

	items, _, _, err := env.dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	var got openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Status != openai.BatchStatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	if got.RequestCounts.Total != 10 || got.RequestCounts.Completed != 7 || got.RequestCounts.Failed != 3 {
		t.Fatalf("request_counts = %+v, want {10,7,3}", got.RequestCounts)
	}
	if got.OutputFileID == "" {
		t.Fatal("expected output_file_id to be set")
	}
	if got.ErrorFileID == "" {
		t.Fatal("expected error_file_id to be set")
	}
}

func TestHandleFailed_Finalization_RecordsCountsOnly(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	jobID := "job-fail-finalization"
	dbJob := seedDBJob(t, env.dbClient, jobID)

	counts := &openai.BatchRequestCounts{Total: 8, Completed: 8, Failed: 0}

	ctx := testLoggerCtx()
	if err := env.p.handleFailed(ctx, env.updater, dbJob, counts); err != nil {
		t.Fatalf("handleFailed: %v", err)
	}

	items, _, _, err := env.dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	var got openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Status != openai.BatchStatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	if got.RequestCounts.Total != 8 || got.RequestCounts.Completed != 8 || got.RequestCounts.Failed != 0 {
		t.Fatalf("request_counts = %+v, want {8,8,0}", got.RequestCounts)
	}
	if got.OutputFileID != "" {
		t.Fatalf("expected empty output_file_id, got %s", got.OutputFileID)
	}
	if got.ErrorFileID != "" {
		t.Fatalf("expected empty error_file_id, got %s", got.ErrorFileID)
	}
}

// =====================================================================
// Tests: uploadPartialResults — empty / missing files
// =====================================================================

// TestUploadPartialResults_EmptyFiles verifies that when both output and error files
// exist but are empty (0 bytes), uploadPartialResults returns empty file IDs and does
// not create any file records in the database.
func TestUploadPartialResults_EmptyFiles(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	jobID := "partial-empty"
	tenantID := "tenant__tenantA"

	jobDir, err := env.p.jobRootDir(jobID, tenantID)
	if err != nil {
		t.Fatalf("jobRootDir: %v", err)
	}
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	outputPath, _ := env.p.jobOutputFilePath(jobID, tenantID)
	if err := os.WriteFile(outputPath, []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFile output: %v", err)
	}
	errorPath, _ := env.p.jobErrorFilePath(jobID, tenantID)
	if err := os.WriteFile(errorPath, []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	jobInfo := &batch_types.JobInfo{JobID: jobID, TenantID: tenantID}
	dbJob := &db.BatchItem{
		BaseIndexes: db.BaseIndexes{ID: jobID, TenantID: tenantID, Tags: db.Tags{}},
	}

	ctx := testLoggerCtx()
	outputFileID, errorFileID := env.p.uploadPartialResults(ctx, jobInfo, dbJob)

	if outputFileID != "" {
		t.Fatalf("outputFileID = %q, want empty (output file was 0 bytes)", outputFileID)
	}
	if errorFileID != "" {
		t.Fatalf("errorFileID = %q, want empty (error file was 0 bytes)", errorFileID)
	}
}

// TestUploadPartialResults_MissingFiles verifies that when neither output nor error
// files exist on disk, uploadPartialResults returns empty file IDs without error.
func TestUploadPartialResults_MissingFiles(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	jobID := "partial-missing"
	tenantID := "tenant__tenantA"

	jobDir, err := env.p.jobRootDir(jobID, tenantID)
	if err != nil {
		t.Fatalf("jobRootDir: %v", err)
	}
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	jobInfo := &batch_types.JobInfo{JobID: jobID, TenantID: tenantID}
	dbJob := &db.BatchItem{
		BaseIndexes: db.BaseIndexes{ID: jobID, TenantID: tenantID, Tags: db.Tags{}},
	}

	ctx := testLoggerCtx()
	outputFileID, errorFileID := env.p.uploadPartialResults(ctx, jobInfo, dbJob)

	if outputFileID != "" {
		t.Fatalf("outputFileID = %q, want empty (output file does not exist)", outputFileID)
	}
	if errorFileID != "" {
		t.Fatalf("errorFileID = %q, want empty (error file does not exist)", errorFileID)
	}
}

// =====================================================================
// Tests: cleanupJobArtifacts
// =====================================================================

func TestCleanupJobArtifacts_RemovesDirectory(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	p := mustNewProcessor(t, cfg, validProcessorClients())

	jobDir, _ := p.jobRootDir("cleanup-job", "tenant-1")
	if err := os.MkdirAll(filepath.Join(jobDir, "plans"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "input.jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

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
	p := mustNewProcessor(t, cfg, &clientset.Clientset{FileDB: failDB})

	ctx := testLoggerCtx()
	err := p.storeFileRecord(ctx, "file_x", "output.jsonl", "tenant-1", 100, db.Tags{})
	if err == nil {
		t.Fatalf("expected error from DB failure")
	}
}
