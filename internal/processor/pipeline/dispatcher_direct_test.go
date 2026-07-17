package pipeline

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"

	batch_types "github.com/llm-d/llm-d-batch-gateway/internal/shared/types"
	httpclient "github.com/llm-d/llm-d-batch-gateway/pkg/clients/http"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

func TestBuildResult(t *testing.T) {
	msg := RequestItem{
		RequestID: "req-1",
		CustomID:  "c-1",
		ModelID:   "m1",
	}

	t.Run("success", func(t *testing.T) {
		resp := &inference.GenerateResponse{
			RequestID: "srv-123",
			Response:  []byte(`{"result":"ok"}`),
		}
		result := buildResult(msg, resp, nil, logr.Discard())
		if result.Error != nil {
			t.Fatalf("expected no error, got %+v", result.Error)
		}
		if result.Response == nil {
			t.Fatal("expected response")
		}
		if result.Response.StatusCode != 200 {
			t.Fatalf("StatusCode = %d, want 200", result.Response.StatusCode)
		}
		if result.Response.RequestID != "srv-123" {
			t.Fatalf("RequestID = %q, want %q", result.Response.RequestID, "srv-123")
		}
	})

	t.Run("success with capacity retry", func(t *testing.T) {
		resp := &inference.GenerateResponse{
			RequestID:        "srv-123",
			Response:         []byte(`{"result":"ok"}`),
			HadCapacityRetry: true,
		}
		result := buildResult(msg, resp, nil, logr.Discard())
		if result.Error != nil {
			t.Fatalf("expected no error, got %+v", result.Error)
		}
		if !result.HadCapacityRetry {
			t.Fatal("expected HadCapacityRetry=true")
		}
		if result.Response == nil || result.Response.StatusCode != 200 {
			t.Fatalf("expected 200 response, got %+v", result.Response)
		}
	})

	t.Run("4xx goes to output file", func(t *testing.T) {
		clientErr := &inference.ClientError{
			Category:     httpclient.ErrCategoryInvalidReq,
			Message:      "HTTP 422: Invalid model",
			StatusCode:   422,
			ResponseBody: []byte(`{"error":{"message":"Invalid model","type":"invalid_request_error","code":"model_not_found"}}`),
		}
		result := buildResult(msg, nil, clientErr, logr.Discard())
		if result.Error != nil {
			t.Fatalf("expected nil error for HTTP error (goes to output file), got %+v", result.Error)
		}
		if result.Response == nil {
			t.Fatal("expected response field")
		}
		if result.Response.StatusCode != 422 {
			t.Fatalf("StatusCode = %d, want 422", result.Response.StatusCode)
		}
	})

	t.Run("HTTP error with empty body", func(t *testing.T) {
		clientErr := &inference.ClientError{
			Category:     httpclient.ErrCategoryServer,
			Message:      "HTTP 502: ",
			StatusCode:   502,
			ResponseBody: nil,
		}
		result := buildResult(msg, nil, clientErr, logr.Discard())
		if result.Error != nil {
			t.Fatalf("expected nil error for HTTP error, got %+v", result.Error)
		}
		if result.Response == nil {
			t.Fatal("expected response field")
		}
		if result.Response.StatusCode != 502 {
			t.Fatalf("StatusCode = %d, want 502", result.Response.StatusCode)
		}
		if result.Response.Body == nil {
			t.Fatal("expected non-nil body (empty object), got nil")
		}
		if len(result.Response.Body) != 0 {
			t.Fatalf("expected empty body object, got %v", result.Response.Body)
		}
	})

	t.Run("HTTP error with non-JSON body", func(t *testing.T) {
		clientErr := &inference.ClientError{
			Category:     httpclient.ErrCategoryServer,
			Message:      "HTTP 500: Bad Gateway",
			StatusCode:   500,
			ResponseBody: []byte("<html>Bad Gateway</html>"),
		}
		result := buildResult(msg, nil, clientErr, logr.Discard())
		if result.Error != nil {
			t.Fatalf("expected nil error for HTTP error, got %+v", result.Error)
		}
		if result.Response == nil {
			t.Fatal("expected response field")
		}
		if result.Response.StatusCode != 500 {
			t.Fatalf("StatusCode = %d, want 500", result.Response.StatusCode)
		}
		errObj, ok := result.Response.Body["error"].(map[string]any)
		if !ok {
			t.Fatalf("expected synthetic error object in body, got %v", result.Response.Body)
		}
		if errObj["message"] != "<html>Bad Gateway</html>" {
			t.Fatalf("expected original body as message, got %v", errObj["message"])
		}
		if errObj["type"] != "server_error" {
			t.Fatalf("expected type 'server_error', got %v", errObj["type"])
		}
	})

	t.Run("non-HTTP error", func(t *testing.T) {
		clientErr := &inference.ClientError{
			Category: httpclient.ErrCategoryServer,
			Message:  "backend unavailable",
		}
		result := buildResult(msg, nil, clientErr, logr.Discard())
		if result.Error == nil {
			t.Fatal("expected error field for non-HTTP error")
		}
		if result.Error.Code != string(httpclient.ErrCategoryServer) {
			t.Fatalf("error code = %q, want %q", result.Error.Code, httpclient.ErrCategoryServer)
		}
		if result.Response != nil {
			t.Fatalf("expected nil response for non-HTTP error, got %+v", result.Response)
		}
	})

	t.Run("DroppedReason TTL expired", func(t *testing.T) {
		clientErr := &inference.ClientError{
			Category:      httpclient.ErrCategoryRateLimit,
			Message:       "HTTP 429: Rate limit exceeded",
			StatusCode:    429,
			ResponseBody:  []byte(`{"error":{"message":"Rate limit exceeded"}}`),
			DroppedReason: httpclient.DroppedReasonTTLExpired,
		}
		result := buildResult(msg, nil, clientErr, logr.Discard())
		if result.Error == nil {
			t.Fatal("expected error for TTL-expired dropped reason")
		}
		if result.Error.Code != string(batch_types.ErrCodeBatchExpired) {
			t.Fatalf("error code = %q, want %q", result.Error.Code, batch_types.ErrCodeBatchExpired)
		}
		if result.Error.Message != batch_types.ErrCodeBatchExpired.Message() {
			t.Fatalf("error message = %q, want %q", result.Error.Message, batch_types.ErrCodeBatchExpired.Message())
		}
		if result.Response != nil {
			t.Fatalf("expected nil response for TTL-expired, got %+v", result.Response)
		}
	})

	t.Run("DroppedReason other 4xx goes to output file", func(t *testing.T) {
		clientErr := &inference.ClientError{
			Category:      httpclient.ErrCategoryRateLimit,
			Message:       "HTTP 429: Rate limit exceeded",
			StatusCode:    429,
			ResponseBody:  []byte(`{"error":{"message":"Rate limit exceeded"}}`),
			DroppedReason: "rejected-saturated",
		}
		result := buildResult(msg, nil, clientErr, logr.Discard())
		if result.Error != nil {
			t.Fatalf("expected nil error for HTTP error (goes to output file), got %+v", result.Error)
		}
		if result.Response == nil {
			t.Fatal("expected response field")
		}
		if result.Response.StatusCode != 429 {
			t.Fatalf("StatusCode = %d, want 429", result.Response.StatusCode)
		}
	})

	t.Run("nil response nil error", func(t *testing.T) {
		result := buildResult(msg, nil, nil, logr.Discard())
		if result.Error == nil {
			t.Fatal("expected error for nil response")
		}
		if result.Error.Code != string(httpclient.ErrCategoryServer) {
			t.Fatalf("error code = %q, want %q", result.Error.Code, httpclient.ErrCategoryServer)
		}
	})

	t.Run("bad JSON response", func(t *testing.T) {
		resp := &inference.GenerateResponse{
			RequestID: "srv-bad",
			Response:  []byte(`{not valid json`),
		}
		result := buildResult(msg, resp, nil, logr.Discard())
		if result.Error == nil {
			t.Fatal("expected error for bad JSON response")
		}
		if result.Error.Code != string(httpclient.ErrCategoryParse) {
			t.Fatalf("error code = %q, want %q", result.Error.Code, httpclient.ErrCategoryParse)
		}
	})

	t.Run("context cancelled overrides non-HTTP error", func(t *testing.T) {
		clientErr := &inference.ClientError{
			Category: httpclient.ErrCategoryUnknown,
			Message:  "request cancelled",
		}
		result := buildResult(msg, nil, clientErr, logr.Discard())
		if result.Error == nil {
			t.Fatal("expected error field")
		}
		// buildResult itself does not do the ctx.Err() override — that's done
		// in DirectDispatcher.Receive. Verify the raw error is preserved.
		if result.Error.Code != string(httpclient.ErrCategoryUnknown) {
			t.Fatalf("error code = %q, want %q", result.Error.Code, httpclient.ErrCategoryUnknown)
		}
	})
}

