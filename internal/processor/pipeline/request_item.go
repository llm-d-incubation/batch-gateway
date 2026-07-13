package pipeline

import (
	"fmt"

	batch_types "github.com/llm-d/llm-d-batch-gateway/internal/shared/types"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

// RequestItem is a fully-parsed inference request ready for dispatch.
type RequestItem struct {
	RequestID string
	CustomID  string
	ModelID   string
	Endpoint  string
	Body      map[string]any
	Headers   map[string]string
}

// ResultItem is the outcome of one inference request.
type ResultItem struct {
	RequestID        string
	CustomID         string
	ModelID          string
	Response         *batch_types.ResponseData
	Error            *OutputError
	HadCapacityRetry bool
}

func (r *RequestItem) Canceled() *ResultItem {
	return r.Error("batch_cancelled", "request cancelled")
}

func (r *RequestItem) ModelNotFound() *ResultItem {
	return r.Error(inference.ErrCodeModelNotFound, fmt.Sprintf("model %q not configured", r.ModelID))
}

func (r *RequestItem) Error(code, message string) *ResultItem {
	return &ResultItem{
		RequestID: r.RequestID,
		CustomID:  r.CustomID,
		ModelID:   r.ModelID,
		Error:     &OutputError{Code: code, Message: message},
	}
}

// OutputError is the error structure for JSONL output.
type OutputError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
