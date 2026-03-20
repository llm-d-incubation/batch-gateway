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

	"github.com/llm-d-incubation/batch-gateway/internal/util/ptr"
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

	// default gateway spot-check
	defaultGW, ok := c.ModelGateways[DefaultModelGatewayKey]
	if !ok {
		t.Fatalf("ModelGateways missing %q key", DefaultModelGatewayKey)
	}
	if defaultGW.URL != "http://localhost:8000" {
		t.Fatalf("default URL = %q, want %q", defaultGW.URL, "http://localhost:8000")
	}
	if defaultGW.RequestTimeout == nil || *defaultGW.RequestTimeout != 5*time.Minute {
		t.Fatalf("default RequestTimeout = %v, want 5m", defaultGW.RequestTimeout)
	}
	if defaultGW.MaxRetries == nil || *defaultGW.MaxRetries != 3 {
		t.Fatalf("default MaxRetries = %v, want 3", defaultGW.MaxRetries)
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

func TestProcessorConfig_LoadFromYAML_PerModelInheritsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")

	yamlData := []byte(`
poll_interval: 5s
task_wait_time: 1s
num_workers: 1
global_concurrency: 100
per_model_max_concurrency: 10
work_dir: "` + dir + `/work"
addr: ":9090"
shutdown_timeout: 30s
queue_time_bucket:
  start: 0.1
  factor: 2
  count: 10
process_time_bucket:
  start: 0.1
  factor: 2
  count: 15
model_gateways:
  "default":
    url: "http://default-gw:8000"
    request_timeout: 5m
    max_retries: 3
    initial_backoff: 1s
    max_backoff: 60s
  "llama-3":
    url: "http://llama-gw:8000"
  "mistral":
    url: "http://mistral-gw:8000"
    request_timeout: 2m
    max_retries: 1
upload_retry:
  max_retries: 3
  initial_backoff: 1s
  max_backoff: 10s
progress_ttl_seconds: 86400
`)

	if err := os.WriteFile(path, yamlData, 0o600); err != nil {
		t.Fatalf("failed to write yaml: %v", err)
	}

	c := &ProcessorConfig{}
	if err := c.LoadFromYAML(path); err != nil {
		t.Fatalf("LoadFromYAML() error: %v", err)
	}

	llama, ok := c.ModelGateways["llama-3"]
	if !ok {
		t.Fatal("ModelGateways missing llama-3")
	}
	if llama.URL != "http://llama-gw:8000" {
		t.Fatalf("llama-3 URL = %q, want %q", llama.URL, "http://llama-gw:8000")
	}
	if llama.RequestTimeout == nil || *llama.RequestTimeout != 5*time.Minute {
		t.Fatalf("llama-3 RequestTimeout = %v, want 5m (inherited from default)", llama.RequestTimeout)
	}
	if llama.MaxRetries == nil || *llama.MaxRetries != 3 {
		t.Fatalf("llama-3 MaxRetries = %v, want 3 (inherited from default)", llama.MaxRetries)
	}
	if llama.InitialBackoff == nil || *llama.InitialBackoff != 1*time.Second {
		t.Fatalf("llama-3 InitialBackoff = %v, want 1s (inherited from default)", llama.InitialBackoff)
	}
	if llama.MaxBackoff == nil || *llama.MaxBackoff != 60*time.Second {
		t.Fatalf("llama-3 MaxBackoff = %v, want 60s (inherited from default)", llama.MaxBackoff)
	}

	mistral, ok := c.ModelGateways["mistral"]
	if !ok {
		t.Fatal("ModelGateways missing mistral")
	}
	if mistral.RequestTimeout == nil || *mistral.RequestTimeout != 2*time.Minute {
		t.Fatalf("mistral RequestTimeout = %v, want 2m (explicit override)", mistral.RequestTimeout)
	}
	if mistral.MaxRetries == nil || *mistral.MaxRetries != 1 {
		t.Fatalf("mistral MaxRetries = %v, want 1 (explicit override)", mistral.MaxRetries)
	}
	if mistral.InitialBackoff == nil || *mistral.InitialBackoff != 1*time.Second {
		t.Fatalf("mistral InitialBackoff = %v, want 1s (inherited from default)", mistral.InitialBackoff)
	}
	if mistral.MaxBackoff == nil || *mistral.MaxBackoff != 60*time.Second {
		t.Fatalf("mistral MaxBackoff = %v, want 60s (inherited from default)", mistral.MaxBackoff)
	}

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() should pass after default inheritance: %v", err)
	}
}