func TestDirectDispatcher_ModelNotFound(t *testing.T) {
	resolver := inference.NewPerModelClientResolver(map[string]inference.InferenceClient{
		"m1": &mockInferenceClient{response: []byte(`{}`)},
	})
	defer func() { _ = resolver.Close() }()

	dispatcher := NewDirectDispatcher(resolver, logr.Discard())

	resultCh := make(chan ResultItem, 1)
	dispatcher.handleMessage(context.Background(), RequestItem{
		RequestID: "req-bad",
		CustomID:  "c-bad",
		ModelID:   "no-such-model",
		Endpoint:  "/v1/chat/completions",
	}, resultCh)

	result := <-resultCh
	if result.Error == nil {
		t.Fatal("expected error for unknown model")
	}
	if result.Error.Code != inference.ErrCodeModelNotFound {
		t.Fatalf("error code = %q, want %q", result.Error.Code, inference.ErrCodeModelNotFound)
	}
	if result.CustomID != "c-bad" {
		t.Fatalf("CustomID = %q, want %q", result.CustomID, "c-bad")
	}
	if result.ModelID != "no-such-model" {
		t.Fatalf("ModelID = %q, want %q", result.ModelID, "no-such-model")
	}
}

func TestToFloat64(t *testing.T) {
	cases := []struct {
		name string
		in   any
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
			got, ok := toFloat64(tc.in)
			if ok != tc.ok {
				t.Fatalf("toFloat64(%v) ok=%v, want %v", tc.in, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("toFloat64(%v)=%v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestRecordTokenUsage(t *testing.T) {
	logger := logr.Discard()

	t.Run("usage present with both fields", func(t *testing.T) {
		body := map[string]any{
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens":     float64(42),
				"completion_tokens": float64(128),
			},
		}
		recordTokenUsage(body, "token-pipe-both", logger)
	})

	t.Run("usage missing", func(t *testing.T) {
		body := map[string]any{"choices": []any{}}
		recordTokenUsage(body, "token-pipe-missing", logger)
	})

	t.Run("usage present but no numeric fields", func(t *testing.T) {
		body := map[string]any{
			"usage": map[string]any{
				"prompt_tokens": "not-a-number",
			},
		}
		recordTokenUsage(body, "token-pipe-non-numeric", logger)
	})

	t.Run("nil body", func(t *testing.T) {
		recordTokenUsage(nil, "token-pipe-nil", logger)
	})

	t.Run("negative token values skipped", func(t *testing.T) {
		body := map[string]any{
			"usage": map[string]any{
				"prompt_tokens":     float64(-10),
				"completion_tokens": float64(50),
			},
		}
		recordTokenUsage(body, "token-pipe-negative", logger)
	})
}

var _ inference.InferenceClient = (*mockInferenceClientForDirectTest)(nil)

type mockInferenceClientForDirectTest struct {
	generateFn func(context.Context, *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError)
}

func (m *mockInferenceClientForDirectTest) Generate(ctx context.Context, req *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
	return m.generateFn(ctx, req)
}
