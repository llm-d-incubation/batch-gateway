package worker

// routeKey returns the model_gateways lookup key for a request model. When
// byTenant is enabled and tenantID is non-empty, gateway entries are scoped
// per tenant as "<tenantID>/<modelID>", letting identically-named models of
// different tenants route to their own backends (e.g. per-InferSet gateways
// sharing one batch-apiserver). Otherwise the bare model ID is used,
// preserving the default behavior.
func routeKey(byTenant bool, tenantID, modelID string) string {
	if !byTenant || tenantID == "" {
		return modelID
	}
	return tenantID + "/" + modelID
}
