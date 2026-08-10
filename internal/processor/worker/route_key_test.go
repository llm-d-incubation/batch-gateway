package worker

import "testing"

func TestRouteKey(t *testing.T) {
	tests := []struct {
		name     string
		byTenant bool
		tenantID string
		modelID  string
		want     string
	}{
		{
			name:     "disabled keeps bare model ID",
			byTenant: false,
			tenantID: "m-20260720103021-nvfbq",
			modelID:  "test-model-v1",
			want:     "test-model-v1",
		},
		{
			name:     "enabled with empty tenant keeps bare model ID",
			byTenant: true,
			tenantID: "",
			modelID:  "test-model-v1",
			want:     "test-model-v1",
		},
		{
			name:     "enabled scopes the lookup key by tenant",
			byTenant: true,
			tenantID: "m-20260720103021-nvfbq",
			modelID:  "test-model-v1",
			want:     "m-20260720103021-nvfbq/test-model-v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeKey(tt.byTenant, tt.tenantID, tt.modelID); got != tt.want {
				t.Fatalf("routeKey(%v, %q, %q) = %q, want %q", tt.byTenant, tt.tenantID, tt.modelID, got, tt.want)
			}
		})
	}
}
