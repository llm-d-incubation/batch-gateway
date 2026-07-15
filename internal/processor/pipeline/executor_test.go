package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr"

	batch_types "github.com/llm-d/llm-d-batch-gateway/internal/shared/types"
	httpclient "github.com/llm-d/llm-d-batch-gateway/pkg/clients/http"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

type mockInferenceClient struct {
	response []byte
}

func (m *mockInferenceClient) Generate(_ context.Context, req *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
	return &inference.GenerateResponse{
		RequestID: req.RequestID,
		Response:  m.response,
	}, nil
}

type sliceSource struct {
	items []RequestItem
}

func (s *sliceSource) Produce(_ context.Context, out chan<- RequestItem) error {
	defer close(out)
	for _, item := range s.items {
		out <- item
	}
	return nil
}

func TestJobExecutorEndToEnd(t *testing.T) {
	body := map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10.0, "completion_tokens": 5.0}}
	respBytes, _ := json.Marshal(body)

	client := &mockInferenceClient{response: respBytes}
	resolver := inference.NewSingleClientResolver(client)
	defer func() { _ = resolver.Close() }()

	items := []RequestItem{
		{RequestID: "req-1", CustomID: "c-1", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-2", CustomID: "c-2", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-3", CustomID: "c-3", ModelID: "m1", Endpoint: "/v1/chat/completions"},
	}

	outputFile := tempFile(t)
	errorFile := tempFile(t)
	pending := NewPendingRequests(0)
	tracker := NewProgressTracker(int64(len(items)), nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	executor := NewJobExecutor(JobExecutorConfig{
		Source:     &sliceSource{items: items},
		Dispatcher: newTestSyncDispatcher(resolver),
		Collector:  collector,
		Tracker:    tracker,
		Logger:     logr.Discard(),
	})

	counts, err := executor.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	if counts.Total != 3 {
		t.Errorf("Total = %d, want 3", counts.Total)
	}
	if counts.Completed != 3 {
		t.Errorf("Completed = %d, want 3", counts.Completed)
	}
	if counts.Failed != 0 {
		t.Errorf("Failed = %d, want 0", counts.Failed)
	}

	outputData := readFile(t, outputFile)
	lines := bytes.Split(bytes.TrimSpace(outputData), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("output lines = %d, want 3", len(lines))
	}

	for i, line := range lines {
		var out outputLine
		if err := json.Unmarshal(line, &out); err != nil {
			t.Fatalf("line %d: unmarshal error: %v", i, err)
		}
		if out.Response == nil {
			t.Errorf("line %d: response is nil", i)
		} else if out.Response.StatusCode != 200 {
			t.Errorf("line %d: status = %d, want 200", i, out.Response.StatusCode)
		}
	}

	errorData := readFile(t, errorFile)
	if len(bytes.TrimSpace(errorData)) != 0 {
		t.Errorf("error output = %q, want empty", errorData)
	}
}

