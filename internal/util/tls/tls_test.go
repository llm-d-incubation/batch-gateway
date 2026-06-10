package tls

import (
	"crypto/tls"
	"testing"
)

func TestGetTlsConfig(t *testing.T) {
	tests := []struct {
		name     string
		loadType LoadType
		insecure bool
	}{
		{
			name:     "client defaults to TLS 1.2 minimum",
			loadType: LOAD_TYPE_CLIENT,
		},
		{
			name:     "server defaults to TLS 1.2 minimum",
			loadType: LOAD_TYPE_SERVER,
		},
		{
			name:     "insecure client still enforces TLS 1.2 minimum",
			loadType: LOAD_TYPE_CLIENT,
			insecure: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := GetTlsConfig(tt.loadType, tt.insecure, "", "", "")
			if err != nil {
				t.Fatalf("GetTlsConfig() error = %v", err)
			}
			if cfg == nil {
				t.Fatal("GetTlsConfig() returned nil config")
			}
			if cfg.MinVersion != tls.VersionTLS12 {
				t.Fatalf("GetTlsConfig() MinVersion = %v, want %v", cfg.MinVersion, tls.VersionTLS12)
			}
		})
	}
}
