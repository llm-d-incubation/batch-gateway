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

func TestNewConfig_Defaults(t *testing.T) {
	c := NewConfig()
	if c == nil {
		t.Fatalf("NewConfig returned nil")
	}

	// spot-check defaults
	if c.PollInterval != 5*time.Second {
		t.Fatalf("PollInterval = %v, want %v", c.PollInterval, 5*time.Second)
	}
	if c.TaskWaitTime != 1*time.Second {
		t.Fatalf("TaskWaitTime = %v, want %v", c.TaskWaitTime, 1*time.Second)
	}
	if c.NumWorkers != 1 {
		t.Fatalf("NumWorkers = %d, want %d", c.NumWorkers, 1)
	}
	if c.GlobalConcurrency != 100 {
		t.Fatalf("GlobalConcurrency = %d, want %d", c.GlobalConcurrency, 100)
	}
	if c.PerModelMaxConcurrency != 10 {
		t.Fatalf("PerModelMaxConcurrency = %d, want %d", c.PerModelMaxConcurrency, 10)
	}
	if c.WorkDir == "" {
		t.Fatalf("WorkDir should not be empty")
	}
	if c.DatabaseType != "redis" {
		t.Fatalf("DatabaseType = %q, want %q", c.DatabaseType, "redis")
	}
	// inference config spot-check
	if c.InferenceConfig.GatewayURL != "http://localhost:8000" {
		t.Fatalf("GatewayURL = %q, want %q", c.InferenceConfig.GatewayURL, "http://localhost:8000")
	}
	if c.InferenceConfig.RequestTimeout != 5*time.Minute {
		t.Fatalf("RequestTimeout = %v, want %v", c.InferenceConfig.RequestTimeout, 5*time.Minute)
	}
	if c.InferenceConfig.MaxRetries != 3 {
		t.Fatalf("MaxRetries = %d, want %d", c.InferenceConfig.MaxRetries, 3)
	}

	// upload retry spot-check
	if c.UploadRetry.MaxRetries != 3 {
		t.Fatalf("UploadRetry.MaxRetries = %d, want %d", c.UploadRetry.MaxRetries, 3)
	}
	if c.UploadRetry.InitialBackoff != 1*time.Second {
		t.Fatalf("UploadRetry.InitialBackoff = %v, want %v", c.UploadRetry.InitialBackoff, 1*time.Second)
	}
	if c.UploadRetry.MaxBackoff != 10*time.Second {
		t.Fatalf("UploadRetry.MaxBackoff = %v, want %v", c.UploadRetry.MaxBackoff, 10*time.Second)
	}

	// output expiration default: 90 days
	want90Days := int64(90 * 24 * 60 * 60)
	if c.DefaultOutputExpirationSeconds != want90Days {
		t.Fatalf("DefaultOutputExpirationSeconds = %d, want %d", c.DefaultOutputExpirationSeconds, want90Days)
	}

	// progress TTL default: 24 hours
	if c.ProgressTTLSeconds != 86400 {
		t.Fatalf("ProgressTTLSeconds = %d, want %d", c.ProgressTTLSeconds, 86400)
	}
}

func TestProcessorConfig_SSLEnabled(t *testing.T) {
	c := NewConfig()

	c.SSLCertFile = ""
	c.SSLKeyFile = ""
	if c.SSLEnabled() {
		t.Fatalf("SSLEnabled() = true, want false when both empty")
	}

	c.SSLCertFile = "/tmp/cert"
	c.SSLKeyFile = ""
	if c.SSLEnabled() {
		t.Fatalf("SSLEnabled() = true, want false when key empty")
	}

	c.SSLCertFile = ""
	c.SSLKeyFile = "/tmp/key"
	if c.SSLEnabled() {
		t.Fatalf("SSLEnabled() = true, want false when cert empty")
	}

	c.SSLCertFile = "/tmp/cert"
	c.SSLKeyFile = "/tmp/key"
	if !c.SSLEnabled() {
		t.Fatalf("SSLEnabled() = false, want true when both set")
	}
}

