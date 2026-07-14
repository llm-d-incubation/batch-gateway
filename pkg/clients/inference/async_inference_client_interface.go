package inference

import "context"

// AsyncInferenceClient defines the interface for non-blocking async dispatch.
// Each instance is per-job: created by AsyncGatewayResolver.ClientFor, used
// for one submit/collect cycle, then closed.
type AsyncInferenceClient interface {
	Submit(ctx context.Context, req *GenerateRequest) *ClientError
	GetResult(ctx context.Context) (*GenerateResponse, error)
	// Cancel marks all still-pending submitted requests as cancelled in the
	// dispatcher (best-effort pre-dispatch). It does not unregister waiters;
	// callers should still Close after local drain.
	Cancel(ctx context.Context) error
	Close() error
}
