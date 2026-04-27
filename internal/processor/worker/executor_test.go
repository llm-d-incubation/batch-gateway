package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	mockdb "github.com/llm-d-incubation/batch-gateway/internal/database/mock"
	mockfiles "github.com/llm-d-incubation/batch-gateway/internal/files_store/mock"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
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
			if req.Headers == nil {
				t.Fatal("expected headers to be set")
			}
			if _, ok := req.Headers[sloTTFTMSHeader]; !ok {
				t.Fatalf("expected %s header to be set", sloTTFTMSHeader)
			}
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

	ctx := testLoggerCtx(t)
	sloCtx, sloCancel := context.WithDeadline(ctx, time.Now().Add(1*time.Second))
	defer sloCancel()
	result, err := env.p.executeOneRequest(ctx, sloCtx, inputFile, entries[0], "m1", nil)
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

func TestExecuteOneRequest_NonHTTPError(t *testing.T) {
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

	ctx := testLoggerCtx(t)
	sloCtx, sloCancel := context.WithDeadline(ctx, time.Now().Add(1*time.Second))
	defer sloCancel()
	result, err := env.p.executeOneRequest(ctx, sloCtx, inputFile, entries[0], "m1", nil)
	if err != nil {
		t.Fatalf("executeOneRequest should not return error for inference failure, got: %v", err)
	}
	if result.Error == nil {
		t.Fatalf("expected error field in output line for non-HTTP error")
	}
	if result.Error.Code != string(httpclient.ErrCategoryServer) {
		t.Fatalf("error code = %q, want %q", result.Error.Code, httpclient.ErrCategoryServer)
	}
	if result.Response != nil {
		t.Fatalf("expected nil response on non-HTTP error")
	}
}

