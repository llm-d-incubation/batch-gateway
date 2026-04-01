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

// Package config provides shared configuration types used by apiserver, processor, and gc.
package config

import (
	"github.com/llm-d-incubation/batch-gateway/internal/database/postgresql"
	fsclient "github.com/llm-d-incubation/batch-gateway/internal/files_store/fs"
	s3client "github.com/llm-d-incubation/batch-gateway/internal/files_store/s3"
	uredis "github.com/llm-d-incubation/batch-gateway/internal/util/redis"
	"github.com/llm-d-incubation/batch-gateway/internal/util/retry"
)

// DBClientConfig holds database client configuration shared by all components.
type DBClientConfig struct {
	// Type specifies the database backend: "mock", "redis", or "postgresql".
	Type string `yaml:"type"`
	// PostgreSQLCfg holds PostgreSQL connection settings (used when Type is "postgresql").
	PostgreSQLCfg postgresql.PostgreSQLConfig `yaml:"postgresql"`
	// RedisCfg holds Redis client settings (timeouts, retries, pool, TLS).
	// URL, ServiceName, EnableTracing, and Certificates are set at runtime, not from YAML.
	RedisCfg uredis.RedisClientConfig `yaml:"redis"`
}

// FileClientConfig holds file storage client configuration shared by all components.
type FileClientConfig struct {
	Type     string          `yaml:"type"`
	FSConfig fsclient.Config `yaml:"fs"`
	S3Config s3client.Config `yaml:"s3"`
	Retry    retry.Config    `yaml:"retry"`
}

// OTelConfig holds OpenTelemetry-related settings shared by apiserver and processor.
type OTelConfig struct {
	RedisTracing      bool `yaml:"redis_tracing"`
	PostgresqlTracing bool `yaml:"postgresql_tracing"`
}
