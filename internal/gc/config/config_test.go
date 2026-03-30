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

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_FilesystemBackend(t *testing.T) {
	path := writeTempConfig(t, `
dry_run: true
interval: 30m
database_type: "postgresql"
file_client:
  type: "fs"
  fs:
    base_path: "/data/files"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.DryRun {
		t.Error("expected dry_run to be true")
	}
	if cfg.Interval != 30*time.Minute {
		t.Errorf("expected interval 30m, got %v", cfg.Interval)
	}
	if cfg.FileClientCfg.Type != "fs" {
		t.Errorf("expected file_client.type fs, got %s", cfg.FileClientCfg.Type)
	}
	if cfg.FileClientCfg.FSConfig.BasePath != "/data/files" {
		t.Errorf("expected base_path /data/files, got %s", cfg.FileClientCfg.FSConfig.BasePath)
	}
}

func TestLoad_S3Backend(t *testing.T) {
	path := writeTempConfig(t, `
database_type: "postgresql"
file_client:
  type: "s3"
  s3:
    region: "us-east-1"
    endpoint: "https://s3.example.com"
    access_key_id: "AKID"
    prefix: "batch/"
    use_path_style: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FileClientCfg.Type != "s3" {
		t.Errorf("expected file_client.type s3, got %s", cfg.FileClientCfg.Type)
	}
	if cfg.FileClientCfg.S3Config.Region != "us-east-1" {
		t.Errorf("expected region us-east-1, got %s", cfg.FileClientCfg.S3Config.Region)
	}
	if cfg.FileClientCfg.S3Config.Endpoint != "https://s3.example.com" {
		t.Errorf("expected endpoint https://s3.example.com, got %s", cfg.FileClientCfg.S3Config.Endpoint)
	}
	if cfg.FileClientCfg.S3Config.AccessKeyID != "AKID" {
		t.Errorf("expected access_key_id AKID, got %s", cfg.FileClientCfg.S3Config.AccessKeyID)
	}
	if cfg.FileClientCfg.S3Config.Prefix != "batch/" {
		t.Errorf("expected prefix batch/, got %s", cfg.FileClientCfg.S3Config.Prefix)
	}
	if !cfg.FileClientCfg.S3Config.UsePathStyle {
		t.Error("expected use_path_style to be true")
	}
}

func TestLoad_RedisDatabase(t *testing.T) {
	path := writeTempConfig(t, `
database_type: "redis"
redis:
  db: 2
  enable_tls: true
file_client:
  type: "fs"
  fs:
    base_path: "/tmp/files"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DatabaseType != "redis" {
		t.Errorf("expected database_type redis, got %s", cfg.DatabaseType)
	}
	if cfg.RedisCfg.DB != 2 {
		t.Errorf("expected redis db 2, got %d", cfg.RedisCfg.DB)
	}
	if !cfg.RedisCfg.EnableTLS {
		t.Error("expected redis enable_tls to be true")
	}
}

func TestLoad_PostgreSQLDatabase(t *testing.T) {
	path := writeTempConfig(t, `
database_type: "postgresql"
file_client:
  type: "fs"
  fs:
    base_path: "/tmp/files"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DatabaseType != "postgresql" {
		t.Errorf("expected database_type postgresql, got %s", cfg.DatabaseType)
	}
}

func TestLoad_BothDatabaseConfigs(t *testing.T) {
	// Both redis and postgresql connectivity configs can be present;
	// database_type selects which is used for tables.
	path := writeTempConfig(t, `
database_type: "postgresql"
redis:
  db: 1
  enable_tls: false
postgresql: {}
file_client:
  type: "fs"
  fs:
    base_path: "/tmp/files"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DatabaseType != "postgresql" {
		t.Errorf("expected database_type postgresql, got %s", cfg.DatabaseType)
	}
	if cfg.RedisCfg.DB != 1 {
		t.Errorf("expected redis db 1, got %d", cfg.RedisCfg.DB)
	}
}

func TestLoad_Defaults(t *testing.T) {
	path := writeTempConfig(t, `
database_type: "postgresql"
file_client:
  type: "fs"
  fs:
    base_path: "/tmp/files"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DryRun {
		t.Error("expected dry_run to default to false")
	}
	if cfg.Interval != 1*time.Hour {
		t.Errorf("expected default interval 1h, got %v", cfg.Interval)
	}
	if cfg.MaxConcurrency != DefaultMaxConcurrency {
		t.Errorf("expected default max_concurrency %d, got %d", DefaultMaxConcurrency, cfg.MaxConcurrency)
	}
}

func TestLoad_CustomMaxConcurrency(t *testing.T) {
	path := writeTempConfig(t, `
database_type: "postgresql"
max_concurrency: 20
file_client:
  type: "fs"
  fs:
    base_path: "/tmp/files"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxConcurrency != 20 {
		t.Errorf("expected max_concurrency 20, got %d", cfg.MaxConcurrency)
	}
}

func TestLoad_ErrorZeroMaxConcurrency(t *testing.T) {
	path := writeTempConfig(t, `
database_type: "postgresql"
max_concurrency: 0
file_client:
  type: "fs"
  fs:
    base_path: "/tmp/files"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for zero max_concurrency")
	}
}

func TestLoad_ErrorNegativeMaxConcurrency(t *testing.T) {
	path := writeTempConfig(t, `
database_type: "postgresql"
max_concurrency: -5
file_client:
  type: "fs"
  fs:
    base_path: "/tmp/files"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for negative max_concurrency")
	}
}

func TestLoad_ErrorNoDatabaseType(t *testing.T) {
	path := writeTempConfig(t, `
file_client:
  type: "fs"
  fs:
    base_path: "/tmp/files"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error when database_type is missing")
	}
}

func TestLoad_ErrorInvalidDatabaseType(t *testing.T) {
	path := writeTempConfig(t, `
database_type: "mysql"
file_client:
  type: "fs"
  fs:
    base_path: "/tmp/files"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid database_type")
	}
}

func TestLoad_ErrorNoFileClientType(t *testing.T) {
	path := writeTempConfig(t, `
database_type: "postgresql"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error when file_client.type is missing")
	}
}

func TestLoad_ErrorInvalidFileClientType(t *testing.T) {
	path := writeTempConfig(t, `
database_type: "postgresql"
file_client:
  type: "gcs"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid file_client.type")
	}
}

func TestLoad_ErrorMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoad_ErrorInvalidYAML(t *testing.T) {
	path := writeTempConfig(t, `{{{invalid`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoad_ErrorZeroInterval(t *testing.T) {
	path := writeTempConfig(t, `
interval: 0s
database_type: "postgresql"
file_client:
  type: "fs"
  fs:
    base_path: "/tmp/files"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for zero interval")
	}
}

func TestLoad_ErrorNegativeInterval(t *testing.T) {
	path := writeTempConfig(t, `
interval: -5m
database_type: "postgresql"
file_client:
  type: "fs"
  fs:
    base_path: "/tmp/files"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for negative interval")
	}
}

func TestLoad_RedisConfigTuning(t *testing.T) {
	path := writeTempConfig(t, `
database_type: "redis"
redis:
  db: 3
  enable_tls: true
  insecure: true
  timeout: "5s"
  max_retries: 5
  min_retry_backoff: "100ms"
  max_retry_backoff: "2s"
  pool_timeout: "10s"
  conn_max_idle_time: "5m"
  conn_max_lifetime: "30m"
file_client:
  type: "fs"
  fs:
    base_path: "/tmp/files"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RedisCfg.DB != 3 {
		t.Errorf("expected db 3, got %d", cfg.RedisCfg.DB)
	}
	if !cfg.RedisCfg.EnableTLS {
		t.Error("expected enable_tls to be true")
	}
	if !cfg.RedisCfg.Insecure {
		t.Error("expected insecure to be true")
	}
	if cfg.RedisCfg.Timeout != 5*time.Second {
		t.Errorf("expected timeout 5s, got %v", cfg.RedisCfg.Timeout)
	}
	if cfg.RedisCfg.MaxRetries != 5 {
		t.Errorf("expected max_retries 5, got %d", cfg.RedisCfg.MaxRetries)
	}
	if cfg.RedisCfg.MinRetryBackoff != 100*time.Millisecond {
		t.Errorf("expected min_retry_backoff 100ms, got %v", cfg.RedisCfg.MinRetryBackoff)
	}
	if cfg.RedisCfg.MaxRetryBackoff != 2*time.Second {
		t.Errorf("expected max_retry_backoff 2s, got %v", cfg.RedisCfg.MaxRetryBackoff)
	}
	if cfg.RedisCfg.PoolTimeout != 10*time.Second {
		t.Errorf("expected pool_timeout 10s, got %v", cfg.RedisCfg.PoolTimeout)
	}
	if cfg.RedisCfg.ConnMaxIdleTime != 5*time.Minute {
		t.Errorf("expected conn_max_idle_time 5m, got %v", cfg.RedisCfg.ConnMaxIdleTime)
	}
	if cfg.RedisCfg.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("expected conn_max_lifetime 30m, got %v", cfg.RedisCfg.ConnMaxLifetime)
	}
}
