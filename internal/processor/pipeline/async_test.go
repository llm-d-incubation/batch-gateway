package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

type fakeAsyncClient struct {
	mu           sync.Mutex
	submitted    []*inference.GenerateRequest
	results      chan *inference.GenerateResponse
	cancelledIDs []string
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

func (c *fakeAsyncClient) Cancel(_ context.Context, ids []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelledIDs = append(c.cancelledIDs, ids...)
	return nil
}
func (c *fakeAsyncClient) Close() error { return nil }

func (c *fakeAsyncClient) deliver(requestID string, body map[string]any) {
	resp, _ := json.Marshal(body)
	c.results <- &inference.GenerateResponse{
		RequestID:  requestID,
		Response:   resp,
		StatusCode: 200,
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
	broadcasters := NewBroadcasterGroup([]*ResultBroadcaster{broadcaster})
	broadcasterCtx, broadcasterCancel := context.WithCancel(context.Background())
	defer broadcasterCancel()
	go broadcaster.Run(broadcasterCtx)

	items := []RequestItem{
		{RequestID: "req-1", CustomID: "c-1", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-2", CustomID: "c-2", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-3", CustomID: "c-3", ModelID: "m1", Endpoint: "/v1/chat/completions"},
	}

	pending := NewPendingRequests(0)
	outputFile := tempFile(t)
	errorFile := tempFile(t)
	tracker := NewProgressTracker(int64(len(items)), nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	dispatcher := NewAsyncDispatcher(resolver,
		broadcasters,
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

func TestAsyncResult(t *testing.T) {
	tests := []struct {
		name           string
		resp           *inference.GenerateResponse
		wantStatusCode int
		wantErrCode    string
		wantResponse   bool
	}{
		{
			name: "nil body treated as empty 2xx",
			resp: &inference.GenerateResponse{
				RequestID: "req-nil-body",
				Response:  nil,
			},
			wantErrCode: "server_error",
		},
		{
			name: "bad JSON on 2xx is parse_error",
			resp: &inference.GenerateResponse{
				RequestID:  "req-bad-json",
				Response:   []byte(`{not valid json`),
				StatusCode: 200,
			},
			wantErrCode: "parse_error",
		},
		{
			name: "legacy success without StatusCode defaults to 200",
			resp: &inference.GenerateResponse{
				RequestID: "req-ok",
				Response:  []byte(`{"choices":[]}`),
			},
			wantStatusCode: 200,
			wantResponse:   true,
		},
		{
			name: "success with StatusCode 200",
			resp: &inference.GenerateResponse{
				RequestID:  "req-ok-200",
				Response:   []byte(`{"choices":[]}`),
				StatusCode: 200,
			},
			wantStatusCode: 200,
			wantResponse:   true,
		},
		{
			name: "HTTP 403 with empty body preserves status",
			resp: &inference.GenerateResponse{
				RequestID:  "req-403",
				Response:   []byte{},
				StatusCode: 403,
			},
			wantStatusCode: 403,
			wantResponse:   true,
		},
		{
			name: "HTTP 403 with JSON error body preserves status",
			resp: &inference.GenerateResponse{
				RequestID:  "req-403-json",
				Response:   []byte(`{"error":{"message":"unauthorized"}}`),
				StatusCode: 403,
			},
			wantStatusCode: 403,
			wantResponse:   true,
		},
		{
			name: "HTTP 422 with unparseable body preserves status",
			resp: &inference.GenerateResponse{
				RequestID:  "req-422",
				Response:   []byte(`not-json`),
				StatusCode: 422,
			},
			wantStatusCode: 422,
			wantResponse:   true,
		},
		{
			name: "non-HTTP failure uses ErrorCode",
			resp: &inference.GenerateResponse{
				RequestID:    "req-deadline",
				StatusCode:   0,
				ErrorCode:    "DEADLINE_EXCEEDED",
				ErrorMessage: "deadline exceeded",
			},
			wantErrCode: "DEADLINE_EXCEEDED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := asyncResult(tt.resp, logr.Discard())
			if tt.wantErrCode != "" {
				if result.Error == nil {
					t.Fatalf("expected error code %q, got nil error", tt.wantErrCode)
				}
				if result.Error.Code != tt.wantErrCode {
					t.Fatalf("error code = %q, want %q", result.Error.Code, tt.wantErrCode)
				}
				if result.Response != nil {
					t.Fatalf("expected nil response, got %+v", result.Response)
				}
				return
			}
			if result.Error != nil {
				t.Fatalf("unexpected error: %+v", result.Error)
			}
			if !tt.wantResponse {
				return
			}
			if result.Response == nil {
				t.Fatal("expected response")
			}
			if result.Response.StatusCode != tt.wantStatusCode {
				t.Fatalf("StatusCode = %d, want %d", result.Response.StatusCode, tt.wantStatusCode)
			}
			if result.Response.RequestID != tt.resp.RequestID {
				t.Fatalf("RequestID = %q, want %q", result.Response.RequestID, tt.resp.RequestID)
			}
		})
	}
}

func TestSafeChannelSend(t *testing.T) {
	b := NewResultBroadcaster(nil, logr.Discard())
	result := ResultItem{RequestID: "req-1"}

	t.Run("sends to open channel", func(t *testing.T) {
		ch := make(chan ResultItem, 1)
		b.safeChannelSend(result, ch)
		got := <-ch
		if got.RequestID != "req-1" {
			t.Fatalf("RequestID = %q, want %q", got.RequestID, "req-1")
		}
	})

	t.Run("recovers send on closed channel", func(t *testing.T) {
		ch := make(chan ResultItem)
		close(ch)
		b.safeChannelSend(result, ch)
	})
}

func TestBroadcaster_RetriesTransientError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var callCount atomic.Int32
		transientErr := fmt.Errorf("connection reset")

		client := &fakeAsyncClientWithErrors{
			getResult: func(ctx context.Context) (*inference.GenerateResponse, error) {
				n := callCount.Add(1)
				if n <= 3 {
					return nil, transientErr
				}
				resp, _ := json.Marshal(map[string]any{"ok": true})
				return &inference.GenerateResponse{
					RequestID: "req-1",
					Response:  resp,
				}, nil
			},
		}

		broadcaster := NewResultBroadcaster(client, logr.Discard())
		resultCh := make(chan ResultItem, 10)
		broadcaster.Subscribe(resultCh)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go broadcaster.Run(ctx)

		// Advance past backoff sleeps: 100ms + 200ms + 400ms = 700ms
		time.Sleep(time.Second)
		synctest.Wait()

		select {
		case result := <-resultCh:
			if result.Error != nil {
				t.Fatalf("expected successful result after transient errors, got error: %+v", result.Error)
			}
			if result.RequestID != "req-1" {
				t.Fatalf("RequestID = %q, want %q", result.RequestID, "req-1")
			}
		default:
			t.Fatal("broadcaster stopped after transient error instead of retrying")
		}

		if n := callCount.Load(); n < 4 {
			t.Fatalf("GetResult called %d times, want >= 4 (3 errors + 1 success)", n)
		}
	})
}

type fakeAsyncClientWithErrors struct {
	getResult func(ctx context.Context) (*inference.GenerateResponse, error)
}

func (c *fakeAsyncClientWithErrors) Submit(_ context.Context, _ *inference.GenerateRequest) *inference.ClientError {
	return nil
}

func (c *fakeAsyncClientWithErrors) GetResult(ctx context.Context) (*inference.GenerateResponse, error) {
	return c.getResult(ctx)
}

func (c *fakeAsyncClientWithErrors) Cancel(_ context.Context, _ []string) error { return nil }
func (c *fakeAsyncClientWithErrors) Close() error                               { return nil }

func TestAsyncDispatcher_ParseError(t *testing.T) {
	client := newFakeAsyncClient()
	resolver := inference.NewTestAsyncResolver(map[string]func() inference.AsyncInferenceClient{
		"m1": func() inference.AsyncInferenceClient { return client },
	})
	defer func() { _ = resolver.Close() }()

	broadcaster := NewResultBroadcaster(client, logr.Discard())
	broadcasters := NewBroadcasterGroup([]*ResultBroadcaster{broadcaster})
	broadcasterCtx, broadcasterCancel := context.WithCancel(context.Background())
	defer broadcasterCancel()
	go broadcaster.Run(broadcasterCtx)

	items := []RequestItem{
		{RequestID: "req-1", CustomID: "c-1", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-bad", CustomID: "req-bad", ParseError: &OutputError{Code: "parse_error", Message: "bad json"}},
		{RequestID: "req-2", CustomID: "c-2", ModelID: "m1", Endpoint: "/v1/chat/completions"},
	}

	pending := NewPendingRequests(0)
	outputFile := tempFile(t)
	errorFile := tempFile(t)
	tracker := NewProgressTracker(int64(len(items)), nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	dispatcher := NewAsyncDispatcher(resolver,
		broadcasters,
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
		client.deliver("req-2", map[string]any{"ok": true})
	}()

	counts, err := executor.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	if counts.Completed != 2 {
		t.Errorf("Completed = %d, want 2", counts.Completed)
	}
	if counts.Failed != 1 {
		t.Errorf("Failed = %d, want 1", counts.Failed)
	}

	outputData := readFile(t, outputFile)
	outputLines := countLines(outputData)
	if outputLines != 2 {
		t.Fatalf("output lines = %d, want 2", outputLines)
	}

	errorData := readFile(t, errorFile)
	errorLines := splitLines(errorData)
	if len(errorLines) != 1 {
		t.Fatalf("error lines = %d, want 1 (parse error)", len(errorLines))
	}
	var errEntry outputLine
	if err := json.Unmarshal(errorLines[0], &errEntry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if errEntry.Error == nil || errEntry.Error.Code != "parse_error" {
		t.Fatalf("expected parse_error, got %+v", errEntry.Error)
	}
}

func TestAsyncDispatcher_ModelNotFound(t *testing.T) {
	client := newFakeAsyncClient()
	resolver := inference.NewTestAsyncResolver(map[string]func() inference.AsyncInferenceClient{
		"m1": func() inference.AsyncInferenceClient { return client },
	})
	defer func() { _ = resolver.Close() }()

	broadcaster := NewResultBroadcaster(client, logr.Discard())
	broadcasters := NewBroadcasterGroup([]*ResultBroadcaster{broadcaster})
	broadcasterCtx, broadcasterCancel := context.WithCancel(context.Background())
	defer broadcasterCancel()
	go broadcaster.Run(broadcasterCtx)

	items := []RequestItem{
		{RequestID: "req-1", CustomID: "c-1", ModelID: "m1", Endpoint: "/v1/chat/completions"},
		{RequestID: "req-2", CustomID: "c-2", ModelID: "no-such-model", Endpoint: "/v1/chat/completions"},
	}

	pending := NewPendingRequests(0)
	outputFile := tempFile(t)
	errorFile := tempFile(t)
	tracker := NewProgressTracker(int64(len(items)), nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	dispatcher := NewAsyncDispatcher(resolver,
		broadcasters,
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
	broadcasters := NewBroadcasterGroup([]*ResultBroadcaster{broadcaster})
	broadcasterCtx, broadcasterCancel := context.WithCancel(context.Background())
	defer broadcasterCancel()
	go broadcaster.Run(broadcasterCtx)

	items := makeItems(10, "m1")

	pending := NewPendingRequests(0)
	outputFile := tempFile(t)
	errorFile := tempFile(t)
	tracker := NewProgressTracker(int64(len(items)), nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	dispatcher := NewAsyncDispatcher(resolver,
		broadcasters,
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
	if err != nil && err != context.Canceled {
		t.Fatalf("Execute() error = %v, want nil or context.Canceled", err)
	}

	outputData := readFile(t, outputFile)
	errorData := readFile(t, errorFile)
	outputLines := countLines(outputData)
	errorLines := countLines(errorData)
	total := outputLines + errorLines

	t.Logf("output=%d error=%d total=%d (of %d)", outputLines, errorLines, total, len(items))

	if outputLines < 3 {
		t.Errorf("expected at least 3 completed, got %d", outputLines)
	}
	if total != len(items) {
		t.Errorf("total output+error lines = %d, want %d (all requests accounted for)", total, len(items))
	}

	// Verify Cancel was called with the uncollected request IDs.
	client.mu.Lock()
	cancelled := client.cancelledIDs
	client.mu.Unlock()

	// We delivered 3 results out of 10, so ~7 should have been cancelled.
	if len(cancelled) == 0 {
		t.Error("expected Cancel to be called with pending IDs, but no IDs were cancelled")
	}
	t.Logf("cancelled %d IDs", len(cancelled))
}
