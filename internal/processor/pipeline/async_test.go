package pipeline

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

type fakeAsyncClient struct {
	mu        sync.Mutex
	submitted []*inference.GenerateRequest
	results   chan *inference.GenerateResponse
}

func newFakeAsyncClient() *fakeAsyncClient {
	return &fakeAsyncClient{
		results: make(chan *inference.GenerateResponse, 64),
	}
}

func (c *fakeAsyncClient) Submit(_ context.Context, req *inference.GenerateRequest) *inference.ClientError {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.submitted = append(c.submitted, req)
	return nil
}

func (c *fakeAsyncClient) GetResult(ctx context.Context) (*inference.GenerateResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-c.results:
		return r, nil
	}
}

func (c *fakeAsyncClient) Cancel(_ context.Context) error { return nil }
func (c *fakeAsyncClient) Close() error                    { return nil }

func (c *fakeAsyncClient) deliver(requestID string, body map[string]any) {
	resp, _ := json.Marshal(body)
	c.results <- &inference.GenerateResponse{
		RequestID: requestID,
		Response:  resp,
	}
}

func TestAsyncEndToEnd(t *testing.T) {
	// Shared client — same instance for submit (via ClientFor) and
	// collect (via SharedClientFor → broadcaster)
	client := newFakeAsyncClient()
	resolver := inference.NewTestAsyncResolver(map[string]func() inference.AsyncInferenceClient{
		"m1": func() inference.AsyncInferenceClient { return client },
	})
	defer func() { _ = resolver.Close() }()

	// Broadcaster backed by the shared client
	broadcaster := NewResultBroadcaster(client, logr.Discard())
	broadcasterCtx, broadcasterCancel := context.WithCancel(context.Background())
	defer broadcasterCancel()
	go broadcaster.Run(broadcasterCtx)

	items := []RequestItem{
		{RequestID: "req-1", CustomID: "c-1", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-2", CustomID: "c-2", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-3", CustomID: "c-3", ModelID: "m1", Endpoint: "/v1/chat/completions"},
	}

	pending := NewPendingRequests()
	outputFile := tempFile(t)
	errorFile := tempFile(t)
	tracker := NewProgressTracker(int64(len(items)), nil, "test-job", logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	dispatcher := NewAsyncDispatcher(resolver,
		map[string]*ResultBroadcaster{"m1": broadcaster},
		pending, logr.Discard())

	executor := NewJobExecutor(JobExecutorConfig{
		Source:     &sliceSource{items: items},
		Dispatcher: dispatcher,
		Collector:  collector,
		Tracker:    tracker,
		Logger:     logr.Discard(),
	})

	// Deliver results asynchronously after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		for _, item := range items {
			client.deliver(item.RequestID, map[string]any{"ok": true})
		}
	}()

	counts, err := executor.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	if counts.Completed != 3 {
		t.Errorf("Completed = %d, want 3", counts.Completed)
	}
	if counts.Failed != 0 {
		t.Errorf("Failed = %d, want 0", counts.Failed)
	}

	outputData := readFile(t, outputFile)
	lines := countLines(outputData)
	if lines != 3 {
		t.Errorf("output lines = %d, want 3", lines)
	}
}

func TestAsyncResult_NilResponseBody(t *testing.T) {
	resp := &inference.GenerateResponse{
		RequestID: "req-nil-body",
		Response:  nil,
	}
	result := asyncResult(resp, logr.Discard())
	if result.Error == nil {
		t.Fatal("expected error for nil response body")
	}
	if result.Error.Code != "server_error" {
		t.Fatalf("error code = %q, want %q", result.Error.Code, "server_error")
	}
	if result.Response != nil {
		t.Fatalf("expected nil response, got %+v", result.Response)
	}
}

func TestAsyncResult_BadJSONBody(t *testing.T) {
	resp := &inference.GenerateResponse{
		RequestID: "req-bad-json",
		Response:  []byte(`{not valid json`),
	}
	result := asyncResult(resp, logr.Discard())
	if result.Error == nil {
		t.Fatal("expected error for bad JSON body")
	}
	if result.Error.Code != "parse_error" {
		t.Fatalf("error code = %q, want %q", result.Error.Code, "parse_error")
	}
}

