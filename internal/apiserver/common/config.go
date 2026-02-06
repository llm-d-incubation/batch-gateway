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

// The file implements server configuration management and validation for API server.
package common

import (
	"flag"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
	"k8s.io/klog/v2"
)

const (
	// DefaultMaxFileSizeBytes is the default maximum file size (200 MB)
	DefaultMaxFileSizeBytes = 200 << 20
	// DefaultFileExpirationSeconds is the default file expiration time (90 days)
	DefaultFileExpirationSeconds = 90 * 24 * 60 * 60 // 7776000 seconds
	// DefaultBatchTTLSeconds is the default batch TTL (30 days)
	DefaultBatchTTLSeconds = 30 * 24 * 60 * 60 // 2592000 seconds
	// DefaultMaxFileLineCount is the default maximum number of lines per file
	DefaultMaxFileLineCount = 50000
)

type FilesAPIConfig struct {
	DefaultExpirationSeconds int64 `yaml:"default_expiration_seconds"`
	MaxSizeBytes             int64 `yaml:"max_size_bytes"`
	MaxLineCount             int64 `yaml:"max_line_count"`
}

type ServerConfig struct {
	Host            string         `yaml:"host"`
	Port            string         `yaml:"port"`
	SSLCertFile     string         `yaml:"ssl_cert_file"`
	SSLKeyFile      string         `yaml:"ssl_key_file"`
	BatchTTLSeconds int            `yaml:"batch_ttl_seconds"`
	FilesAPI        FilesAPIConfig `yaml:"files_api"`
}

func NewConfig() *ServerConfig {
	return &ServerConfig{}
}

func (c *ServerConfig) Load() error {
	// Initialize flags (including klog flags)
	fs := flag.NewFlagSet("batch-gateway-apiserver", flag.ContinueOnError)
	klog.InitFlags(fs)

	var configFile string
	fs.StringVar(&configFile, "config", "cmd/apiserver/config.yaml", "path to YAML config file")

	// Parse all flags (klog flags and application flags)
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	if err := c.loadFromFile(configFile); err != nil {
		return err
	}

	return c.Validate()
}

func (c *ServerConfig) Validate() error {
	if c.Port == "" {
		return fmt.Errorf("port cannot be empty")
	}

	// If one SSL file is provided, both must be provided
	if (c.SSLCertFile != "" && c.SSLKeyFile == "") || (c.SSLCertFile == "" && c.SSLKeyFile != "") {
		return fmt.Errorf("both ssl-cert-file and ssl-private-key-file must be provided together")
	}

	// Verify SSL files exist if provided
	if c.SSLCertFile != "" {
		if _, err := os.Stat(c.SSLCertFile); err != nil {
			return fmt.Errorf("ssl cert file not found: %w", err)
		}
		if _, err := os.Stat(c.SSLKeyFile); err != nil {
			return fmt.Errorf("ssl key file not found: %w", err)
		}
	}

	return nil
}

func (c *ServerConfig) loadFromFile(path string) error {
	if path == "" {
		return fmt.Errorf("config file path cannot be empty")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, c); err != nil {
		return fmt.Errorf("failed to parse YAML config file: %w", err)
	}

	return nil
}

func (c *ServerConfig) SSLEnabled() bool {
	return (c.SSLCertFile != "" && c.SSLKeyFile != "")
}

func (c *ServerConfig) GetBatchTTLSeconds() int {
	if c.BatchTTLSeconds > 0 {
		return c.BatchTTLSeconds
	}
	// Default to 30 days if not configured
	return DefaultBatchTTLSeconds
}

func (c *ServerConfig) GetMaxFileSizeBytes() int64 {
	if c.FilesAPI.MaxSizeBytes > 0 {
		return c.FilesAPI.MaxSizeBytes
	}
	// The file can contain up to 50,000 requests, and can be up to 200 MB in size.
	// Line counting is enforced via LineCountingReader in the file upload handler.
	return DefaultMaxFileSizeBytes
}

func (c *ServerConfig) GetFileDefaultExpirationSeconds() int64 {
	if c.FilesAPI.DefaultExpirationSeconds > 0 {
		return c.FilesAPI.DefaultExpirationSeconds
	}
	// Default to 90 days if not configured
	return DefaultFileExpirationSeconds
}

func (c *ServerConfig) GetMaxFileLineCount() int64 {
	if c.FilesAPI.MaxLineCount > 0 {
		return c.FilesAPI.MaxLineCount
	}
	// Default to 50,000 lines if not configured
	return DefaultMaxFileLineCount
}
