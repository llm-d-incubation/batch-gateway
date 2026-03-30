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

// Package config provides configuration loading and validation for the batch garbage collector.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/llm-d-incubation/batch-gateway/internal/database/postgresql"
	fsclient "github.com/llm-d-incubation/batch-gateway/internal/files_store/fs"
	s3client "github.com/llm-d-incubation/batch-gateway/internal/files_store/s3"
	uredis "github.com/llm-d-incubation/batch-gateway/internal/util/redis"
	"github.com/llm-d-incubation/batch-gateway/internal/util/retry"
)

const (
	// DefaultMaxConcurrency is the default number of concurrent item deletions per GC cycle.
	DefaultMaxConcurrency = 10
)

// Config holds the garbage collector configuration.
type Config struct {
	DryRun         bool          `yaml:"dry_run"`
	Interval       time.Duration `yaml:"interval"`
	MaxConcurrency int           `yaml:"max_concurrency"`

	// DatabaseType selects which backend stores persistent data (batch_items, file_items).
	// Must be "redis" or "postgresql".
	DatabaseType string `yaml:"database_type"`

	// RedisCfg holds Redis client connection and tuning settings.
	// URL is resolved from a mounted K8s Secret at runtime, not from this config.
	RedisCfg uredis.RedisClientConfig `yaml:"redis"`

	// PostgreSQLCfg holds PostgreSQL connection settings.
	// URL is resolved from a mounted K8s Secret at runtime, not from this config.
	PostgreSQLCfg postgresql.PostgreSQLConfig `yaml:"postgresql"`

	// FileClientCfg holds the file storage backend configuration.
	FileClientCfg FileClientConfig `yaml:"file_client"`
}

// FileClientConfig holds the file storage client configuration.
// Type selects the active backend; both FS and S3 connection configs
// may be present simultaneously (matching apiserver/processor pattern).
type FileClientConfig struct {
	// Type selects the file storage backend. Must be "fs" or "s3".
	Type     string          `yaml:"type"`
	FSConfig fsclient.Config `yaml:"fs"`
	S3Config s3client.Config `yaml:"s3"`
	Retry    retry.Config    `yaml:"retry"`
}

// Load reads and validates a Config from the given YAML file path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	cfg := &Config{
		Interval:       1 * time.Hour,
		MaxConcurrency: DefaultMaxConcurrency,
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if cfg.MaxConcurrency <= 0 {
		return nil, fmt.Errorf("max_concurrency must be positive, got %d", cfg.MaxConcurrency)
	}

	if cfg.Interval <= 0 {
		return nil, fmt.Errorf("interval must be positive, got %v", cfg.Interval)
	}

	switch cfg.DatabaseType {
	case "redis", "postgresql":
		// valid
	case "":
		return nil, fmt.Errorf("database_type is required (must be \"redis\" or \"postgresql\")")
	default:
		return nil, fmt.Errorf("database_type must be \"redis\" or \"postgresql\", got %q", cfg.DatabaseType)
	}

	switch cfg.FileClientCfg.Type {
	case "fs", "s3":
		// valid
	case "":
		return nil, fmt.Errorf("file_client.type is required (must be \"fs\" or \"s3\")")
	default:
		return nil, fmt.Errorf("file_client.type must be \"fs\" or \"s3\", got %q", cfg.FileClientCfg.Type)
	}

	if err := cfg.FileClientCfg.Retry.Validate(); err != nil {
		return nil, fmt.Errorf("file_client.retry: %w", err)
	}

	return cfg, nil
}