func TestProcessorConfig_Validate_WorkDirEmpty(t *testing.T) {
	c := NewConfig()
	c.WorkDir = ""
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error for empty WorkDir, got nil")
	}
}

func TestProcessorConfig_Validate_TaskWaitTimeMustBeShorterThanPollInterval(t *testing.T) {
	c := NewConfig()
	c.DatabaseType = "mock"
	c.PollInterval = 1 * time.Second
	c.TaskWaitTime = 1 * time.Second
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error when task_wait_time >= poll_interval, got nil")
	}

	c.TaskWaitTime = 500 * time.Millisecond
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() unexpected error when task_wait_time < poll_interval: %v", err)
	}
}

func TestProcessorConfig_Validate_SSLDisabled_DoesNotRequireCertFiles(t *testing.T) {
	c := NewConfig()
	c.DatabaseType = "mock"
	c.SSLCertFile = ""
	c.SSLKeyFile = ""
	// WorkDir is not empty (default)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() unexpected error when SSL disabled: %v", err)
	}
}

func TestProcessorConfig_Validate_SSLEnabled_RequiresExistingFiles(t *testing.T) {
	dir := t.TempDir()

	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	// dummy file generation
	if err := os.WriteFile(certPath, []byte("dummy"), 0o600); err != nil {
		t.Fatalf("failed to write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("dummy"), 0o600); err != nil {
		t.Fatalf("failed to write key: %v", err)
	}

	c := NewConfig()
	c.DatabaseType = "mock"
	c.SSLCertFile = certPath
	c.SSLKeyFile = keyPath

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() unexpected error with existing cert/key: %v", err)
	}
}

func TestProcessorConfig_Validate_SSLEnabled_MissingCertOrKey(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	c := NewConfig()
	c.SSLCertFile = certPath
	c.SSLKeyFile = keyPath

	// not existing > error
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error when cert/key missing, got nil")
	}

	// only cert file > error
	if err := os.WriteFile(certPath, []byte("dummy"), 0o600); err != nil {
		t.Fatalf("failed to write cert: %v", err)
	}
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error when key missing, got nil")
	}
}

func TestProcessorConfig_Validate_SSLPartialConfigRejected(t *testing.T) {
	c := NewConfig()
	c.SSLCertFile = "/tmp/only-cert.pem"
	c.SSLKeyFile = ""
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error when only ssl_cert_file is set, got nil")
	}

	c.SSLCertFile = ""
	c.SSLKeyFile = "/tmp/only-key.pem"
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error when only ssl_key_file is set, got nil")
	}
}

func TestProcessorConfig_Validate_InferenceTLSPartialConfigRejected(t *testing.T) {
	c := NewConfig()
	c.InferenceConfig.TLSClientCertFile = "/tmp/client-cert.pem"
	c.InferenceConfig.TLSClientKeyFile = ""
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error when only tls_client_cert_file is set, got nil")
	}

	c.InferenceConfig.TLSClientCertFile = ""
	c.InferenceConfig.TLSClientKeyFile = "/tmp/client-key.pem"
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error when only tls_client_key_file is set, got nil")
	}
}

func TestProcessorConfig_Validate_MinimumValueChecks(t *testing.T) {
	c := NewConfig()
	c.NumWorkers = 0
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error for num_workers <= 0, got nil")
	}

	c = NewConfig()
	c.GlobalConcurrency = 0
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error for global_concurrency <= 0, got nil")
	}

	c = NewConfig()
	c.PerModelMaxConcurrency = 0
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error for per_model_max_concurrency <= 0, got nil")
	}

	c = NewConfig()
	c.ShutdownTimeout = 0
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error for shutdown_timeout <= 0, got nil")
	}

	c = NewConfig()
	c.InferenceConfig.RequestTimeout = 0
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error for request_timeout <= 0, got nil")
	}
}

