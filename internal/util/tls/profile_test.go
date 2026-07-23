/*
Copyright 2026 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tls

import (
	"crypto/tls"
	"testing"
)

func TestParseMinVersion(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    uint16
		wantErr bool
	}{
		{
			name:  "empty string defaults to TLS 1.2",
			input: "",
			want:  tls.VersionTLS12,
		},
		{
			name:  "VersionTLS12",
			input: "VersionTLS12",
			want:  tls.VersionTLS12,
		},
		{
			name:  "VersionTLS13",
			input: "VersionTLS13",
			want:  tls.VersionTLS13,
		},
		{
			name:    "VersionTLS10 rejected",
			input:   "VersionTLS10",
			wantErr: true,
		},
		{
			name:    "VersionTLS11 rejected",
			input:   "VersionTLS11",
			wantErr: true,
		},
		{
			name:    "numeric version rejected",
			input:   "1.2",
			wantErr: true,
		},
		{
			name:    "garbage rejected",
			input:   "garbage",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMinVersion(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseMinVersion(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("ParseMinVersion(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseCipherSuites(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantNil bool
		wantLen int
		wantErr bool
	}{
		{
			name:    "empty string returns nil",
			input:   "",
			wantNil: true,
		},
		{
			name:    "single valid cipher",
			input:   "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			wantLen: 1,
		},
		{
			name:    "multiple valid ciphers",
			input:   "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
			wantLen: 2,
		},
		{
			name:    "whitespace trimmed",
			input:   " TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 , TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384 ",
			wantLen: 2,
		},
		{
			name:    "trailing comma ignored",
			input:   "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,",
			wantLen: 1,
		},
		{
			name:    "unknown cipher rejected",
			input:   "UNKNOWN_CIPHER",
			wantErr: true,
		},
		{
			name:    "insecure cipher rejected",
			input:   "TLS_RSA_WITH_RC4_128_SHA",
			wantErr: true,
		},
		{
			name:    "only whitespace returns nil",
			input:   " , , ",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCipherSuites(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseCipherSuites(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.wantNil && got != nil {
				t.Fatalf("ParseCipherSuites(%q) = %v, want nil", tt.input, got)
			}
			if !tt.wantNil && len(got) != tt.wantLen {
				t.Fatalf("ParseCipherSuites(%q) returned %d IDs, want %d", tt.input, len(got), tt.wantLen)
			}
		})
	}
}
