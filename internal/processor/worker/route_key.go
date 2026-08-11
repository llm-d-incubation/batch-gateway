package worker

import (
	"github.com/llm-d/llm-d-batch-gateway/internal/processor/config"
)

// routeKey returns the model_gateways lookup key for a request model. When
// the route key method is "tenant" and tenantID is non-empty, gateway entries
// are scoped per tenant as "<tenantID>/<modelID>", letting identically-named
// models of different tenants route to their own backends (e.g. per-InferSet
// gateways sharing one batch-apiserver). Otherwise the bare model ID is used,
// preserving the default behavior.
func routeKey(method config.RouteKeyMethod, tenantID, modelID string) string {
	if method == config.RouteKeyMethodTenant && tenantID != "" {
		return tenantID + "/" + modelID
	}
	return modelID
}
