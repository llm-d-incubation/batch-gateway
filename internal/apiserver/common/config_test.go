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

// The file contains unit tests for the configuration.
package common

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAPIServerConfig(t *testing.T) {
	t.Run("LoadFromYAML", func(t *testing.T) {
		tests := []struct {
			name       string
			yamlConfig string
			fileName   string
			want       ServerConfig
			wantErr    bool
		}{
			{
				name: "valid yaml config",
				yamlConfig: `
host: 0.0.0.0
port: "8080"
ssl_cert_file: testdata/cert.pem
ssl_key_file: testdata/key.pem
`,
				fileName: "config.yaml",
				want: ServerConfig{
					Host:        "0.0.0.0",
					Port:        "8080",
					SSLCertFile: "testdata/cert.pem",
					SSLKeyFile:  "testdata/key.pem",
				},
				wantErr: false,
			},
			{
				name: "valid yml extension",
				yamlConfig: `
host: 127.0.0.1
port: "9000"
`,
				fileName: "config.yml",
				want: ServerConfig{
					Host: "127.0.0.1",
					Port: "9000",
				},
				wantErr: false,
			},
			{
				name: "yaml config without ssl",
				yamlConfig: `
host: localhost
port: "3000"
`,
				fileName: "config.yaml",
				want: ServerConfig{
					Host: "localhost",
					Port: "3000",
				},
				wantErr: false,
			},
			{
				name:       "invalid yaml",
				yamlConfig: `invalid: yaml: syntax: error`,
				fileName:   "config.yaml",
				wantErr:    true,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				// Create temporary YAML config file
				tmpDir := t.TempDir()
				configFile := filepath.Join(tmpDir, tt.fileName)
				if err := os.WriteFile(configFile, []byte(tt.yamlConfig), 0644); err != nil {
					t.Fatalf("Failed to create test config file: %v", err)
				}

				// Setup test SSL files if needed
				if tt.want.SSLCertFile != "" {
					setupTestSSLFiles(t)
					defer cleanupTestSSLFiles(t)
				}

				// Save original os.Args and restore after test
				oldArgs := os.Args
				defer func() { os.Args = oldArgs }()

				// Set args with config file
				os.Args = []string{"test", "--config=" + configFile}

				config := NewConfig()
				err := config.Load()

				if (err != nil) != tt.wantErr {
					t.Errorf("Load() error = %v, wantErr %v", err, tt.wantErr)
					return
				}

				if !tt.wantErr {
					if config.Host != tt.want.Host {
						t.Errorf("Host = %v, want %v", config.Host, tt.want.Host)
					}
					if config.Port != tt.want.Port {
						t.Errorf("Port = %v, want %v", config.Port, tt.want.Port)
					}
					if config.SSLCertFile != tt.want.SSLCertFile {
						t.Errorf("SSLCertFile = %v, want %v", config.SSLCertFile, tt.want.SSLCertFile)
					}
					if config.SSLKeyFile != tt.want.SSLKeyFile {
						t.Errorf("SSLKeyFile = %v, want %v", config.SSLKeyFile, tt.want.SSLKeyFile)
					}
				}
			})
		}
	})

	t.Run("LoadNegative", func(t *testing.T) {
		t.Run("MissingConfigFile", func(t *testing.T) {
			// Save original os.Args and restore after test
			oldArgs := os.Args
			defer func() { os.Args = oldArgs }()

			// Set args without config file
			os.Args = []string{"test"}

			config := NewConfig()
			err := config.Load()

			if err == nil {
				t.Error("Load() expected error for missing config file, got nil")
			}
		})

		t.Run("UnsupportedFormat", func(t *testing.T) {
			// Create temporary config file with unsupported extension
			tmpDir := t.TempDir()
			configFile := filepath.Join(tmpDir, "config.toml")
			if err := os.WriteFile(configFile, []byte("host = \"localhost\""), 0644); err != nil {
				t.Fatalf("Failed to create test config file: %v", err)
			}

			// Save original os.Args and restore after test
			oldArgs := os.Args
			defer func() { os.Args = oldArgs }()

			// Set args with config file
			os.Args = []string{"test", "--config=" + configFile}

			config := NewConfig()
			err := config.Load()

			if err == nil {
				t.Error("Load() expected error for unsupported format, got nil")
			}
		})

		t.Run("FileNotFound", func(t *testing.T) {
			// Save original os.Args and restore after test
			oldArgs := os.Args
			defer func() { os.Args = oldArgs }()

			// Set args with non-existent config file
			os.Args = []string{"test", "--config=/nonexistent/config.yaml"}

			config := NewConfig()
			err := config.Load()

			if err == nil {
				t.Error("Load() expected error for non-existent file, got nil")
			}
		})

	})
}

// Helper functions
func setupTestSSLFiles(t *testing.T) {
	// Create testdata directory
	if err := os.MkdirAll("testdata", 0755); err != nil {
		t.Fatalf("Failed to create testdata directory: %v", err)
	}

	// Create dummy cert file
	certContent := []byte("-----BEGIN CERTIFICATE-----\nDUMMY CERT\n-----END CERTIFICATE-----")
	if err := os.WriteFile("testdata/cert.pem", certContent, 0644); err != nil {
		t.Fatalf("Failed to create cert file: %v", err)
	}

	// Create dummy key file
	keyContent := []byte("-----BEGIN PRIVATE KEY-----\nDUMMY KEY\n-----END PRIVATE KEY-----")
	if err := os.WriteFile("testdata/key.pem", keyContent, 0644); err != nil {
		t.Fatalf("Failed to create key file: %v", err)
	}
}

func cleanupTestSSLFiles(t *testing.T) {
	if err := os.RemoveAll("testdata"); err != nil {
		t.Logf("Failed to cleanup testdata directory: %v", err)
	}
}