func TestAsyncResult_Success(t *testing.T) {
	resp := &inference.GenerateResponse{
		RequestID: "req-ok",
		Response:  []byte(`{"choices":[]}`),
	}
	result := asyncResult(resp, logr.Discard())
	if result.Error != nil {
		t.Fatalf("unexpected error: %+v", result.Error)
	}
	if result.Response == nil {
		t.Fatal("expected response")
	}
	if result.Response.StatusCode != 200 {
		t.Fatalf("StatusCode = %d, want 200", result.Response.StatusCode)
	}
	if result.Response.RequestID != "req-ok" {
		t.Fatalf("RequestID = %q, want %q", result.Response.RequestID, "req-ok")
	}
}

func TestAsyncDispatcher_ModelNotFound(t *testing.T) {
	client := newFakeAsyncClient()
	resolver := inference.NewTestAsyncResolver(map[string]func() inference.AsyncInferenceClient{
		"m1": func() inference.AsyncInferenceClient { return client },
	})
	defer func() { _ = resolver.Close() }()

	broadcaster := NewResultBroadcaster(client, logr.Discard())
	broadcasterCtx, broadcasterCancel := context.WithCancel(context.Background())
	defer broadcasterCancel()
	go broadcaster.Run(broadcasterCtx)

	items := []RequestItem{
		{RequestID: "req-1", CustomID: "c-1", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-2", CustomID: "c-2", ModelID: "no-such-model", Endpoint: "/v1/chat/completions"},
	}

	pending := NewPendingRequests()
	outputFile := tempFile(t)
	errorFile := tempFile(t)
	tracker := NewProgressTracker(int64(len(items)), nil, "test-job", logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	dispatcher := NewAsyncDispatcher(resolver,
		map[string]*ResultBroadcaster{"m1": broadcaster},
		pending, logr.Discard())

	executor := NewJobExecutor(JobExecutorConfig{
		Source:     &sliceSource{items: items},
		Dispatcher: dispatcher,
		Collector:  collector,
		Tracker:    tracker,
		Logger:     logr.Discard(),
	})

	go func() {
		time.Sleep(50 * time.Millisecond)
		client.deliver("req-1", map[string]any{"ok": true})
	}()

	counts, err := executor.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	if counts.Completed != 1 {
		t.Errorf("Completed = %d, want 1", counts.Completed)
	}
	if counts.Failed != 1 {
		t.Errorf("Failed = %d, want 1", counts.Failed)
	}

	errorData := readFile(t, errorFile)
	errorLines := splitLines(errorData)
	if len(errorLines) != 1 {
		t.Fatalf("error lines = %d, want 1", len(errorLines))
	}
	var errLine outputLine
	if err := json.Unmarshal(errorLines[0], &errLine); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if errLine.Error == nil || errLine.Error.Code != "model_not_found" {
		t.Fatalf("expected model_not_found, got %+v", errLine.Error)
	}
}

func TestAsyncCancellation(t *testing.T) {
	client := newFakeAsyncClient()
	resolver := inference.NewTestAsyncResolver(map[string]func() inference.AsyncInferenceClient{
		"m1": func() inference.AsyncInferenceClient { return client },
	})
	defer func() { _ = resolver.Close() }()

	broadcaster := NewResultBroadcaster(client, logr.Discard())
	broadcasterCtx, broadcasterCancel := context.WithCancel(context.Background())
	defer broadcasterCancel()
	go broadcaster.Run(broadcasterCtx)

	items := makeItems(10, "m1")

	pending := NewPendingRequests()
	outputFile := tempFile(t)
	errorFile := tempFile(t)
	tracker := NewProgressTracker(int64(len(items)), nil, "test-job", logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	dispatcher := NewAsyncDispatcher(resolver,
		map[string]*ResultBroadcaster{"m1": broadcaster},
		pending, logr.Discard())

	executor := NewJobExecutor(JobExecutorConfig{
		Source:     &sliceSource{items: items},
		Dispatcher: dispatcher,
		Collector:  collector,
		Tracker:    tracker,
		Logger:     logr.Discard(),
	})

	// Deliver only 3 results, then cancel
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		for i := 0; i < 3; i++ {
			client.deliver(items[i].RequestID, map[string]any{"ok": true})
		}
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := executor.Execute(ctx)
	_ = err

	outputData := readFile(t, outputFile)
	errorData := readFile(t, errorFile)
	outputLines := countLines(outputData)
	errorLines := countLines(errorData)
	total := outputLines + errorLines

	t.Logf("output=%d error=%d total=%d (of %d)", outputLines, errorLines, total, len(items))

	if outputLines < 3 {
		t.Errorf("expected at least 3 completed, got %d", outputLines)
	}
}
