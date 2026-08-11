package worker

import (
	"testing"

	"github.com/llm-d/llm-d-batch-gateway/internal/processor/config"
)

func TestRouteKey(t *testing.T) {
	tests := []struct {
		name     string
		method   config.RouteKeyMethod
		tenantID string
		modelID  string
		want     string
	}{
		{
			name:     "default method keeps bare model ID",
			method:   config.RouteKeyMethodBare,
			tenantID: "m-20260720103021-nvfbq",
			modelID:  "test-model-v1",
			want:     "test-model-v1",
		},
		{
			name:     "tenant method with empty tenant keeps bare model ID",
			method:   config.RouteKeyMethodTenant,
			tenantID: "",
			modelID:  "test-model-v1",
			want:     "test-model-v1",
		},
		{
			name:     "tenant method scopes the lookup key by tenant",
			method:   config.RouteKeyMethodTenant,
			tenantID: "m-20260720103021-nvfbq",
			modelID:  "test-model-v1",
			want:     "m-20260720103021-nvfbq/test-model-v1",
		},
		{
			name:     "unknown method keeps bare model ID",
			method:   config.RouteKeyMethod("geography"),
			tenantID: "m-20260720103021-nvfbq",
			modelID:  "test-model-v1",
			want:     "test-model-v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeKey(tt.method, tt.tenantID, tt.modelID); got != tt.want {
				t.Fatalf("routeKey(%q, %q, %q) = %q, want %q", tt.method, tt.tenantID, tt.modelID, got, tt.want)
			}
		})
	}
}
