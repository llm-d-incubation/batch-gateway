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

	sharedcfg "github.com/llm-d-incubation/batch-gateway/internal/shared/config"
)

const (
	// DefaultMaxConcurrency is the default number of concurrent item deletions per GC cycle.
	DefaultMaxConcurrency = 10

	// DefaultReconcilerInterval is the default interval between orphan reconciler cycles.
	// It also serves as the staleness threshold for in-flight entries.
	DefaultReconcilerInterval = 60 * time.Minute
)

// ReconcilerConfig holds the orphan reconciler configuration.
type ReconcilerConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
}

// CollectorConfig holds collector-specific settings (interval and concurrency).
type CollectorConfig struct {
	Interval       time.Duration `yaml:"interval"`
	MaxConcurrency int           `yaml:"max_concurrency"`
}

// Config holds the garbage collector configuration.
type Config struct {
	DryRun bool `yaml:"dry_run"`

	// Collector holds the collector-specific configuration (interval, concurrency).
	Collector CollectorConfig `yaml:"collector"`

	// Reconciler configures the orphan reconciler that detects and recovers
	// batch jobs stuck in non-terminal states.
	Reconciler ReconcilerConfig `yaml:"reconciler"`

	// DB client configuration
	DBClientCfg sharedcfg.DBClientConfig `yaml:"db_client"`

	// FileClientCfg holds the file storage backend configuration.
	FileClientCfg sharedcfg.FileClientConfig `yaml:"file_client"`
}

// Load reads and validates a Config from the given YAML file path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	cfg := &Config{
		Collector: CollectorConfig{
			Interval:       1 * time.Hour,
			MaxConcurrency: DefaultMaxConcurrency,
		},
		Reconciler: ReconcilerConfig{
			Enabled:  true,
			Interval: DefaultReconcilerInterval,
		},
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if cfg.Collector.MaxConcurrency <= 0 {
		return nil, fmt.Errorf("collector.max_concurrency must be positive, got %d", cfg.Collector.MaxConcurrency)
	}

	if cfg.Collector.Interval <= 0 {
		return nil, fmt.Errorf("collector.interval must be positive, got %v", cfg.Collector.Interval)
	}

	if cfg.Reconciler.Enabled && cfg.Reconciler.Interval <= 0 {
		return nil, fmt.Errorf("reconciler.interval must be positive when enabled, got %v", cfg.Reconciler.Interval)
	}

	switch cfg.DBClientCfg.Type {
	case sharedcfg.DBTypeRedis, sharedcfg.DBTypeValkey, sharedcfg.DBTypePostgreSQL:
		// valid
	case "":
		return nil, fmt.Errorf("db_client.type is required (must be \"redis\", \"valkey\", or \"postgresql\")")
	default:
		return nil, fmt.Errorf("db_client.type must be \"redis\", \"valkey\", or \"postgresql\", got %q", cfg.DBClientCfg.Type)
	}

	switch cfg.FileClientCfg.Type {
	case sharedcfg.FileTypeFS, sharedcfg.FileTypeS3:
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