func TestProcessorConfig_LoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")

	// time.Duration check yaml
	yamlData := []byte(`
poll_interval: 2s
task_wait_time: 500ms
num_workers: 3
global_concurrency: 50
per_model_max_concurrency: 5
work_dir: "` + dir + `/work"
addr: ":1234"
inference_config:
  gateway_url: "http://example:8000"
  request_timeout: 30s
  max_retries: 9
  initial_backoff: 250ms
  max_backoff: 10s
  tls_insecure_skip_verify: true
upload_retry:
  max_retries: 5
  initial_backoff: 500ms
  max_backoff: 30s
default_output_expiration_seconds: 86400
progress_ttl_seconds: 3600
`)

	if err := os.WriteFile(path, yamlData, 0o600); err != nil {
		t.Fatalf("failed to write yaml: %v", err)
	}

	c := &ProcessorConfig{}
	if err := c.LoadFromYAML(path); err != nil {
		t.Fatalf("LoadFromYAML() error: %v", err)
	}

	if c.PollInterval != 2*time.Second {
		t.Fatalf("PollInterval = %v, want %v", c.PollInterval, 2*time.Second)
	}
	if c.TaskWaitTime != 500*time.Millisecond {
		t.Fatalf("TaskWaitTime = %v, want %v", c.TaskWaitTime, 500*time.Millisecond)
	}
	if c.NumWorkers != 3 {
		t.Fatalf("NumWorkers = %d, want %d", c.NumWorkers, 3)
	}
	if c.GlobalConcurrency != 50 {
		t.Fatalf("GlobalConcurrency = %d, want %d", c.GlobalConcurrency, 50)
	}
	if c.PerModelMaxConcurrency != 5 {
		t.Fatalf("PerModelMaxConcurrency = %d, want %d", c.PerModelMaxConcurrency, 5)
	}
	if c.WorkDir != filepath.Join(dir, "work") {
		t.Fatalf("WorkDir = %q, want %q", c.WorkDir, filepath.Join(dir, "work"))
	}
	if c.Addr != ":1234" {
		t.Fatalf("Addr = %q, want %q", c.Addr, ":1234")
	}

	if c.InferenceConfig.GatewayURL != "http://example:8000" {
		t.Fatalf("GatewayURL = %q, want %q", c.InferenceConfig.GatewayURL, "http://example:8000")
	}
	if c.InferenceConfig.RequestTimeout != 30*time.Second {
		t.Fatalf("RequestTimeout = %v, want %v", c.InferenceConfig.RequestTimeout, 30*time.Second)
	}
	if c.InferenceConfig.MaxRetries != 9 {
		t.Fatalf("MaxRetries = %d, want %d", c.InferenceConfig.MaxRetries, 9)
	}
	if c.InferenceConfig.InitialBackoff != 250*time.Millisecond {
		t.Fatalf("InitialBackoff = %v, want %v", c.InferenceConfig.InitialBackoff, 250*time.Millisecond)
	}
	if c.InferenceConfig.MaxBackoff != 10*time.Second {
		t.Fatalf("MaxBackoff = %v, want %v", c.InferenceConfig.MaxBackoff, 10*time.Second)
	}
	if !c.InferenceConfig.TLSInsecureSkipVerify {
		t.Fatalf("TLSInsecureSkipVerify = false, want true")
	}

	if c.UploadRetry.MaxRetries != 5 {
		t.Fatalf("UploadRetry.MaxRetries = %d, want %d", c.UploadRetry.MaxRetries, 5)
	}
	if c.UploadRetry.InitialBackoff != 500*time.Millisecond {
		t.Fatalf("UploadRetry.InitialBackoff = %v, want %v", c.UploadRetry.InitialBackoff, 500*time.Millisecond)
	}
	if c.UploadRetry.MaxBackoff != 30*time.Second {
		t.Fatalf("UploadRetry.MaxBackoff = %v, want %v", c.UploadRetry.MaxBackoff, 30*time.Second)
	}

	if c.DefaultOutputExpirationSeconds != 86400 {
		t.Fatalf("DefaultOutputExpirationSeconds = %d, want %d", c.DefaultOutputExpirationSeconds, 86400)
	}

	if c.ProgressTTLSeconds != 3600 {
		t.Fatalf("ProgressTTLSeconds = %d, want %d", c.ProgressTTLSeconds, 3600)
	}
}