// TestExecuteOneRequest_NilInferenceClient covers recovery when model_map/plan files
// reference a model that no longer has a gateway client (config drift after ingestion).
// ClientFor returns nil; the request must fail gracefully like ingestion-time rejection,
// not as a fatal processModel error.
func TestExecuteOneRequest_NilInferenceClient(t *testing.T) {
	ctx := testLoggerCtx(t)
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	resolver, err := inference.NewPerModelResolver(
		map[string]inference.GatewayClientConfig{
			"other-model": {URL: "http://fake:8000"},
		},
		testLogger(t),
	)
	if err != nil {
		t.Fatalf("NewPerModelResolver: %v", err)
	}

	clients := &clientset.Clientset{
		BatchDB:   newMockBatchDBClient(),
		FileDB:    newMockFileDBClient(),
		File:      mockfiles.NewMockBatchFilesClient(t.TempDir()),
		Queue:     mockdb.NewMockBatchPriorityQueueClient(),
		Status:    mockdb.NewMockBatchStatusClient(),
		Event:     mockdb.NewMockBatchEventChannelClient(),
		Inference: resolver,
	}
	p := mustNewProcessor(t, cfg, clients)

	jobID := "test-job"
	tenantID := "tenant-1"
	requests := []batch_types.Request{
		{CustomID: "req-1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}

	jobRootDir, err := p.jobRootDir(jobID, tenantID)
	if err != nil {
		t.Fatalf("jobRootDir: %v", err)
	}
	if err := os.MkdirAll(jobRootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	inputPath := filepath.Join(jobRootDir, "input.jsonl")
	rawInput := writeInputJSONL(t, inputPath, requests)
	allEntries := planEntriesFromLines(rawInput)

	plansDir := filepath.Join(jobRootDir, "plans")
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		t.Fatalf("MkdirAll plans: %v", err)
	}
	writePlanFile(t, plansDir, "m1", allEntries)

	writeModelMap(t, jobRootDir, modelMapFile{
		ModelToSafe: map[string]string{"m1": "m1"},
		SafeToModel: map[string]string{"m1": "m1"},
		LineCount:   1,
	})

	inputFile, err := os.Open(inputPath)
	if err != nil {
		t.Fatalf("open input: %v", err)
	}
	defer inputFile.Close()

	sloCtx, sloCancel := context.WithDeadline(ctx, time.Now().Add(1*time.Second))
	defer sloCancel()
	result, err := p.executeOneRequest(ctx, sloCtx, inputFile, allEntries[0], "m1", nil)
	if err != nil {
		t.Fatalf("executeOneRequest: %v", err)
	}
	if result.Error == nil {
		t.Fatal("expected model_not_found output line")
	}
	if result.Error.Code != inference.ErrCodeModelNotFound {
		t.Fatalf("error code = %q, want %q", result.Error.Code, inference.ErrCodeModelNotFound)
	}
	if result.CustomID != "req-1" {
		t.Fatalf("CustomID = %q, want req-1", result.CustomID)
	}
}

func TestExecuteOneRequest_HTTPError(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	errorBody := []byte(`{"error":{"message":"Invalid model","type":"invalid_request_error","code":"model_not_found"}}`)
	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			return nil, &inference.ClientError{
				Category:     httpclient.ErrCategoryInvalidReq,
				Message:      "HTTP 422: Invalid model",
				StatusCode:   422,
				ResponseBody: errorBody,
			}
		},
	}

	requests := []batch_types.Request{
		{CustomID: "req-http-err", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, _ := os.Open(inputPath)
	defer inputFile.Close()

	jobRootDir, _ := env.p.jobRootDir(jobInfo.JobID, jobInfo.TenantID)
	entries := planEntriesFromLines(mustReadFile(t, filepath.Join(jobRootDir, "input.jsonl")))

	ctx := testLoggerCtx(t)
	sloCtx, sloCancel := context.WithDeadline(ctx, time.Now().Add(1*time.Second))
	defer sloCancel()
	result, err := env.p.executeOneRequest(ctx, sloCtx, inputFile, entries[0], "m1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != nil {
		t.Fatalf("expected nil error for HTTP error response, got: %+v", result.Error)
	}
	if result.Response == nil {
		t.Fatalf("expected response field for HTTP error")
	}
	if result.Response.StatusCode != 422 {
		t.Fatalf("status_code = %d, want 422", result.Response.StatusCode)
	}
	if result.Response.Body == nil {
		t.Fatalf("expected non-nil body")
	}
	// Verify the original error body is preserved
	errObj, ok := result.Response.Body["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected error object in body, got: %v", result.Response.Body)
	}
	if errObj["message"] != "Invalid model" {
		t.Fatalf("expected error message 'Invalid model', got: %v", errObj["message"])
	}
	if errObj["code"] != "model_not_found" {
		t.Fatalf("expected error code 'model_not_found', got: %v", errObj["code"])
	}
}

func TestExecuteOneRequest_HTTPErrorEmptyBody(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			return nil, &inference.ClientError{
				Category:     httpclient.ErrCategoryServer,
				Message:      "HTTP 502: ",
				StatusCode:   502,
				ResponseBody: nil,
			}
		},
	}

	requests := []batch_types.Request{
		{CustomID: "req-empty", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, _ := os.Open(inputPath)
	defer inputFile.Close()

	jobRootDir, _ := env.p.jobRootDir(jobInfo.JobID, jobInfo.TenantID)
	entries := planEntriesFromLines(mustReadFile(t, filepath.Join(jobRootDir, "input.jsonl")))

	ctx := testLoggerCtx(t)
	sloCtx, sloCancel := context.WithDeadline(ctx, time.Now().Add(1*time.Second))
	defer sloCancel()
	result, err := env.p.executeOneRequest(ctx, sloCtx, inputFile, entries[0], "m1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Response == nil {
		t.Fatalf("expected response field for HTTP error")
	}
	if result.Response.StatusCode != 502 {
		t.Fatalf("status_code = %d, want 502", result.Response.StatusCode)
	}
	// Body should be empty object {}, not null
	if result.Response.Body == nil {
		t.Fatalf("expected non-nil body (empty object), got nil")
	}
	if len(result.Response.Body) != 0 {
		t.Fatalf("expected empty body object, got: %v", result.Response.Body)
	}
}

func TestExecuteOneRequest_HTTPErrorNonJSONBody(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			return nil, &inference.ClientError{
				Category:     httpclient.ErrCategoryServer,
				Message:      "HTTP 500: Bad Gateway",
				StatusCode:   500,
				ResponseBody: []byte("<html>Bad Gateway</html>"),
			}
		},
	}

	requests := []batch_types.Request{
		{CustomID: "req-html", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, _ := os.Open(inputPath)
	defer inputFile.Close()

	jobRootDir, _ := env.p.jobRootDir(jobInfo.JobID, jobInfo.TenantID)
	entries := planEntriesFromLines(mustReadFile(t, filepath.Join(jobRootDir, "input.jsonl")))

	ctx := testLoggerCtx(t)
	sloCtx, sloCancel := context.WithDeadline(ctx, time.Now().Add(1*time.Second))
	defer sloCancel()
	result, err := env.p.executeOneRequest(ctx, sloCtx, inputFile, entries[0], "m1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Response == nil {
		t.Fatalf("expected response field for HTTP error")
	}
	// Non-JSON body should be wrapped in synthetic error object
	errObj, ok := result.Response.Body["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected synthetic error object in body, got: %v", result.Response.Body)
	}
	if errObj["message"] != "<html>Bad Gateway</html>" {
		t.Fatalf("expected original body as message, got: %v", errObj["message"])
	}
	if errObj["type"] != "server_error" {
		t.Fatalf("expected type %q, got: %v", "server_error", errObj["type"])
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

	ctx := testLoggerCtx(t)
	sloCtx, sloCancel := context.WithDeadline(ctx, time.Now().Add(1*time.Second))
	defer sloCancel()
	result, err := env.p.executeOneRequest(ctx, sloCtx, inputFile, entries[0], "m1", nil)
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

	ctx := testLoggerCtx(t)
	sloCtx, sloCancel := context.WithDeadline(ctx, time.Now().Add(1*time.Second))
	defer sloCancel()
	result, err := env.p.executeOneRequest(ctx, sloCtx, inputFile, entries[0], "m1", nil)
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
	ctx := testLoggerCtx(t)
	sloCtx, sloCancel := context.WithDeadline(ctx, time.Now().Add(1*time.Second))
	defer sloCancel()
	_, err := env.p.executeOneRequest(ctx, sloCtx, inputFile, badEntry, "m1", nil)
	if err == nil {
		t.Fatalf("expected error for bad offset")
	}
}

func TestExecuteOneRequest_SLOExpiredBeforeExecution(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			return &inference.GenerateResponse{RequestID: "srv", Response: []byte(`{"ok":true}`)}, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "req-1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, _ := os.Open(inputPath)
	defer inputFile.Close()

	jobRootDir, _ := env.p.jobRootDir(jobInfo.JobID, jobInfo.TenantID)
	entries := planEntriesFromLines(mustReadFile(t, filepath.Join(jobRootDir, "input.jsonl")))

	ctx := testLoggerCtx(t)
	sloCtx, sloCancel := context.WithDeadline(ctx, time.Now().Add(-1*time.Second))
	defer sloCancel()
	result, err := env.p.executeOneRequest(ctx, sloCtx, inputFile, entries[0], "m1", nil)
	if err != nil {
		t.Fatalf("executeOneRequest error: %v", err)
	}
	if result.Error == nil {
		t.Fatalf("expected error for SLO expired during execution")
	}
	if result.Error.Code != string(batch_types.ErrCodeBatchExpired) {
		t.Fatalf("error code = %q, want %q", result.Error.Code, batch_types.ErrCodeBatchExpired)
	}
	if result.Error.Message != batch_types.ErrCodeBatchExpired.Message() {
		t.Fatalf("error message = %q, want %q", result.Error.Message, batch_types.ErrCodeBatchExpired.Message())
	}
}

func TestExecuteOneRequest_SLOExpiredDuringExecution(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			return &inference.GenerateResponse{RequestID: "srv", Response: []byte(`{"ok":true}`)}, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "req-1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	inputPath, _ := env.p.jobInputFilePath(jobInfo.JobID, jobInfo.TenantID)
	inputFile, _ := os.Open(inputPath)
	defer inputFile.Close()

	jobRootDir, _ := env.p.jobRootDir(jobInfo.JobID, jobInfo.TenantID)
	entries := planEntriesFromLines(mustReadFile(t, filepath.Join(jobRootDir, "input.jsonl")))

	ctx := testLoggerCtx(t)
	sloCtx, sloCancel := context.WithDeadline(ctx, time.Now().Add(10*time.Nanosecond))
	defer sloCancel()
	result, err := env.p.executeOneRequest(ctx, sloCtx, inputFile, entries[0], "m1", nil)
	if err != nil {
		t.Fatalf("executeOneRequest error: %v", err)
	}
	if result.Error == nil {
		t.Fatalf("expected error for SLO expired during execution")
	}
	if result.Error.Code != string(batch_types.ErrCodeBatchExpired) {
		t.Fatalf("error code = %q, want %q", result.Error.Code, batch_types.ErrCodeBatchExpired)
	}
	if result.Error.Message != batch_types.ErrCodeBatchExpired.Message() {
		t.Fatalf("error message = %q, want %q", result.Error.Message, batch_types.ErrCodeBatchExpired.Message())
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

	progress := &executionProgress{
		total:   int64(len(requests)),
		updater: env.updater,
		jobID:   jobInfo.JobID,
	}

	var errBuf bytes.Buffer
	writers := &outputWriters{output: writer, errors: bufio.NewWriter(&errBuf)}

	ctx := testLoggerCtx(t)
	err := env.p.processModel(ctx, ctx, ctx, context.Background(), inputFile, plansDir, "m1", "m1", writers, progress, nil)
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

// TestProcessModel_CancelStopsDispatch verifies that when userCancelCtx is cancelled,
// processModel stops dispatch and drains undispatched entries as batch_cancelled.
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

	progress := &executionProgress{
		total:   1,
		updater: env.updater,
		jobID:   jobInfo.JobID,
	}

	var errBuf bytes.Buffer
	errWriter := bufio.NewWriter(&errBuf)
	writers := &outputWriters{output: writer, errors: errWriter}

	// Cancel ctx to simulate requestAbortCtx being cancelled (by watchCancel calling requestAbortFn).
	// Separately pass ctx as userCancelCtx so drain chooses errCancelled, not errShutdown.
	// mainCtx (baseCtx) is not cancelled — errShutdown should NOT fire.
	// sloCtx is background — drain should choose userCancelCtx path (errCancelled), not SLO.
	baseCtx := testLoggerCtx(t)
	ctx, cancel := context.WithCancel(baseCtx)
	cancel()

	err := env.p.processModel(ctx, baseCtx, context.Background(), ctx, inputFile, plansDir, "m1", "m1", writers, progress, nil)
	if !errors.Is(err, errCancelled) {
		t.Fatalf("expected errCancelled, got: %v", err)
	}

	// Verify that undispatched entry was drained as batch_cancelled.
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
	if drainEntry.Error == nil || drainEntry.Error.Code != string(batch_types.ErrCodeBatchCancelled) {
		t.Fatalf("expected error code %s, got %+v", batch_types.ErrCodeBatchCancelled, drainEntry.Error)
	}
}

// TestProcessModel_CancelWritesInFlightToErrorFile verifies that when userCancelCtx is
// cancelled after inference returns (but before the goroutine writes the result), the
// completed response is overwritten as batch_cancelled and written to the error file.
// This tests the "late cancel overwrite" path — the cancel arrives between Generate()
// returning and the result being written. processModel must return errCancelled.
func TestProcessModel_CancelWritesInFlightToErrorFile(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.GlobalConcurrency = 10
	cfg.PerModelMaxConcurrency = 10

	userCancelCtx, abortFn := context.WithCancel(context.Background())

	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			abortFn()
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

	ctx := testLoggerCtx(t)
	modelErr := env.p.processModel(ctx, ctx, ctx, userCancelCtx, inputFile, plansDir, "m1", "m1", writers, progress, nil)
	if !errors.Is(modelErr, errCancelled) {
		t.Fatalf("expected errCancelled from processModel, got: %v", modelErr)
	}

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
	if entry.Error == nil || entry.Error.Code != string(batch_types.ErrCodeBatchCancelled) {
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

	progress := &executionProgress{
		total:   int64(len(requests)),
		updater: env.updater,
		jobID:   jobInfo.JobID,
	}

	var errBuf bytes.Buffer
	writers := &outputWriters{output: writer, errors: bufio.NewWriter(&errBuf)}

	ctx := testLoggerCtx(t)
	err := env.p.processModel(ctx, ctx, ctx, context.Background(), inputFile, plansDir, "m1", "m1", writers, progress, nil)
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

	progress := &executionProgress{
		total:   int64(len(requests)),
		updater: env.updater,
		jobID:   jobInfo.JobID,
	}

	ctx, cancel := context.WithCancel(testLoggerCtx(t))

	var errBuf bytes.Buffer
	writers := &outputWriters{output: writer, errors: bufio.NewWriter(&errBuf)}

	done := make(chan error, 1)
	go func() {
		done <- env.p.processModel(ctx, ctx, ctx, context.Background(), inputFile, plansDir, "m1", "m1", writers, progress, nil)
	}()

	<-started
	cancel()
	close(block)

	err := <-done
	if err == nil {
		t.Fatalf("expected error on context cancellation")
	}
}

// TestProcessModel_SiblingAbort_ReturnsNil verifies that when requestAbortCtx is cancelled
// by a sibling model error (via requestAbortFn), processModel returns nil rather than errShutdown.
//
// P1c regression: Before the fix, the drain-switch default checked ctx.Err() (which is
// requestAbortCtx), so a sibling abort looked like a pod shutdown and returned errShutdown.
// executeJob would then route the job to re-enqueue instead of failed-with-partial, breaking
// retry safety for batches with partial results. After the fix, the default checks mainCtx
// (the main processor context), which is only cancelled on SIGTERM.
func TestProcessModel_SiblingAbort_ReturnsNil(t *testing.T) {
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
	writers := &outputWriters{output: bufio.NewWriter(&buf), errors: bufio.NewWriter(&buf)}

	progress := &executionProgress{
		total:   1,
		updater: env.updater,
		jobID:   jobInfo.JobID,
	}

	// mainCtx is not cancelled — only requestAbortCtx is, simulating a sibling model calling
	// requestAbortFn() on error. SLO and user-cancel signals are both absent.
	mainCtx := testLoggerCtx(t)
	requestAbortCtx, requestAbortFn := context.WithCancel(mainCtx)
	requestAbortFn() // simulate sibling model calling requestAbortFn

	err := env.p.processModel(requestAbortCtx, mainCtx, mainCtx, context.Background(), inputFile, plansDir, "m1", "m1", writers, progress, nil)
	// requestAbortCtx cancelled, but no SLO / user-cancel / SIGTERM → nil, not errShutdown
	if err != nil {
		t.Fatalf("expected nil when only requestAbortCtx is cancelled (sibling abort), got: %v", err)
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

	ctx := testLoggerCtx(t)
	counts, err := env.p.executeJob(ctx, ctx, ctx, ctx, &jobExecutionParams{
		updater: env.updater,
		jobInfo: jobInfo,
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

	// Verify each custom_id appears exactly once — guards against duplicates.
	seenIDs := make(map[string]int)
	for _, line := range outputLines {
		var entry outputLine
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal output line: %v", err)
		}
		seenIDs[entry.CustomID]++
	}
	for _, wantID := range []string{"r1", "r2"} {
		if seenIDs[wantID] != 1 {
			t.Errorf("custom_id %q appeared %d times, want 1", wantID, seenIDs[wantID])
		}
	}

	// Error file should be empty (all requests succeeded).
	errorPath, _ := env.p.jobErrorFilePath(jobInfo.JobID, jobInfo.TenantID)
	errBytes, err := os.ReadFile(errorPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read error file: %v", err)
	}
	if len(bytes.TrimSpace(errBytes)) > 0 {
		t.Fatalf("expected empty error file, got: %s", errBytes)
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

	ctx := testLoggerCtx(t)
	counts, err := env.p.executeJob(ctx, ctx, ctx, ctx, &jobExecutionParams{
		updater: env.updater,
		jobInfo: jobInfo,
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

	// Verify each custom_id appears exactly once and all 4 are present.
	outputPath, _ := env.p.jobOutputFilePath(jobInfo.JobID, jobInfo.TenantID)
	outBytes, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	outputLines := bytes.Split(bytes.TrimSpace(outBytes), []byte{'\n'})
	if len(outputLines) != 4 {
		t.Fatalf("output lines = %d, want 4", len(outputLines))
	}
	seenIDs := make(map[string]int)
	for _, line := range outputLines {
		var entry outputLine
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal output line: %v", err)
		}
		seenIDs[entry.CustomID]++
	}
	for _, wantID := range []string{"a", "b", "c", "d"} {
		if seenIDs[wantID] != 1 {
			t.Errorf("custom_id %q appeared %d times, want 1", wantID, seenIDs[wantID])
		}
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

	ctx, cancel := context.WithCancel(testLoggerCtx(t))
	cancel()

	_, err := env.p.executeJob(ctx, ctx, ctx, ctx, &jobExecutionParams{
		updater: env.updater,
		jobInfo: jobInfo,
	})
	if err == nil {
		t.Fatalf("expected error on cancelled context")
	}
	// All four contexts are the same cancelled context. sloCtx.Err() == context.Canceled
	// (not DeadlineExceeded), so the SLO branch is skipped. userCancelCtx.Err() != nil,
	// so the drain switch routes to errCancelled.
	if !errors.Is(err, errCancelled) {
		t.Fatalf("expected errCancelled, got: %v", err)
	}
}

// TestExecuteJob_UserCancelFlag verifies that when only userCancelCtx is cancelled
// (ctx, sloCtx, and requestAbortCtx all remain alive), executeJob returns errCancelled.
// This enforces the separation contract: userCancelCtx is an independent signal derived
// from context.Background() in production. requestAbortCtx must NOT be cancelled here —
// the dispatch loop runs to completion, the mock returns a normal response, and the
// post-execution switch detects userCancelCtx.Err() to produce errCancelled.
// If requestAbortCtx were also cancelled, the test would pass even if the two contexts
// were conflated, hiding regressions.
func TestExecuteJob_UserCancelFlag(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	userCancelCtx, cancelFn := context.WithCancel(context.Background())

	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			cancelFn()
			return &inference.GenerateResponse{RequestID: "srv", Response: []byte(`{"ok":true}`)}, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	ctx := testLoggerCtx(t)

	counts, err := env.p.executeJob(ctx, ctx, userCancelCtx, ctx, &jobExecutionParams{
		updater: env.updater,
		jobInfo: jobInfo,
	})
	if !errors.Is(err, errCancelled) {
		t.Fatalf("expected errCancelled, got: %v", err)
	}
	if counts == nil || counts.Total != 1 {
		t.Fatalf("expected counts with Total=1, got %+v", counts)
	}
	if counts.Failed != 1 {
		t.Fatalf("expected Failed=1 (cancel overwrites completed response as batch_cancelled), got %+v", counts)
	}
}

// TestExecuteJob_CancelAfterAllRequestsComplete verifies that if userCancelCtx is cancelled
// after all requests have already been dispatched and completed successfully (i.e. context
// cancellation never interrupted dispatch), executeJob still returns errCancelled rather than
// nil, preventing the job from being finalized as "completed".
func TestExecuteJob_CancelAfterAllRequestsComplete(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	userCancelCtx, abortFn := context.WithCancel(context.Background())

	// The mock cancels userCancelCtx after the inference call returns, simulating
	// the race where the cancel event arrives while (or just after) the last request completes.
	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			abortFn()
			return &inference.GenerateResponse{RequestID: "srv", Response: []byte(`{"ok":true}`)}, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	ctx := testLoggerCtx(t)
	_, err := env.p.executeJob(ctx, ctx, userCancelCtx, ctx, &jobExecutionParams{
		updater: env.updater,
		jobInfo: jobInfo,
	})
	if !errors.Is(err, errCancelled) {
		t.Fatalf("expected errCancelled when cancel arrives after all requests complete, got: %v", err)
	}
}

// TestExecuteJob_SIGTERMAfterAllComplete verifies that when all requests finish successfully
// and SIGTERM arrives before executeJob returns, the function returns nil (not errShutdown).
// This ensures the caller proceeds to finalizeJob (which uses a detached context) rather than
// re-enqueueing a fully-complete job.
func TestExecuteJob_SIGTERMAfterAllComplete(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	// ctx (processor/main context) is cancelled after the inference call returns,
	// simulating SIGTERM arriving just after the last request completes.
	mainCtx, mainCancel := context.WithCancel(testLoggerCtx(t))
	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			mainCancel()
			return &inference.GenerateResponse{RequestID: "srv", Response: []byte(`{"ok":true}`)}, nil
		},
	}

	requests := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	userCancelCtx := context.Background()
	counts, err := env.p.executeJob(mainCtx, mainCtx, userCancelCtx, mainCtx, &jobExecutionParams{
		updater: env.updater,
		jobInfo: jobInfo,
	})
	if err != nil {
		t.Fatalf("expected nil error when SIGTERM arrives after all requests complete, got: %v", err)
	}
	if counts == nil {
		t.Fatal("expected non-nil counts")
	}
	if counts.Total != 1 || counts.Completed != 1 {
		t.Fatalf("counts = {Total:%d, Completed:%d, Failed:%d}, want {1,1,0}",
			counts.Total, counts.Completed, counts.Failed)
	}
}

// TestExecuteJob_AbortCtxCancel_AbortsInflightRequests verifies that a user cancel aborts
// in-flight inference requests. The test calls both userCancelFn() (user-cancel signal) and
// requestAbortFn() (stops dispatch), mirroring watchCancel's production behavior. The mock
// blocks until requestAbortCtx cancellation propagates to its ctx argument. executeJob must
// return errCancelled with the in-flight request counted as failed.
func TestExecuteJob_AbortCtxCancel_AbortsInflightRequests(t *testing.T) {
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

	ctx := testLoggerCtx(t)
	// userCancelCtx must be background-derived (user-cancel-only signal, no parent propagation).
	userCancelCtx, userCancelFn := context.WithCancel(context.Background())
	// requestAbortCtx is pre-set in params before the goroutine starts, matching the production
	// flow where runJob sets requestAbortFn before starting watchCancel.
	requestAbortCtx, requestAbortFn := context.WithCancel(ctx)

	params := &jobExecutionParams{
		updater:        env.updater,
		jobInfo:        jobInfo,
		requestAbortFn: requestAbortFn,
	}
	type result struct {
		counts *openai.BatchRequestCounts
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		counts, err := env.p.executeJob(ctx, ctx, userCancelCtx, requestAbortCtx, params)
		resCh <- result{counts, err}
	}()

	<-inferStarted
	// Simulate watchCancel: set userCancelCtx (user-cancel signal) and stop dispatch via requestAbortFn.
	userCancelFn()
	requestAbortFn()

	select {
	case res := <-resCh:
		if !errors.Is(res.err, errCancelled) {
			t.Fatalf("expected errCancelled, got: %v", res.err)
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
		t.Fatal("executeJob did not return within 5s after userCancelCtx cancellation")
	}
}

// TestExecuteJob_SLOExpiredBeforeDispatch verifies that when the SLO deadline has already
// passed before execution begins, executeJob returns errExpired immediately with the total
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

	ctx := testLoggerCtx(t)
	// SLO deadline already in the past: early check fires before any files are opened.
	sloCtx, cancel := context.WithDeadline(ctx, time.Now().Add(-1*time.Second))
	defer cancel()

	counts, err := env.p.executeJob(ctx, sloCtx, context.Background(), sloCtx, &jobExecutionParams{
		updater: env.updater,
		jobInfo: jobInfo,
	})
	if !errors.Is(err, errExpired) {
		t.Fatalf("expected errExpired, got: %v", err)
	}
	if counts == nil {
		t.Fatal("expected non-nil counts")
		return
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
// returns errExpired with accurate partial counts.
//
// Context hierarchy for SLO expiry:
//
//	processorCtx → sloCtx (WithDeadline) → requestAbortCtx (WithCancel)
//	                        DeadlineExceeded        Canceled (propagated)
//
//	userCancelCtx (WithCancel, derived from context.Background — NOT in the chain above)
//
// requestAbortCtx sees Canceled to stop dispatch;
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

	ctx := testLoggerCtx(t)
	// Use context.WithDeadline so sloCtx.Err() returns DeadlineExceeded (matching real code).
	sloCtx, sloCancel := context.WithDeadline(ctx, time.Now().Add(100*time.Millisecond))
	defer sloCancel()

	type result struct {
		counts *openai.BatchRequestCounts
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		counts, err := env.p.executeJob(ctx, sloCtx, context.Background(), sloCtx, &jobExecutionParams{
			updater: env.updater,
			jobInfo: jobInfo,
		})
		resCh <- result{counts, err}
	}()

	select {
	case res := <-resCh:
		if !errors.Is(res.err, errExpired) {
			t.Fatalf("expected errExpired, got: %v", res.err)
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
			if entry.Error == nil || entry.Error.Code != string(batch_types.ErrCodeBatchExpired) {
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

	ctx := testLoggerCtx(t)
	err := env.p.finalizeJob(ctx, context.Background(), env.updater, dbJob, jobInfo, counts)
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
	if statusInfo.OutputFileID == nil {
		t.Fatalf("expected OutputFileID to be set")
	}
}

// TestFinalizeJob_UploadFailure verifies that when all uploads fail, finalizeJob marks the
// job as failed (not completed) and returns errFinalizeFailedOver. The job must not be
// marked completed with missing artifacts — that would violate the batch contract.
func TestFinalizeJob_UploadFailure(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

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

	ctx := testLoggerCtx(t)
	err := env.p.finalizeJob(ctx, context.Background(), env.updater, dbJob, jobInfo, counts)
	if !errors.Is(err, errFinalizeFailedOver) {
		t.Fatalf("expected errFinalizeFailedOver, got: %v", err)
	}

	items, _, _, getErr := env.dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, true, 0, 1)
	if getErr != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", getErr, len(items))
	}
	var got openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Status != openai.BatchStatusFailed {
		t.Fatalf("status = %s, want failed (upload failure must not produce completed)", got.Status)
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

	ctx := testLoggerCtx(t)
	counts, err := env.p.executeJob(ctx, ctx, ctx, ctx, &jobExecutionParams{
		updater: env.updater,
		jobInfo: jobInfo,
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

// TestExecuteJob_HTTPErrorGoesToOutputFile verifies that HTTP error responses (4xx/5xx)
// are written to output.jsonl (not error.jsonl) with the response field populated,
// while non-HTTP errors go to error.jsonl with the error field populated.
// This matches the OpenAI batch spec: error file is for non-HTTP failures only.
func TestExecuteJob_HTTPErrorGoesToOutputFile(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	var callCount atomic.Int32
	mock := &mockInferenceClient{
		generateFn: func(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			n := callCount.Add(1)
			switch n {
			case 1:
				// success
				return &inference.GenerateResponse{RequestID: "srv-1", Response: []byte(`{"ok":true}`)}, nil
			case 2:
				// HTTP 422 error — should go to output file
				return nil, &inference.ClientError{
					Category:     httpclient.ErrCategoryInvalidReq,
					Message:      "HTTP 422: Invalid model",
					StatusCode:   422,
					ResponseBody: []byte(`{"error":{"message":"Invalid model","type":"invalid_request_error","code":"model_not_found"}}`),
				}
			default:
				// non-HTTP error — should go to error file
				return nil, &inference.ClientError{
					Category: httpclient.ErrCategoryServer,
					Message:  "connection refused",
				}
			}
		},
	}

	requests := []batch_types.Request{
		{CustomID: "r1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "r2", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "r3", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	env, jobInfo := setupExecutionJob(t, cfg, mock, requests, map[string]string{"m1": "m1"})

	ctx := testLoggerCtx(t)
	counts, err := env.p.executeJob(ctx, ctx, ctx, ctx, &jobExecutionParams{
		updater: env.updater,
		jobInfo: jobInfo,
	})
	if err != nil {
		t.Fatalf("executeJob error: %v", err)
	}
	// Only the 200 response counts as completed; HTTP 422 and non-HTTP error are both failures.
	if counts.Completed != 1 {
		t.Fatalf("Completed = %d, want 1", counts.Completed)
	}
	if counts.Failed != 2 {
		t.Fatalf("Failed = %d, want 2", counts.Failed)
	}

	// output.jsonl should contain 2 lines: the 200 success AND the HTTP 422 error.
	outputPath, _ := env.p.jobOutputFilePath(jobInfo.JobID, jobInfo.TenantID)
	outputLines := readNonEmptyJSONLLines(t, outputPath)
	if len(outputLines) != 2 {
		t.Fatalf("output.jsonl lines = %d, want 2", len(outputLines))
	}

	// Verify both output lines: one success (200) and one HTTP error (422).
	var found200, found422 bool
	for _, line := range outputLines {
		var entry outputLine
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal output line: %v", err)
		}
		if entry.Error != nil {
			t.Fatalf("output line should not have error field, got: %+v", entry.Error)
		}
		if entry.Response == nil {
			t.Fatalf("output line should have response field")
		}
		switch entry.Response.StatusCode {
		case 200:
			found200 = true
		case 422:
			found422 = true
			errObj, ok := entry.Response.Body["error"].(map[string]interface{})
			if !ok {
				t.Fatalf("HTTP error response body should contain error object, got: %v", entry.Response.Body)
			}
			if errObj["code"] != "model_not_found" {
				t.Fatalf("expected error code 'model_not_found', got: %v", errObj["code"])
			}
		default:
			t.Fatalf("unexpected status code %d in output file", entry.Response.StatusCode)
		}
	}
	if !found200 || !found422 {
		t.Fatalf("expected both 200 and 422 in output file, found200=%v found422=%v", found200, found422)
	}

	// error.jsonl should contain 1 line: the non-HTTP error only.
	errorPath, _ := env.p.jobErrorFilePath(jobInfo.JobID, jobInfo.TenantID)
	errorLines := readNonEmptyJSONLLines(t, errorPath)
	if len(errorLines) != 1 {
		t.Fatalf("error.jsonl lines = %d, want 1", len(errorLines))
	}
	var errEntry outputLine
	if err := json.Unmarshal(errorLines[0], &errEntry); err != nil {
		t.Fatalf("unmarshal error line: %v", err)
	}
	if errEntry.Error == nil {
		t.Fatalf("error file line should have error field")
	}
	if errEntry.Response != nil {
		t.Fatalf("error file line should not have response field")
	}
	if errEntry.Error.Code != string(httpclient.ErrCategoryServer) {
		t.Fatalf("error code = %q, want %q", errEntry.Error.Code, httpclient.ErrCategoryServer)
	}
}

// TestFinalizeJob_EmptyOutputFile_OutputFileIDOmitted verifies that when the output
// file is empty (all requests failed), output_file_id is omitted per the OpenAI spec.
func TestFinalizeJob_EmptyOutputFile_OutputFileIDOmitted(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DefaultOutputExpirationSeconds = 86400

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

	ctx := testLoggerCtx(t)
	if err := env.p.finalizeJob(ctx, context.Background(), env.updater, dbJob, jobInfo, counts); err != nil {
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
	if statusInfo.OutputFileID != nil {
		t.Errorf("OutputFileID = %q, want nil (output file was empty)", *statusInfo.OutputFileID)
	}
	if statusInfo.ErrorFileID == nil {
		t.Errorf("ErrorFileID should be set when error file has content")
	}
}

// TestFinalizeJob_EmptyErrorFile_ErrorFileIDOmitted verifies that when the error
// file is empty (no requests failed), error_file_id is omitted per the OpenAI spec.
func TestFinalizeJob_EmptyErrorFile_ErrorFileIDOmitted(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DefaultOutputExpirationSeconds = 86400

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

	ctx := testLoggerCtx(t)
	if err := env.p.finalizeJob(ctx, context.Background(), env.updater, dbJob, jobInfo, counts); err != nil {
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
	if statusInfo.OutputFileID == nil {
		t.Errorf("OutputFileID should be set when output file has content")
	}
	if statusInfo.ErrorFileID != nil {
		t.Errorf("ErrorFileID = %q, want nil (error file was empty)", *statusInfo.ErrorFileID)
	}
}

// =====================================================================
// Tests: handleJobError (routing branches)
// =====================================================================

func TestHandleJobError_errCancelled(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	dbJob := seedDBJob(t, env.dbClient, "job-cancel")
	ji := &batch_types.JobInfo{
		JobID:    "job-cancel",
		BatchJob: &openai.Batch{BatchSpec: openai.BatchSpec{CreatedAt: time.Now().Add(-10 * time.Second).Unix()}},
	}

	before := gatherHistogramSampleCount(t, "batch_job_e2e_latency_seconds", map[string]string{"status": "cancelled"})

	ctx := testLoggerCtx(t)
	env.p.handleJobError(ctx, &jobExecutionParams{
		updater: env.updater,
		jobItem: dbJob,
		jobInfo: ji,
	}, errCancelled)

	after := gatherHistogramSampleCount(t, "batch_job_e2e_latency_seconds", map[string]string{"status": "cancelled"})
	if delta := after - before; delta != 1 {
		t.Fatalf("E2E latency cancelled: delta=%d, want 1", delta)
	}

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

func TestHandleJobError_Shutdown_ReEnqueues(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	dbJob := seedDBJob(t, env.dbClient, "job-ctx")
	task := &db.BatchJobPriority{ID: "job-ctx"}
	ji := &batch_types.JobInfo{
		JobID:    "job-ctx",
		BatchJob: &openai.Batch{BatchSpec: openai.BatchSpec{CreatedAt: time.Now().Add(-10 * time.Second).Unix()}},
	}

	beforeFailed := gatherHistogramSampleCount(t, "batch_job_e2e_latency_seconds", map[string]string{"status": "failed"})

	ctx := testLoggerCtx(t)
	env.p.handleJobError(ctx, &jobExecutionParams{
		updater: env.updater,
		jobItem: dbJob,
		task:    task,
		jobInfo: ji,
	}, errShutdown)

	afterFailed := gatherHistogramSampleCount(t, "batch_job_e2e_latency_seconds", map[string]string{"status": "failed"})
	if delta := afterFailed - beforeFailed; delta != 0 {
		t.Fatalf("E2E latency failed: delta=%d, want 0 (re-enqueue succeeded, not terminal)", delta)
	}

	tasks, err := env.pqClient.PQDequeue(ctx, 0, 10)
	if err != nil {
		t.Fatalf("PQDequeue: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatalf("expected re-enqueued task, got none")
	}
}

func TestHandleJobError_Shutdown_NilTask(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	dbJob := seedDBJob(t, env.dbClient, "job-ctx-nil")

	ctx := testLoggerCtx(t)
	// task is nil — should not panic, and job status should remain unchanged
	env.p.handleJobError(ctx, &jobExecutionParams{
		updater: env.updater,
		jobItem: dbJob,
	}, errShutdown)

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
	ji := &batch_types.JobInfo{
		JobID:    "job-fail",
		BatchJob: &openai.Batch{BatchSpec: openai.BatchSpec{CreatedAt: time.Now().Add(-10 * time.Second).Unix()}},
	}

	before := gatherHistogramSampleCount(t, "batch_job_e2e_latency_seconds", map[string]string{"status": "failed"})

	ctx := testLoggerCtx(t)
	env.p.handleJobError(ctx, &jobExecutionParams{
		updater: env.updater,
		jobItem: dbJob,
		jobInfo: ji,
	}, errors.New("some error"))

	after := gatherHistogramSampleCount(t, "batch_job_e2e_latency_seconds", map[string]string{"status": "failed"})
	if delta := after - before; delta != 1 {
		t.Fatalf("E2E latency failed: delta=%d, want 1", delta)
	}

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

// TestHandleJobError_ExpiredWithCancelledCtx_StillTransitionsExpired verifies that
// handleExpired completes the DB status write even when the parent context is already
// cancelled (e.g. SIGTERM arrived concurrently with SLO expiry). handleExpired must use
// a detached context for the UpdateExpiredStatus call so that SIGTERM does not abort
// the DB write after file uploads succeed.
func TestHandleJobError_ExpiredWithCancelledCtx_StillTransitionsExpired(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	jobID := "job-expired-sigterm"
	dbJob := seedDBJob(t, env.dbClient, jobID)
	ji := &batch_types.JobInfo{
		JobID:    jobID,
		TenantID: dbJob.TenantID,
		BatchJob: &openai.Batch{BatchSpec: openai.BatchSpec{CreatedAt: time.Now().Add(-10 * time.Second).Unix()}},
	}
	counts := &openai.BatchRequestCounts{Total: 10, Completed: 5, Failed: 5}

	// Simulate SIGTERM: parent ctx is already cancelled.
	cancelledCtx, cancel := context.WithCancel(testLoggerCtx(t))
	cancel()

	env.p.handleJobError(cancelledCtx, &jobExecutionParams{
		updater:       env.updater,
		jobItem:       dbJob,
		jobInfo:       ji,
		requestCounts: counts,
	}, errExpired)

	items, _, _, err := env.dbClient.DBGet(context.Background(), &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	var got openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &got); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if got.Status != openai.BatchStatusExpired {
		t.Fatalf("status = %s, want expired (detached context must survive SIGTERM)", got.Status)
	}
	if got.RequestCounts.Total != 10 || got.RequestCounts.Completed != 5 {
		t.Fatalf("request_counts = %+v, want {10,5,5}", got.RequestCounts)
	}
}

// TestHandleJobError_ExpiredDuringIngestion_NilCountsTransitionsExpired verifies that
// handleJobError routes errExpired with nil requestCounts (SLO expired during preprocessing,
// before executeJob ran) to handleExpired, which tolerates nil counts and transitions the
// job to expired status.
func TestHandleJobError_ExpiredDuringIngestion_NilCountsTransitionsExpired(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	dbJob := seedDBJob(t, env.dbClient, "job-expired-ingestion")
	ji := &batch_types.JobInfo{
		JobID:    "job-expired-ingestion",
		BatchJob: &openai.Batch{BatchSpec: openai.BatchSpec{CreatedAt: time.Now().Add(-10 * time.Second).Unix()}},
	}

	ctx := testLoggerCtx(t)
	env.p.handleJobError(ctx, &jobExecutionParams{
		updater:       env.updater,
		jobItem:       dbJob,
		jobInfo:       ji,
		requestCounts: nil, // nil: SLO expired before executeJob ran
	}, errExpired)

	items, _, _, err := env.dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{"job-expired-ingestion"}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	var got openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &got); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if got.Status != openai.BatchStatusExpired {
		t.Fatalf("status = %s, want %s", got.Status, openai.BatchStatusExpired)
	}
}

// =====================================================================
// Tests: handleCancelled / handleFailed
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

	jobInfo := &batch_types.JobInfo{
		JobID:    jobID,
		TenantID: tenantID,
		BatchJob: &openai.Batch{BatchSpec: openai.BatchSpec{CreatedAt: time.Now().Add(-10 * time.Second).Unix()}},
	}
	counts := &openai.BatchRequestCounts{Total: 5, Completed: 3, Failed: 2}

	before := gatherHistogramSampleCount(t, "batch_job_e2e_latency_seconds", map[string]string{"status": "cancelled"})

	ctx := testLoggerCtx(t)
	if err := env.p.handleCancelled(ctx, &jobExecutionParams{
		updater:       env.updater,
		jobItem:       dbJob,
		jobInfo:       jobInfo,
		requestCounts: counts,
	}); err != nil {
		t.Fatalf("handleCancelled: %v", err)
	}

	after := gatherHistogramSampleCount(t, "batch_job_e2e_latency_seconds", map[string]string{"status": "cancelled"})
	if delta := after - before; delta != 1 {
		t.Fatalf("E2E latency cancelled: delta=%d, want 1", delta)
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
	if got.OutputFileID == nil {
		t.Fatal("expected output_file_id to be set")
	}
	if got.ErrorFileID == nil {
		t.Fatal("expected error_file_id to be set")
	}
}

// TestHandleCancelled_CancelledWriteFails_FallsBackToFailed verifies that when
// UpdateCancelledStatus fails inside handleCancelled, the fallback writes "failed"
// status with file IDs preserved. This mirrors the failover pattern in finalizeJob.
func TestHandleCancelled_CancelledWriteFails_FallsBackToFailed(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	innerDB := newMockBatchDBClient()
	failDB := &failOnStatusDB{
		inner:      innerDB,
		failStatus: openai.BatchStatusCancelled,
		failErr:    errors.New("injected: cancelled write failed"),
	}
	statusClient := mockdb.NewMockBatchStatusClient()

	jobID := "job-cancel-failover"
	tenantID := "tenant__tenantA"

	dbJob := seedDBJob(t, innerDB, jobID)
	dbJob.TenantID = tenantID

	clients := &clientset.Clientset{
		BatchDB:   failDB,
		FileDB:    newMockFileDBClient(),
		File:      mockfiles.NewMockBatchFilesClient(t.TempDir()),
		Status:    statusClient,
		Queue:     mockdb.NewMockBatchPriorityQueueClient(),
		Event:     mockdb.NewMockBatchEventChannelClient(),
		Inference: inference.NewSingleClientResolver(&mockInferenceClient{}),
	}
	p := mustNewProcessor(t, cfg, clients)
	p.poller = NewPoller(clients.Queue, failDB)
	updater := NewStatusUpdater(failDB, statusClient, 86400)

	createPartialOutputFiles(t, p, jobID, tenantID)

	jobInfo := &batch_types.JobInfo{
		JobID:    jobID,
		TenantID: tenantID,
		BatchJob: &openai.Batch{BatchSpec: openai.BatchSpec{CreatedAt: time.Now().Add(-10 * time.Second).Unix()}},
	}
	counts := &openai.BatchRequestCounts{Total: 5, Completed: 3, Failed: 2}

	ctx := testLoggerCtx(t)
	err := p.handleCancelled(ctx, &jobExecutionParams{
		updater:       updater,
		jobItem:       dbJob,
		jobInfo:       jobInfo,
		requestCounts: counts,
	})

	if !errors.Is(err, errFinalizeFailedOver) {
		t.Fatalf("expected errFinalizeFailedOver, got: %v", err)
	}

	items, _, _, getErr := innerDB.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, true, 0, 1)
	if getErr != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", getErr, len(items))
	}
	var got openai.BatchStatusInfo
	if unmarshalErr := json.Unmarshal(items[0].Status, &got); unmarshalErr != nil {
		t.Fatalf("unmarshal: %v", unmarshalErr)
	}
	if got.Status != openai.BatchStatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	if got.OutputFileID == nil {
		t.Fatal("output_file_id must be preserved in fallback, got nil")
	}
	if got.RequestCounts.Total != 5 || got.RequestCounts.Completed != 3 || got.RequestCounts.Failed != 2 {
		t.Fatalf("request_counts = %+v, want {5,3,2}", got.RequestCounts)
	}
}

func TestHandleFailed_Execution_UploadsPartialOutput(t *testing.T) {
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

	ctx := testLoggerCtx(t)
	if err := env.p.handleFailed(ctx, env.updater, dbJob, counts, jobInfo); err != nil {
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
	if got.RequestCounts.Total != 10 || got.RequestCounts.Completed != 7 || got.RequestCounts.Failed != 3 {
		t.Fatalf("request_counts = %+v, want {10,7,3}", got.RequestCounts)
	}
	if got.OutputFileID == nil {
		t.Fatal("expected output_file_id to be set")
	}
	if got.ErrorFileID == nil {
		t.Fatal("expected error_file_id to be set")
	}
}

func TestHandleFailed_Finalization_RecordsCountsOnly(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	jobID := "job-fail-finalization"
	dbJob := seedDBJob(t, env.dbClient, jobID)
	ji := &batch_types.JobInfo{
		JobID:    jobID,
		BatchJob: &openai.Batch{BatchSpec: openai.BatchSpec{CreatedAt: time.Now().Add(-10 * time.Second).Unix()}},
	}

	counts := &openai.BatchRequestCounts{Total: 8, Completed: 8, Failed: 0}

	before := gatherHistogramSampleCount(t, "batch_job_e2e_latency_seconds", map[string]string{"status": "failed"})

	ctx := testLoggerCtx(t)
	if err := env.p.handleFailed(ctx, env.updater, dbJob, counts, ji); err != nil {
		t.Fatalf("handleFailed: %v", err)
	}

	after := gatherHistogramSampleCount(t, "batch_job_e2e_latency_seconds", map[string]string{"status": "failed"})
	if delta := after - before; delta != 1 {
		t.Fatalf("E2E latency failed: delta=%d, want 1", delta)
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
	if got.OutputFileID != nil {
		t.Fatalf("expected nil output_file_id, got %s", *got.OutputFileID)
	}
	if got.ErrorFileID != nil {
		t.Fatalf("expected nil error_file_id, got %s", *got.ErrorFileID)
	}
}

// TestHandleFailed_CancelledCtx_StillWritesDB verifies that handleFailed completes
// the DB status write even when the parent context is already cancelled.
func TestHandleFailed_CancelledCtx_StillWritesDB(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	env := newTestProcessorEnv(t, cfg, &mockInferenceClient{})

	jobID := "job-failed-sigterm"
	dbJob := seedDBJob(t, env.dbClient, jobID)
	counts := &openai.BatchRequestCounts{Total: 3, Completed: 1, Failed: 2}

	cancelledCtx, cancel := context.WithCancel(testLoggerCtx(t))
	cancel()

	if err := env.p.handleFailed(cancelledCtx, env.updater, dbJob, counts, nil); err != nil {
		t.Fatalf("handleFailed with cancelled ctx: %v", err)
	}

	items, _, _, err := env.dbClient.DBGet(context.Background(), &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, true, 0, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("DBGet: err=%v len=%d", err, len(items))
	}
	var got openai.BatchStatusInfo
	if err := json.Unmarshal(items[0].Status, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Status != openai.BatchStatusFailed {
		t.Fatalf("status = %s, want failed (detached context must survive cancelled parent)", got.Status)
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

	ctx := testLoggerCtx(t)
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

	ctx := testLoggerCtx(t)
	outputFileID, errorFileID := env.p.uploadPartialResults(ctx, jobInfo, dbJob)

	if outputFileID != "" {
		t.Fatalf("outputFileID = %q, want empty (output file does not exist)", outputFileID)
	}
	if errorFileID != "" {
		t.Fatalf("errorFileID = %q, want empty (error file does not exist)", errorFileID)
	}
}

// TestUploadPartialResults_OneUploadFails_OtherSurvives verifies that when one of the two
// parallel uploads fails, the other file ID is still returned. uploadPartialResults is
// best-effort: one-side failure must not lose the other side's reference.
func TestUploadPartialResults_OneUploadFails_OtherSurvives(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()

	dbClient := newMockBatchDBClient()
	statusClient := mockdb.NewMockBatchStatusClient()
	filesClient := &failOnNthCallClient{
		failN:   1,
		failErr: errors.New("injected: one-side upload failure"),
	}

	clients := &clientset.Clientset{
		BatchDB:   dbClient,
		FileDB:    newMockFileDBClient(),
		File:      filesClient,
		Status:    statusClient,
		Queue:     mockdb.NewMockBatchPriorityQueueClient(),
		Inference: inference.NewSingleClientResolver(&fakeInferenceClient{}),
	}
	p := mustNewProcessor(t, cfg, clients)
	p.poller = NewPoller(clients.Queue, dbClient)

	jobID := "partial-one-side"
	tenantID := "tenant__tenantA"

	jobDir, err := p.jobRootDir(jobID, tenantID)
	if err != nil {
		t.Fatalf("jobRootDir: %v", err)
	}
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	outputPath, _ := p.jobOutputFilePath(jobID, tenantID)
	if err := os.WriteFile(outputPath, []byte(`{"custom_id":"r1"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile output: %v", err)
	}
	errorPath, _ := p.jobErrorFilePath(jobID, tenantID)
	if err := os.WriteFile(errorPath, []byte(`{"custom_id":"e1"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	jobInfo := &batch_types.JobInfo{JobID: jobID, TenantID: tenantID}
	dbJob := seedDBJob(t, dbClient, jobID)

	ctx := testLoggerCtx(t)
	outputFileID, errorFileID := p.uploadPartialResults(ctx, jobInfo, dbJob)

	// Exactly one of the two uploads failed (the first call to Store).
	// The other must have succeeded and returned a non-empty file ID.
	nonEmpty := 0
	if outputFileID != "" {
		nonEmpty++
	}
	if errorFileID != "" {
		nonEmpty++
	}
	if nonEmpty != 1 {
		t.Fatalf("expected exactly 1 surviving file ID (one upload failed), got output=%q error=%q",
			outputFileID, errorFileID)
	}
}

// =====================================================================
// Tests: cleanupJobArtifacts
// =====================================================================

func TestCleanupJobArtifacts_RemovesDirectory(t *testing.T) {
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	p := mustNewProcessor(t, cfg, validProcessorClients(t))

	jobDir, _ := p.jobRootDir("cleanup-job", "tenant-1")
	if err := os.MkdirAll(filepath.Join(jobDir, "plans"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "input.jsonl"), []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx := testLoggerCtx(t)
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

	ctx := testLoggerCtx(t)
	err := p.storeFileRecord(ctx, "file_x", "output.jsonl", "tenant-1", 100, db.Tags{})
	if err == nil {
		t.Fatalf("expected error from DB failure")
	}
}

// countingStatusClient wraps a status client and counts StatusSet calls.
type countingStatusClient struct {
	db.BatchStatusClient
	count atomic.Int32
}

func (c *countingStatusClient) StatusSet(ctx context.Context, ID string, TTL int, data []byte) error {
	c.count.Add(1)
	return c.BatchStatusClient.StatusSet(ctx, ID, TTL, data)
}

func TestExecutionProgress_Throttle(t *testing.T) {
	orig := progressUpdateInterval
	progressUpdateInterval = 50 * time.Millisecond
	t.Cleanup(func() { progressUpdateInterval = orig })

	statusClient := &countingStatusClient{BatchStatusClient: mockdb.NewMockBatchStatusClient()}
	updater := NewStatusUpdater(newMockBatchDBClient(), statusClient, 86400)

	progress := &executionProgress{
		total:   100,
		updater: updater,
		jobID:   "job-throttle",
	}

	ctx := testLoggerCtx(t)

	// Record 100 requests as fast as possible — most should be throttled.
	for i := 0; i < 100; i++ {
		progress.record(ctx, true)
	}

	throttled := statusClient.count.Load()
	if throttled >= 100 {
		t.Fatalf("expected throttled updates < 100, got %d (no throttling occurred)", throttled)
	}
	if throttled == 0 {
		t.Fatalf("expected at least 1 Redis update, got 0")
	}
	t.Logf("100 requests produced %d Redis updates (throttled)", throttled)
}

func TestExecutionProgress_Flush(t *testing.T) {
	orig := progressUpdateInterval
	progressUpdateInterval = time.Hour // effectively disable throttled updates
	t.Cleanup(func() { progressUpdateInterval = orig })

	statusClient := &countingStatusClient{BatchStatusClient: mockdb.NewMockBatchStatusClient()}
	updater := NewStatusUpdater(newMockBatchDBClient(), statusClient, 86400)

	progress := &executionProgress{
		total:   10,
		updater: updater,
		jobID:   "job-flush",
	}

	ctx := testLoggerCtx(t)

	// Record some requests — all should be throttled (interval=1h).
	for i := 0; i < 10; i++ {
		progress.record(ctx, true)
	}

	beforeFlush := statusClient.count.Load()

	// flush should push unconditionally.
	progress.flush(ctx)

	afterFlush := statusClient.count.Load()
	if afterFlush <= beforeFlush {
		t.Fatalf("expected flush to push at least 1 update, before=%d after=%d", beforeFlush, afterFlush)
	}
}

// =====================================================================
// Tests: jsonNumericToFloat64
// =====================================================================

// findMetric finds a metric by name and label set from the default Prometheus registry.
// Returns nil if not found.
func findMetric(t *testing.T, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	outer:
		for _, m := range mf.Metric {
			for k, v := range labels {
				var got string
				for _, lp := range m.Label {
					if lp.GetName() == k {
						got = lp.GetValue()
						break
					}
				}
				if got != v {
					continue outer
				}
			}
			return m
		}
	}
	return nil
}

func gatherCounterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	m := findMetric(t, name, labels)
	if m == nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

func gatherHistogramSampleCount(t *testing.T, name string, labels map[string]string) uint64 {
	t.Helper()
	m := findMetric(t, name, labels)
	if m == nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}

func TestRecordTokenUsageFromBody(t *testing.T) {
	logger := logr.Discard()

	t.Run("usage present with both fields", func(t *testing.T) {
		model := "token-test-both"
		body := map[string]interface{}{
			"choices": []interface{}{},
			"usage": map[string]interface{}{
				"prompt_tokens":     float64(42),
				"completion_tokens": float64(128),
				"total_tokens":      float64(170),
			},
		}
		recordTokenUsageFromBody(body, model, logger)

		if v := gatherCounterValue(t, "batch_request_prompt_tokens_total", map[string]string{"model": model}); v != 42 {
			t.Fatalf("prompt_tokens=%v, want 42", v)
		}
		if v := gatherCounterValue(t, "batch_request_generation_tokens_total", map[string]string{"model": model}); v != 128 {
			t.Fatalf("generation_tokens=%v, want 128", v)
		}
	})

	t.Run("usage missing", func(t *testing.T) {
		model := "token-test-missing"
		body := map[string]interface{}{
			"choices": []interface{}{},
		}
		recordTokenUsageFromBody(body, model, logger)

		if v := gatherCounterValue(t, "batch_request_prompt_tokens_total", map[string]string{"model": model}); v != 0 {
			t.Fatalf("prompt_tokens=%v, want 0 (no usage object)", v)
		}
	})

	t.Run("usage present but no numeric fields", func(t *testing.T) {
		model := "token-test-non-numeric"
		body := map[string]interface{}{
			"usage": map[string]interface{}{
				"prompt_tokens": "not-a-number",
			},
		}
		recordTokenUsageFromBody(body, model, logger)

		if v := gatherCounterValue(t, "batch_request_prompt_tokens_total", map[string]string{"model": model}); v != 0 {
			t.Fatalf("prompt_tokens=%v, want 0 (non-numeric field)", v)
		}
	})

	t.Run("usage with only prompt_tokens", func(t *testing.T) {
		model := "token-test-prompt-only"
		body := map[string]interface{}{
			"usage": map[string]interface{}{
				"prompt_tokens": float64(100),
			},
		}
		recordTokenUsageFromBody(body, model, logger)

		if v := gatherCounterValue(t, "batch_request_prompt_tokens_total", map[string]string{"model": model}); v != 100 {
			t.Fatalf("prompt_tokens=%v, want 100", v)
		}
		if v := gatherCounterValue(t, "batch_request_generation_tokens_total", map[string]string{"model": model}); v != 0 {
			t.Fatalf("generation_tokens=%v, want 0 (only prompt provided)", v)
		}
	})

	t.Run("usage with only completion_tokens", func(t *testing.T) {
		model := "token-test-completion-only"
		body := map[string]interface{}{
			"usage": map[string]interface{}{
				"completion_tokens": float64(50),
			},
		}
		recordTokenUsageFromBody(body, model, logger)

		if v := gatherCounterValue(t, "batch_request_prompt_tokens_total", map[string]string{"model": model}); v != 0 {
			t.Fatalf("prompt_tokens=%v, want 0 (only completion provided)", v)
		}
		if v := gatherCounterValue(t, "batch_request_generation_tokens_total", map[string]string{"model": model}); v != 50 {
			t.Fatalf("generation_tokens=%v, want 50", v)
		}
	})

	t.Run("nil body", func(t *testing.T) {
		model := "token-test-nil"
		recordTokenUsageFromBody(nil, model, logger)

		if v := gatherCounterValue(t, "batch_request_prompt_tokens_total", map[string]string{"model": model}); v != 0 {
			t.Fatalf("prompt_tokens=%v, want 0 (nil body)", v)
		}
	})

	t.Run("negative token values skipped", func(t *testing.T) {
		model := "token-test-negative"
		body := map[string]interface{}{
			"usage": map[string]interface{}{
				"prompt_tokens":     float64(-10),
				"completion_tokens": float64(50),
			},
		}
		recordTokenUsageFromBody(body, model, logger)

		if v := gatherCounterValue(t, "batch_request_prompt_tokens_total", map[string]string{"model": model}); v != 0 {
			t.Fatalf("prompt_tokens=%v, want 0 (negative values should be skipped)", v)
		}
		if v := gatherCounterValue(t, "batch_request_generation_tokens_total", map[string]string{"model": model}); v != 0 {
			t.Fatalf("generation_tokens=%v, want 0 (negative values should be skipped)", v)
		}
	})
}

func TestRecordE2ELatency(t *testing.T) {
	t.Run("nil jobInfo", func(t *testing.T) {
		recordE2ELatency(nil, metrics.E2EStatusCompleted)
	})

	t.Run("nil BatchJob", func(t *testing.T) {
		ji := &batch_types.JobInfo{JobID: "j1"}
		recordE2ELatency(ji, metrics.E2EStatusCompleted)
	})

	t.Run("zero CreatedAt", func(t *testing.T) {
		ji := &batch_types.JobInfo{
			JobID:    "j1",
			BatchJob: &openai.Batch{BatchSpec: openai.BatchSpec{CreatedAt: 0}},
		}
		recordE2ELatency(ji, metrics.E2EStatusCompleted)
	})

	t.Run("valid CreatedAt", func(t *testing.T) {
		ji := &batch_types.JobInfo{
			JobID:    "j1",
			BatchJob: &openai.Batch{BatchSpec: openai.BatchSpec{CreatedAt: time.Now().Add(-30 * time.Second).Unix()}},
		}
		recordE2ELatency(ji, metrics.E2EStatusCompleted)
	})
}

func TestJsonNumericToFloat64(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want float64
		ok   bool
	}{
		{"float64", float64(42.5), 42.5, true},
		{"int", int(10), 10, true},
		{"int64", int64(999), 999, true},
		{"json.Number", json.Number("128"), 128, true},
		{"string", "nope", 0, false},
		{"nil", nil, 0, false},
		{"bool", true, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := jsonNumericToFloat64(tc.in)
			if ok != tc.ok {
				t.Fatalf("jsonNumericToFloat64(%v) ok=%v, want %v", tc.in, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("jsonNumericToFloat64(%v)=%v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// =====================================================================
// Tests: mergeInferenceHeaders
// =====================================================================

func TestMergeInferenceHeaders(t *testing.T) {
	t.Run("no deadline no objective leaves headers unchanged", func(t *testing.T) {
		if got := mergeInferenceHeaders(nil, context.Background(), ""); got != nil {
			t.Fatalf("nil headers: got %v, want nil", got)
		}
		in := map[string]string{"a": "b"}
		got := mergeInferenceHeaders(in, context.Background(), "")
		if len(got) != 1 {
			t.Fatalf("expected no new keys, got len=%d %#v", len(got), got)
		}
		if _, ok := got[sloTTFTMSHeader]; ok {
			t.Fatalf("unexpected %s without deadline", sloTTFTMSHeader)
		}
		if got["a"] != "b" {
			t.Fatal("lost existing header")
		}
	})

	t.Run("deadline remaining milliseconds", func(t *testing.T) {
		want := 5*time.Second + 123*time.Millisecond
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(want))
		defer cancel()
		h := mergeInferenceHeaders(nil, ctx, "")
		got, err := strconv.ParseInt(h[sloTTFTMSHeader], 10, 64)
		if err != nil {
			t.Fatalf("parse header: %v", err)
		}
		const slackMs int64 = 150
		hi := want.Milliseconds()
		lo := hi - slackMs
		if got < lo || got > hi {
			t.Fatalf("x-slo-ttft-ms = %d, want in [%d, %d]", got, lo, hi)
		}
	})

	t.Run("deadline in the past leaves headers unchanged", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		if got := mergeInferenceHeaders(nil, ctx, ""); got != nil {
			t.Fatalf("nil headers: got %v, want nil (expired deadline => no merge)", got)
		}
		in := map[string]string{"a": "b"}
		got := mergeInferenceHeaders(in, ctx, "")
		if len(got) != 1 {
			t.Fatalf("expected no new keys, got len=%d %#v", len(got), got)
		}
		if _, ok := got[sloTTFTMSHeader]; ok {
			t.Fatalf("unexpected %s with expired deadline", sloTTFTMSHeader)
		}
		if got["a"] != "b" {
			t.Fatal("lost existing header")
		}
	})

	t.Run("preserves existing headers", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
		defer cancel()
		h := mergeInferenceHeaders(map[string]string{"a": "b"}, ctx, "")
		if h["a"] != "b" {
			t.Fatal("lost existing header")
		}
		got, err := strconv.ParseInt(h[sloTTFTMSHeader], 10, 64)
		if err != nil || got <= 0 {
			t.Fatalf("x-slo-ttft-ms = %q, want positive ms", h[sloTTFTMSHeader])
		}
	})

	t.Run("non-nil empty map mutated in place", func(t *testing.T) {
		want := 2 * time.Second
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(want))
		defer cancel()
		in := map[string]string{}
		h := mergeInferenceHeaders(in, ctx, "")
		got, err := strconv.ParseInt(in[sloTTFTMSHeader], 10, 64)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		const slackMs int64 = 250
		hi := want.Milliseconds()
		lo := hi - slackMs
		if got < lo || got > hi {
			t.Fatalf("in-place map x-slo-ttft-ms = %d, want in [%d, %d]", got, lo, hi)
		}
		if h[sloTTFTMSHeader] != in[sloTTFTMSHeader] {
			t.Fatalf("returned map header %q != in-place %q", h[sloTTFTMSHeader], in[sloTTFTMSHeader])
		}
	})

	t.Run("objective header sent when configured", func(t *testing.T) {
		h := mergeInferenceHeaders(nil, context.Background(), "batch-low-priority")
		if h[inferenceObjectiveHeader] != "batch-low-priority" {
			t.Fatalf("got %q, want %q", h[inferenceObjectiveHeader], "batch-low-priority")
		}
		if _, ok := h[sloTTFTMSHeader]; ok {
			t.Fatal("SLO header should not be set without deadline")
		}
	})

	t.Run("objective header not sent when empty", func(t *testing.T) {
		h := mergeInferenceHeaders(map[string]string{"a": "b"}, context.Background(), "")
		if _, ok := h[inferenceObjectiveHeader]; ok {
			t.Fatal("objective header should not be set when empty")
		}
	})

	t.Run("both SLO and objective headers", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(10*time.Second))
		defer cancel()
		h := mergeInferenceHeaders(nil, ctx, "batch-low-priority")
		if _, ok := h[sloTTFTMSHeader]; !ok {
			t.Fatal("SLO header missing")
		}
		if h[inferenceObjectiveHeader] != "batch-low-priority" {
			t.Fatalf("objective header: got %q, want %q", h[inferenceObjectiveHeader], "batch-low-priority")
		}
	})

	t.Run("objective only with expired deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		h := mergeInferenceHeaders(nil, ctx, "batch-low-priority")
		if _, ok := h[sloTTFTMSHeader]; ok {
			t.Fatal("SLO header should not be set with expired deadline")
		}
		if h[inferenceObjectiveHeader] != "batch-low-priority" {
			t.Fatalf("objective header: got %q, want %q", h[inferenceObjectiveHeader], "batch-low-priority")
		}
	})
}
