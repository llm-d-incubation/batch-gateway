package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/go-logr/logr"

	batch_types "github.com/llm-d/llm-d-batch-gateway/internal/shared/types"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
	httpclient "github.com/llm-d/llm-d-batch-gateway/pkg/clients/http"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

// DirectDispatcher dispatches requests to an inference gateway via HTTP client.
type DirectDispatcher struct {
	inference *inference.GatewayResolver
	logger    logr.Logger
	wg        sync.WaitGroup
}

var _ RequestDispatcher = (*DirectDispatcher)(nil)

func NewDirectDispatcher(resolver *inference.GatewayResolver, logger logr.Logger) *DirectDispatcher {
	return &DirectDispatcher{inference: resolver, logger: logger}
}

func (d *DirectDispatcher) Run(ctx context.Context, requestCh <-chan RequestItem, resultCh chan<- ResultItem) error {
	for msg := range requestCh {
		d.handleMessage(ctx, msg, resultCh)
	}
	d.wg.Wait()
	close(resultCh)
	return nil
}

func (d *DirectDispatcher) handleMessage(
	ctx context.Context, msg RequestItem, resultCh chan<- ResultItem) {

	// Find inference client for the model
	client := d.inference.ClientFor(msg.ModelID)

	// If the model is unknown return an error.
	if client == nil {
		resultCh <- *msg.ModelNotFound()
		return
	}

	// Otherwise spawn a request in a go routine.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.submitRequest(ctx, msg, client, resultCh)
	}()
}

func (d *DirectDispatcher) submitRequest(
	ctx context.Context, msg RequestItem, client inference.InferenceClient, resultCh chan<- ResultItem) {

	req := &inference.GenerateRequest{
		RequestID: msg.RequestID,
		Endpoint:  msg.Endpoint,
		Params:    msg.Body,
		Headers:   msg.Headers,
	}

	resp, clientErr := client.Generate(ctx, req)

	result := buildResult(msg, resp, clientErr, d.logger)
	if ctx.Err() != nil && result.Error != nil && result.Response == nil {
		code, message := cancelCode(ctx)
		result.Error = &OutputError{Code: code, Message: message}
	}
	resultCh <- result
}

// All per-request metrics (inflight Dec, duration, error counts, token usage)
// are recorded by the collector via SubmittedAt propagation — not here.
func buildResult(msg RequestItem, resp *inference.GenerateResponse, clientErr *inference.ClientError, logger logr.Logger) ResultItem {
	result := ResultItem{
		RequestID:   msg.RequestID,
		CustomID:    msg.CustomID,
		ModelID:     msg.ModelID,
		SubmittedAt: msg.SubmittedAt,
	}
	if resp != nil {
		result.HadCapacityRetry = resp.HadCapacityRetry
	}

	switch {
	case clientErr != nil:
		handleClientError(&result, clientErr, logger)
	case resp == nil:
		result.Error = &OutputError{Code: string(httpclient.ErrCategoryServer), Message: "inference returned no response"}
	default:
		handleSuccess(&result, resp, logger)
	}

	return result
}

func handleClientError(result *ResultItem, clientErr *inference.ClientError, logger logr.Logger) {
	logger.V(logging.DEBUG).Info("Inference request failed", "requestId", result.RequestID, "error", clientErr.Message)

	if clientErr.StatusCode <= 0 {
		result.Error = &OutputError{Code: string(clientErr.Category), Message: clientErr.Message}

		return
	}

	if clientErr.DroppedReason == httpclient.DroppedReasonTTLExpired {
		result.Error = &OutputError{
			Code:    string(batch_types.ErrCodeBatchExpired),
			Message: batch_types.ErrCodeBatchExpired.Message(),
		}
		return
	}

	// Build the HTTP response for all status codes so that upstream
	// middleware (e.g. AIMD) can always inspect the status code.
	body := make(map[string]any)
	if len(clientErr.ResponseBody) > 0 {
		if err := json.Unmarshal(clientErr.ResponseBody, &body); err != nil {
			body = map[string]any{
				"error": map[string]any{
					"message": string(clientErr.ResponseBody),
					"type":    clientErr.OpenAIErrorType(),
				},
			}
		}
	}
	result.Response = &batch_types.ResponseData{
		StatusCode: clientErr.StatusCode,
		RequestID:  result.RequestID,
		Body:       body,
	}
}

func handleSuccess(result *ResultItem, resp *inference.GenerateResponse, logger logr.Logger) {
	var body map[string]any
	if len(resp.Response) > 0 {
		if err := json.Unmarshal(resp.Response, &body); err != nil {
			logger.Error(err, "Failed to unmarshal inference response body", "requestId", result.RequestID)
			result.Error = &OutputError{
				Code:    string(httpclient.ErrCategoryParse),
				Message: fmt.Sprintf("response body could not be parsed: %v", err),
			}

			return
		}
	}

	result.Response = &batch_types.ResponseData{
		StatusCode: 200,
		RequestID:  resp.RequestID,
		Body:       body,
	}
}