func TestProcessorConfig_LoadFromYAML_ExplicitZeroMaxRetries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")

	yamlData := []byte(`
poll_interval: 5s
task_wait_time: 1s
num_workers: 1
global_concurrency: 100
per_model_max_concurrency: 10
work_dir: "` + dir + `/work"
addr: ":9090"
shutdown_timeout: 30s
queue_time_bucket:
  start: 0.1
  factor: 2
  count: 10
process_time_bucket:
  start: 0.1
  factor: 2
  count: 15
model_gateways:
  "default":
    url: "http://default-gw:8000"
    request_timeout: 5m
    max_retries: 3
    initial_backoff: 1s
    max_backoff: 60s
  "no-retry-model":
    url: "http://no-retry-gw:8000"
    max_retries: 0
upload_retry:
  max_retries: 3
  initial_backoff: 1s
  max_backoff: 10s
progress_ttl_seconds: 86400
`)

	if err := os.WriteFile(path, yamlData, 0o600); err != nil {
		t.Fatalf("failed to write yaml: %v", err)
	}

	c := &ProcessorConfig{}
	if err := c.LoadFromYAML(path); err != nil {
		t.Fatalf("LoadFromYAML() error: %v", err)
	}

	noRetry, ok := c.ModelGateways["no-retry-model"]
	if !ok {
		t.Fatal("ModelGateways missing no-retry-model")
	}
	if noRetry.MaxRetries == nil {
		t.Fatal("no-retry-model MaxRetries should not be nil after YAML parse")
	}
	if *noRetry.MaxRetries != 0 {
		t.Fatalf("no-retry-model MaxRetries = %d, want 0 (explicit zero must not be overwritten by default)", *noRetry.MaxRetries)
	}
	if noRetry.RequestTimeout == nil || *noRetry.RequestTimeout != 5*time.Minute {
		t.Fatalf("no-retry-model RequestTimeout = %v, want 5m (inherited from default)", noRetry.RequestTimeout)
	}

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() should pass with explicit max_retries=0: %v", err)
	}
}

func TestProcessorConfig_Validate_MissingDefaultGateway(t *testing.T) {
	c := NewConfig()
	c.ModelGateways = map[string]ModelGatewayConfig{
		"llama-3": {URL: "http://gateway-a:8000"},
	}
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error when model_gateways.default is missing, got nil")
	}
}

