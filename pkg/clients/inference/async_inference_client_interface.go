package inference

import "context"

// AsyncInferenceClient defines the interface for non-blocking async dispatch.
// Each instance is per-job: created by AsyncGatewayResolver.ClientFor, used
// for one submit/collect cycle, then closed.
type AsyncInferenceClient interface {
	Submit(ctx context.Context, req *GenerateRequest) *ClientError
	GetResult(ctx context.Context) (*GenerateResponse, error)
	Close() error
}