func TestJobExecutorWithErrors(t *testing.T) {
	resolver := inference.NewPerModelClientResolver(map[string]inference.InferenceClient{
		"m1": &mockInferenceClient{response: []byte(`{}`)},
	})
	defer func() { _ = resolver.Close() }()

	items := []RequestItem{
		{RequestID: "req-1", CustomID: "c-1", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-bad", CustomID: "c-bad", ModelID: "no-such-model", Endpoint: "/v1/chat/completions"},
	}

	outputFile := tempFile(t)
	errorFile := tempFile(t)
	pending := NewPendingRequests(0)
	tracker := NewProgressTracker(int64(len(items)), nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	executor := NewJobExecutor(JobExecutorConfig{
		Source:     &sliceSource{items: items},
		Dispatcher: newTestSyncDispatcher(resolver),
		Collector:  collector,
		Tracker:    tracker,
		Logger:     logr.Discard(),
	})

	counts, err := executor.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	if counts.Completed+counts.Failed != int64(len(items)) {
		t.Errorf("Completed(%d) + Failed(%d) != Total(%d)", counts.Completed, counts.Failed, counts.Total)
	}

	errorData := readFile(t, errorFile)
	if len(bytes.TrimSpace(errorData)) == 0 {
		t.Error("expected error output for unknown model")
	}

	var errLine outputLine
	if err := json.Unmarshal(bytes.TrimSpace(errorData), &errLine); err != nil {
		t.Fatalf("unmarshal error line: %v", err)
	}
	if errLine.Error == nil {
		t.Fatal("expected error field in error output")
	}
	if errLine.Error.Code != inference.ErrCodeModelNotFound {
		t.Errorf("error code = %q, want %q", errLine.Error.Code, inference.ErrCodeModelNotFound)
	}
}

func TestJobExecutorCancellation(t *testing.T) {
	resolver := inference.NewSingleClientResolver(&mockInferenceClient{response: []byte(`{}`)})
	defer func() { _ = resolver.Close() }()

	ctx, cancel := context.WithCancel(context.Background())

	source := &cancellingSource{
		items:    []RequestItem{{RequestID: "req-1", CustomID: "c-1", ModelID: "m1"}},
		cancelFn: cancel,
	}

	outputFile := tempFile(t)
	errorFile := tempFile(t)
	pending := NewPendingRequests(0)
	tracker := NewProgressTracker(10, nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	executor := NewJobExecutor(JobExecutorConfig{
		Source:     source,
		Dispatcher: newTestSyncDispatcher(resolver),
		Collector:  collector,
		Tracker:    tracker,
		Logger:     logr.Discard(),
	})

	_, err := executor.Execute(ctx)
	if err != nil && err != context.Canceled {
		t.Fatalf("Execute() error: %v", err)
	}
}

// cancellingSource sends one item then cancels the context.
type cancellingSource struct {
	items    []RequestItem
	cancelFn context.CancelFunc
}

func (s *cancellingSource) Produce(_ context.Context, out chan<- RequestItem) error {
	defer close(out)
	for _, item := range s.items {
		out <- item
	}
	s.cancelFn()
	return nil
}

func TestJobExecutorMultipleModels(t *testing.T) {
	body := map[string]any{"ok": true}
	respBytes, _ := json.Marshal(body)

	resolver := inference.NewPerModelClientResolver(map[string]inference.InferenceClient{
		"m1": &mockInferenceClient{response: respBytes},
		"m2": &mockInferenceClient{response: respBytes},
	})
	defer func() { _ = resolver.Close() }()

	items := []RequestItem{
		{RequestID: "req-1", CustomID: "a", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-2", CustomID: "b", ModelID: "m2", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-3", CustomID: "c", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-4", CustomID: "d", ModelID: "m2", Endpoint: "/v1/chat/completions"},
	}

	outputFile := tempFile(t)
	errorFile := tempFile(t)
	pending := NewPendingRequests(0)
	tracker := NewProgressTracker(int64(len(items)), nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	executor := NewJobExecutor(JobExecutorConfig{
		Source:     &sliceSource{items: items},
		Dispatcher: newTestSyncDispatcher(resolver),
		Collector:  collector,
		Tracker:    tracker,
		Logger:     logr.Discard(),
	})

	counts, err := executor.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if counts.Total != 4 {
		t.Fatalf("Total = %d, want 4", counts.Total)
	}
	if counts.Completed != 4 {
		t.Fatalf("Completed = %d, want 4", counts.Completed)
	}

	outputData := readFile(t, outputFile)
	lines := bytes.Split(bytes.TrimSpace(outputData), []byte("\n"))
	if len(lines) != 4 {
		t.Fatalf("output lines = %d, want 4", len(lines))
	}

	seenIDs := make(map[string]int)
	for _, line := range lines {
		var out outputLine
		if err := json.Unmarshal(line, &out); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		seenIDs[out.CustomID]++
	}
	for _, wantID := range []string{"a", "b", "c", "d"} {
		if seenIDs[wantID] != 1 {
			t.Errorf("custom_id %q appeared %d times, want 1", wantID, seenIDs[wantID])
		}
	}

	errorData := readFile(t, errorFile)
	if len(bytes.TrimSpace(errorData)) != 0 {
		t.Errorf("expected empty error file, got %q", errorData)
	}
}

func TestJobExecutorMultipleModels_ModelNotFound(t *testing.T) {
	resolver := inference.NewPerModelClientResolver(map[string]inference.InferenceClient{
		"m1": &mockInferenceClient{response: []byte(`{"ok":true}`)},
	})
	defer func() { _ = resolver.Close() }()

	items := []RequestItem{
		{RequestID: "req-1", CustomID: "a", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-2", CustomID: "b", ModelID: "no-such-model", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-3", CustomID: "c", ModelID: "m1", Endpoint: "/v1/chat/completions"},
	}

	outputFile := tempFile(t)
	errorFile := tempFile(t)
	pending := NewPendingRequests(0)
	tracker := NewProgressTracker(int64(len(items)), nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	executor := NewJobExecutor(JobExecutorConfig{
		Source:     &sliceSource{items: items},
		Dispatcher: newTestSyncDispatcher(resolver),
		Collector:  collector,
		Tracker:    tracker,
		Logger:     logr.Discard(),
	})

	counts, err := executor.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	if counts.Total != 3 {
		t.Fatalf("Total = %d, want 3", counts.Total)
	}
	if counts.Completed != 2 {
		t.Fatalf("Completed = %d, want 2 (model-not-found for one shouldn't affect others)", counts.Completed)
	}
	if counts.Failed != 1 {
		t.Fatalf("Failed = %d, want 1", counts.Failed)
	}

	outputData := readFile(t, outputFile)
	outputLines := splitLines(outputData)
	if len(outputLines) != 2 {
		t.Fatalf("output lines = %d, want 2", len(outputLines))
	}

	errorData := readFile(t, errorFile)
	errorLines := splitLines(errorData)
	if len(errorLines) != 1 {
		t.Fatalf("error lines = %d, want 1", len(errorLines))
	}

	var errLine outputLine
	if err := json.Unmarshal(errorLines[0], &errLine); err != nil {
		t.Fatalf("unmarshal error line: %v", err)
	}
	if errLine.Error == nil || errLine.Error.Code != inference.ErrCodeModelNotFound {
		t.Fatalf("expected model_not_found error, got %+v", errLine.Error)
	}
}

func TestJobExecutorSeparatesSuccessAndErrors(t *testing.T) {
	var callCount int
	client := &mockInferenceClientForE2E{
		generateFn: func(_ context.Context, req *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			callCount++
			if callCount%2 == 1 {
				return &inference.GenerateResponse{RequestID: req.RequestID, Response: []byte(`{"ok":true}`)}, nil
			}
			return nil, &inference.ClientError{Category: httpclient.ErrCategoryServer, Message: "mock error"}
		},
	}
	resolver := inference.NewSingleClientResolver(client)
	defer func() { _ = resolver.Close() }()

	items := []RequestItem{
		{RequestID: "req-1", CustomID: "r1", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-2", CustomID: "r2", ModelID: "m1", Endpoint: "/v1/chat/completions"},
	}

	outputFile := tempFile(t)
	errorFile := tempFile(t)
	pending := NewPendingRequests(0)
	tracker := NewProgressTracker(int64(len(items)), nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	executor := NewJobExecutor(JobExecutorConfig{
		Source:     &sliceSource{items: items},
		Dispatcher: newTestSyncDispatcher(resolver),
		Collector:  collector,
		Tracker:    tracker,
		Logger:     logr.Discard(),
	})

	counts, err := executor.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if counts.Completed != 1 || counts.Failed != 1 {
		t.Fatalf("counts: completed=%d failed=%d, want completed=1 failed=1", counts.Completed, counts.Failed)
	}

	outputData := readFile(t, outputFile)
	outputLines := splitLines(outputData)
	if len(outputLines) != 1 {
		t.Fatalf("output lines = %d, want 1", len(outputLines))
	}
	var outLine outputLine
	if err := json.Unmarshal(outputLines[0], &outLine); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if outLine.Response == nil || outLine.Error != nil {
		t.Fatalf("output: want response set and error nil, got response=%v error=%v", outLine.Response, outLine.Error)
	}

	errorData := readFile(t, errorFile)
	errorLines := splitLines(errorData)
	if len(errorLines) != 1 {
		t.Fatalf("error lines = %d, want 1", len(errorLines))
	}
	var errLine outputLine
	if err := json.Unmarshal(errorLines[0], &errLine); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if errLine.Error == nil || errLine.Response != nil {
		t.Fatalf("error: want error set and response nil, got response=%v error=%v", errLine.Response, errLine.Error)
	}
}

func TestJobExecutorHTTPErrorGoesToOutputFile(t *testing.T) {
	var callCount int
	client := &mockInferenceClientForE2E{
		generateFn: func(_ context.Context, req *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
			callCount++
			switch callCount {
			case 1:
				return &inference.GenerateResponse{RequestID: req.RequestID, Response: []byte(`{"ok":true}`)}, nil
			case 2:
				return nil, &inference.ClientError{
					Category:     httpclient.ErrCategoryInvalidReq,
					Message:      "HTTP 422: Invalid model",
					StatusCode:   422,
					ResponseBody: []byte(`{"error":{"message":"Invalid model","type":"invalid_request_error","code":"model_not_found"}}`),
				}
			default:
				return nil, &inference.ClientError{
					Category: httpclient.ErrCategoryServer,
					Message:  "connection refused",
				}
			}
		},
	}
	resolver := inference.NewSingleClientResolver(client)
	defer func() { _ = resolver.Close() }()

	items := []RequestItem{
		{RequestID: "req-1", CustomID: "r1", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-2", CustomID: "r2", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-3", CustomID: "r3", ModelID: "m1", Endpoint: "/v1/chat/completions"},
	}

	outputFile := tempFile(t)
	errorFile := tempFile(t)
	pending := NewPendingRequests(0)
	tracker := NewProgressTracker(int64(len(items)), nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	executor := NewJobExecutor(JobExecutorConfig{
		Source:     &sliceSource{items: items},
		Dispatcher: newTestSyncDispatcher(resolver),
		Collector:  collector,
		Tracker:    tracker,
		Logger:     logr.Discard(),
	})

	counts, err := executor.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if counts.Completed != 1 {
		t.Fatalf("Completed = %d, want 1", counts.Completed)
	}
	if counts.Failed != 2 {
		t.Fatalf("Failed = %d, want 2", counts.Failed)
	}

	// output.jsonl should have 2 lines: 200 success + HTTP 422
	outputData := readFile(t, outputFile)
	outputLines := splitLines(outputData)
	if len(outputLines) != 2 {
		t.Fatalf("output lines = %d, want 2", len(outputLines))
	}

	var found200, found422 bool
	for _, line := range outputLines {
		var entry outputLine
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal output: %v", err)
		}
		if entry.Error != nil {
			t.Fatalf("output line should not have error field, got %+v", entry.Error)
		}
		if entry.Response == nil {
			t.Fatal("output line should have response field")
		}
		switch entry.Response.StatusCode {
		case 200:
			found200 = true
		case 422:
			found422 = true
			errObj, ok := entry.Response.Body["error"].(map[string]any)
			if !ok {
				t.Fatalf("HTTP error body should contain error object, got %v", entry.Response.Body)
			}
			if errObj["code"] != "model_not_found" {
				t.Fatalf("expected error code 'model_not_found', got %v", errObj["code"])
			}
		default:
			t.Fatalf("unexpected status code %d", entry.Response.StatusCode)
		}
	}
	if !found200 || !found422 {
		t.Fatalf("expected both 200 and 422 in output, found200=%v found422=%v", found200, found422)
	}

	// error.jsonl should have 1 line: non-HTTP error only
	errorData := readFile(t, errorFile)
	errorLines := splitLines(errorData)
	if len(errorLines) != 1 {
		t.Fatalf("error lines = %d, want 1", len(errorLines))
	}
	var errEntry outputLine
	if err := json.Unmarshal(errorLines[0], &errEntry); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if errEntry.Error == nil {
		t.Fatal("error file line should have error field")
	}
	if errEntry.Response != nil {
		t.Fatal("error file line should not have response field")
	}
	if errEntry.Error.Code != string(httpclient.ErrCategoryServer) {
		t.Fatalf("error code = %q, want %q", errEntry.Error.Code, httpclient.ErrCategoryServer)
	}
}

type mockInferenceClientForE2E struct {
	generateFn func(context.Context, *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError)
}

func (m *mockInferenceClientForE2E) Generate(ctx context.Context, req *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
	return m.generateFn(ctx, req)
}

// TestJobExecutorCancellation_AllRequestsAccountedFor verifies that when the
// context is cancelled mid-execution, completed + failed == total. Every
// request must appear in either the output or error file.
func TestJobExecutorCancellation_AllRequestsAccountedFor(t *testing.T) {
	const total = 10
	body, _ := json.Marshal(map[string]any{"ok": true})
	client := &mockInferenceClient{response: body}
	resolver := inference.NewSingleClientResolver(client)
	defer func() { _ = resolver.Close() }()

	items := makeItems(total, "m1")

	// Source sends one item then cancels ctx, but keeps producing
	// remaining items (matching fixed PlanFileSource behavior).
	ctx, cancel := context.WithCancel(context.Background())
	source := &cancelAfterNSource{items: items, cancelAt: 1, cancelFn: cancel}

	outputFile := tempFile(t)
	errorFile := tempFile(t)
	pending := NewPendingRequests(0)
	tracker := NewProgressTracker(int64(total), nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	executor := NewJobExecutor(JobExecutorConfig{
		Source:     source,
		Dispatcher: newTestSyncDispatcher(resolver),
		Collector:  collector,
		Tracker:    tracker,
		Logger:     logr.Discard(),
	})

	_, err := executor.Execute(ctx)
	if err != nil && err != context.Canceled {
		t.Fatalf("Execute() error: %v", err)
	}

	counts := tracker.Counts()
	accounted := counts.Completed + counts.Failed
	if accounted != int64(total) {
		t.Fatalf("Completed(%d) + Failed(%d) = %d, want %d: %d requests were silently dropped",
			counts.Completed, counts.Failed, accounted, total, int64(total)-accounted)
	}
}

// TestCancelCode_SLOExpiry verifies that cancelCode returns batch_expired
// when the context cancellation was caused by a deadline.
func TestCancelCode_SLOExpiry(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	<-ctx.Done()

	code, _ := cancelCode(ctx)
	if code != string(batch_types.ErrCodeBatchExpired) {
		t.Fatalf("cancelCode() = %q, want %q", code, batch_types.ErrCodeBatchExpired)
	}
}

// TestCancelCode_UserCancel verifies that cancelCode returns batch_cancelled
// when the context was cancelled (not deadline).
func TestCancelCode_UserCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code, _ := cancelCode(ctx)
	if code != string(batch_types.ErrCodeBatchCancelled) {
		t.Fatalf("cancelCode() = %q, want %q", code, batch_types.ErrCodeBatchCancelled)
	}
}

// cancelAfterNSource sends cancelAt items, cancels ctx, then keeps producing
// the rest. This models the fixed PlanFileSource behavior where the source
// continues producing all items regardless of cancellation.
type cancelAfterNSource struct {
	items    []RequestItem
	cancelAt int
	cancelFn context.CancelFunc
}

func (s *cancelAfterNSource) Produce(_ context.Context, out chan<- RequestItem) error {
	defer close(out)
	for i, item := range s.items {
		if i == s.cancelAt {
			s.cancelFn()
		}
		out <- item
	}
	return nil
}

var _ inference.InferenceClient = (*mockInferenceClient)(nil)
var _ inference.InferenceClient = (*mockInferenceClientForE2E)(nil)
var _ RequestSource = (*sliceSource)(nil)
var _ RequestSource = (*cancelAfterNSource)(nil)
var _ RequestDispatcher = (*testSyncDispatcher)(nil)

// testSyncDispatcher is a minimal synchronous dispatcher for executor tests.
// It calls Generate inline (no concurrency) and converts the result.
type testSyncDispatcher struct {
	resolver *inference.GatewayResolver
}

func newTestSyncDispatcher(resolver *inference.GatewayResolver) *testSyncDispatcher {
	return &testSyncDispatcher{resolver: resolver}
}

func (d *testSyncDispatcher) Run(_ context.Context, requestCh <-chan RequestItem, resultCh chan<- ResultItem) error {
	for msg := range requestCh {
		client := d.resolver.ClientFor(msg.ModelID)
		if client == nil {
			resultCh <- *msg.ModelNotFound()
			continue
		}
		req := &inference.GenerateRequest{
			RequestID: msg.RequestID,
			Endpoint:  msg.Endpoint,
			Params:    msg.Body,
			Headers:   msg.Headers,
		}
		resp, clientErr := client.Generate(context.Background(), req)
		result := ResultItem{RequestID: msg.RequestID, CustomID: msg.CustomID, ModelID: msg.ModelID}
		switch {
		case clientErr != nil && clientErr.StatusCode > 0:
			body := make(map[string]any)
			if len(clientErr.ResponseBody) > 0 {
				_ = json.Unmarshal(clientErr.ResponseBody, &body)
			}
			result.Response = &batch_types.ResponseData{StatusCode: clientErr.StatusCode, RequestID: msg.RequestID, Body: body}
		case clientErr != nil:
			result.Error = &OutputError{Code: string(clientErr.Category), Message: clientErr.Message}
		case resp == nil:
			result.Error = &OutputError{Code: string(httpclient.ErrCategoryServer), Message: "nil response"}
		default:
			var body map[string]any
			if len(resp.Response) > 0 {
				_ = json.Unmarshal(resp.Response, &body)
			}
			result.Response = &batch_types.ResponseData{StatusCode: 200, RequestID: resp.RequestID, Body: body}
		}
		resultCh <- result
	}
	close(resultCh)
	return nil
}
