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

	"github.com/llm-d-incubation/batch-gateway/internal/database/postgresql"
	fsclient "github.com/llm-d-incubation/batch-gateway/internal/files_store/fs"
	s3client "github.com/llm-d-incubation/batch-gateway/internal/files_store/s3"
	"gopkg.in/yaml.v3"
	"k8s.io/klog/v2"
)

const (
	// DefaultMaxFileSizeBytes is the default maximum file size (200 MB)
	DefaultMaxFileSizeBytes = 200 << 20
	// DefaultFileExpirationSeconds is the default file expiration time (90 days)
	DefaultFileExpirationSeconds = 90 * 24 * 60 * 60 // 7776000 seconds
	// DefaultBatchEventTTLSeconds is the default batch event TTL (30 days)
	DefaultBatchEventTTLSeconds = 30 * 24 * 60 * 60 // 2592000 seconds
	// DefaultMaxFileLineCount is the default maximum number of lines per file
	DefaultMaxFileLineCount = 50000
	// DefaultTenantHeader is the default HTTP header name for tenant ID
	DefaultTenantHeader = "X-MaaS-Username"

	// HTTP server timeout defaults
	// DefaultReadHeaderTimeoutSeconds prevents slow-client attacks (Slowloris)
	DefaultReadHeaderTimeoutSeconds = 10
	// DefaultReadTimeoutSeconds includes reading request headers and body
	// Must accommodate large file uploads (up to 200MB)
	// Supports upload speeds as low as ~2.8 Mbps
	DefaultReadTimeoutSeconds = 900 // 15 minutes
	// DefaultWriteTimeoutSeconds includes writing response
	// Must accommodate large batch query results
	DefaultWriteTimeoutSeconds = 120 // 2 minutes
	// DefaultIdleTimeoutSeconds for keep-alive connections
	DefaultIdleTimeoutSeconds = 90
)

type BatchAPIConfig struct {
	BatchEventTTLSeconds int      `yaml:"batch_event_ttl_seconds"`
	PassThroughHeaders   []string `yaml:"pass_through_headers"`
}

func (b *BatchAPIConfig) applyDefaults() {
	if b.BatchEventTTLSeconds <= 0 {
		b.BatchEventTTLSeconds = DefaultBatchEventTTLSeconds
	}
}

func (b *BatchAPIConfig) GetBatchEventTTLSeconds() int {
	return b.BatchEventTTLSeconds
}

type FileAPIConfig struct {
	DefaultExpirationSeconds int64 `yaml:"default_expiration_seconds"`
	MaxSizeBytes             int64 `yaml:"max_size_bytes"`
	MaxLineCount             int64 `yaml:"max_line_count"`
}

func (f *FileAPIConfig) applyDefaults() {
	if f.DefaultExpirationSeconds <= 0 {
		f.DefaultExpirationSeconds = DefaultFileExpirationSeconds
	}
	if f.MaxSizeBytes <= 0 {
		f.MaxSizeBytes = DefaultMaxFileSizeBytes
	}
	if f.MaxLineCount <= 0 {
		f.MaxLineCount = DefaultMaxFileLineCount
	}
}

func (f *FileAPIConfig) GetDefaultExpirationSeconds() int64 {
	return f.DefaultExpirationSeconds
}

func (f *FileAPIConfig) GetMaxSizeBytes() int64 {
	return f.MaxSizeBytes
}

func (f *FileAPIConfig) GetMaxLineCount() int64 {
	return f.MaxLineCount
}

type ServerConfig struct {
	Host              string `yaml:"host"`
	Port              string `yaml:"port"`
	ObservabilityPort string `yaml:"observability_port"`
	SSLCertFile       string `yaml:"ssl_cert_file"`
	SSLKeyFile        string `yaml:"ssl_key_file"`
	TenantHeader      string `yaml:"tenant_header"`

	// HTTP server timeout configurations (in seconds)
	ReadHeaderTimeoutSeconds int64 `yaml:"read_header_timeout_seconds"`
	ReadTimeoutSeconds       int64 `yaml:"read_timeout_seconds"`
	WriteTimeoutSeconds      int64 `yaml:"write_timeout_seconds"`
	IdleTimeoutSeconds       int64 `yaml:"idle_timeout_seconds"`

	// API endpoint configurations
	BatchAPI BatchAPIConfig `yaml:"batch_api"`
	FileAPI  FileAPIConfig  `yaml:"file_api"`

	// Files client configuration
	FileClientCfg struct {
		Type     string          `yaml:"type"`
		FSConfig fsclient.Config `yaml:"fs"`
		S3Config s3client.Config `yaml:"s3"`
	} `yaml:"file_client"`

	// DatabaseType specifies the database backend: "mock", "redis", or "postgresql".
	DatabaseType string `yaml:"database_type"`

	// PostgreSQLCfg holds PostgreSQL connection settings (used when DatabaseType is "postgresql").
	PostgreSQLCfg postgresql.PostgreSQLConfig `yaml:"postgresql"`

	// OTel holds OpenTelemetry-related settings.
	OTel struct {
		RedisTracing      bool `yaml:"redis_tracing"`
		PostgresqlTracing bool `yaml:"postgresql_tracing"`
	} `yaml:"otel"`
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

	c.applyDefaults()

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

func (c *ServerConfig) applyDefaults() {
	if c.ObservabilityPort == "" {
		c.ObservabilityPort = "8081"
	}
	if c.DatabaseType == "" {
		c.DatabaseType = "redis"
	}
	if c.TenantHeader == "" {
		c.TenantHeader = DefaultTenantHeader
	}
	if c.ReadHeaderTimeoutSeconds <= 0 {
		c.ReadHeaderTimeoutSeconds = DefaultReadHeaderTimeoutSeconds
	}
	if c.ReadTimeoutSeconds <= 0 {
		c.ReadTimeoutSeconds = DefaultReadTimeoutSeconds
	}
	if c.WriteTimeoutSeconds <= 0 {
		c.WriteTimeoutSeconds = DefaultWriteTimeoutSeconds
	}
	if c.IdleTimeoutSeconds <= 0 {
		c.IdleTimeoutSeconds = DefaultIdleTimeoutSeconds
	}
	c.BatchAPI.applyDefaults()
	c.FileAPI.applyDefaults()
}

func (c *ServerConfig) SSLEnabled() bool {
	return (c.SSLCertFile != "" && c.SSLKeyFile != "")
}

func (c *ServerConfig) GetReadHeaderTimeoutSeconds() int64 {
	return c.ReadHeaderTimeoutSeconds
}

func (c *ServerConfig) GetReadTimeoutSeconds() int64 {
	return c.ReadTimeoutSeconds
}

func (c *ServerConfig) GetWriteTimeoutSeconds() int64 {
	return c.WriteTimeoutSeconds
}

func (c *ServerConfig) GetIdleTimeoutSeconds() int64 {
	return c.IdleTimeoutSeconds
}

func (c *ServerConfig) GetTenantHeader() string {
	return c.TenantHeader
}