func TestProcessorConfig_Validate_APIKeyFile(t *testing.T) {
	t.Run("name_and_file_mutually_exclusive", func(t *testing.T) {
		c := NewConfig()
		gw := c.ModelGateways[DefaultModelGatewayKey]
		gw.APIKeyName = "my-key"
		gw.APIKeyFile = "/some/file"
		c.ModelGateways[DefaultModelGatewayKey] = gw
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error when both api_key_name and api_key_file are set, got nil")
		}
	})

	t.Run("file_not_found", func(t *testing.T) {
		c := NewConfig()
		gw := c.ModelGateways[DefaultModelGatewayKey]
		gw.APIKeyFile = "/nonexistent/path/to/key"
		c.ModelGateways[DefaultModelGatewayKey] = gw
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error when api_key_file does not exist, got nil")
		}
	})

	t.Run("valid_file", func(t *testing.T) {
		dir := t.TempDir()
		keyFile := filepath.Join(dir, "token")
		if err := os.WriteFile(keyFile, []byte("my-secret-token"), 0o600); err != nil {
			t.Fatalf("failed to write key file: %v", err)
		}

		c := NewConfig()
		gw := c.ModelGateways[DefaultModelGatewayKey]
		gw.APIKeyFile = keyFile
		c.ModelGateways[DefaultModelGatewayKey] = gw
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error with valid api_key_file: %v", err)
		}
	})

	t.Run("resolve_reads_and_trims_file", func(t *testing.T) {
		dir := t.TempDir()
		keyFile := filepath.Join(dir, "token")
		if err := os.WriteFile(keyFile, []byte("  file-based-token  \n"), 0o600); err != nil {
			t.Fatalf("failed to write key file: %v", err)
		}

		gateways := map[string]ModelGatewayConfig{
			DefaultModelGatewayKey: {
				URL:            "http://gateway:8000",
				APIKeyFile:     keyFile,
				RequestTimeout: ptr.To(5 * time.Minute),
				MaxRetries:     ptr.To(3),
				InitialBackoff: ptr.To(1 * time.Second),
				MaxBackoff:     ptr.To(60 * time.Second),
			},
		}

		resolved, err := ResolveModelGateways(gateways)
		if err != nil {
			t.Fatalf("ResolveModelGateways() error: %v", err)
		}

		got := resolved[DefaultModelGatewayKey].APIKey
		if got != "file-based-token" {
			t.Fatalf("APIKey = %q, want %q", got, "file-based-token")
		}
	})
}

func TestProcessorConfig_Validate_GatewayTLSPartialConfigRejected(t *testing.T) {
	c := NewConfig()
	gw := c.ModelGateways[DefaultModelGatewayKey]
	gw.TLSClientCertFile = "/tmp/client-cert.pem"
	gw.TLSClientKeyFile = ""
	c.ModelGateways[DefaultModelGatewayKey] = gw
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error when only tls_client_cert_file is set, got nil")
	}

	gw2 := c.ModelGateways[DefaultModelGatewayKey]
	gw2.TLSClientCertFile = ""
	gw2.TLSClientKeyFile = "/tmp/client-key.pem"
	c.ModelGateways[DefaultModelGatewayKey] = gw2
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
	gw := c.ModelGateways[DefaultModelGatewayKey]
	gw.RequestTimeout = nil
	c.ModelGateways[DefaultModelGatewayKey] = gw
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error for nil request_timeout, got nil")
	}
}

func TestProcessorConfig_LoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")

	yamlData := []byte(`
poll_interval: 2s
task_wait_time: 500ms
num_workers: 3
global_concurrency: 50
per_model_max_concurrency: 5
work_dir: "` + dir + `/work"
addr: ":1234"
model_gateways:
  "default":
    url: "http://example:8000"
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

	defaultGW, ok := c.ModelGateways[DefaultModelGatewayKey]
	if !ok {
		t.Fatalf("ModelGateways missing %q key after YAML load", DefaultModelGatewayKey)
	}
	if defaultGW.URL != "http://example:8000" {
		t.Fatalf("default URL = %q, want %q", defaultGW.URL, "http://example:8000")
	}
	if defaultGW.RequestTimeout == nil || *defaultGW.RequestTimeout != 30*time.Second {
		t.Fatalf("default RequestTimeout = %v, want 30s", defaultGW.RequestTimeout)
	}
	if defaultGW.MaxRetries == nil || *defaultGW.MaxRetries != 9 {
		t.Fatalf("default MaxRetries = %v, want 9", defaultGW.MaxRetries)
	}
	if defaultGW.InitialBackoff == nil || *defaultGW.InitialBackoff != 250*time.Millisecond {
		t.Fatalf("default InitialBackoff = %v, want 250ms", defaultGW.InitialBackoff)
	}
	if defaultGW.MaxBackoff == nil || *defaultGW.MaxBackoff != 10*time.Second {
		t.Fatalf("default MaxBackoff = %v, want 10s", defaultGW.MaxBackoff)
	}
	if !defaultGW.TLSInsecureSkipVerify {
		t.Fatalf("default TLSInsecureSkipVerify = false, want true")
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
