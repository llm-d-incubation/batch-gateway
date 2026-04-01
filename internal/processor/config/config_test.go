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

// validPerModelConfig returns a minimal valid per-model gateway config for tests.
func validPerModelConfig() map[string]ModelGatewayConfig {
	return map[string]ModelGatewayConfig{
		"llama-3": {
			URL:            "http://llama-gw:8000",
			RequestTimeout: ptr.To(5 * time.Minute),
			MaxRetries:     ptr.To(3),
			InitialBackoff: ptr.To(1 * time.Second),
			MaxBackoff:     ptr.To(60 * time.Second),
		},
	}
}

// validGlobalConfig returns a minimal valid global gateway config for tests.
func validGlobalConfig() *ModelGatewayConfig {
	return &ModelGatewayConfig{
		URL:            "http://global-gw:8000",
		RequestTimeout: ptr.To(5 * time.Minute),
		MaxRetries:     ptr.To(3),
		InitialBackoff: ptr.To(1 * time.Second),
		MaxBackoff:     ptr.To(60 * time.Second),
	}
}

func TestNewConfig_Defaults(t *testing.T) {
	c := NewConfig()
	if c == nil {
		t.Fatalf("NewConfig returned nil")
	}

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
	if c.DBClientCfg.Type != "redis" {
		t.Fatalf("DBClientCfg.Type = %q, want %q", c.DBClientCfg.Type, "redis")
	}
	if c.RecoveryMaxConcurrency != 5 {
		t.Fatalf("RecoveryMaxConcurrency = %d, want %d", c.RecoveryMaxConcurrency, 5)
	}
	if c.ModelGateways != nil {
		t.Fatalf("ModelGateways should be nil by default, got %v", c.ModelGateways)
	}
	if c.GlobalInferenceGateway != nil {
		t.Fatalf("GlobalInferenceGateway should be nil by default")
	}

	want90Days := int64(90 * 24 * 60 * 60)
	if c.DefaultOutputExpirationSeconds != want90Days {
		t.Fatalf("DefaultOutputExpirationSeconds = %d, want %d", c.DefaultOutputExpirationSeconds, want90Days)
	}
	if c.ProgressTTLSeconds != 86400 {
		t.Fatalf("ProgressTTLSeconds = %d, want %d", c.ProgressTTLSeconds, 86400)
	}
}

func TestProcessorConfig_Validate_WorkDirEmpty(t *testing.T) {
	c := NewConfig()
	c.ModelGateways = validPerModelConfig()
	c.WorkDir = ""
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error for empty WorkDir, got nil")
	}
}

func TestProcessorConfig_Validate_TaskWaitTimeMustBeShorterThanPollInterval(t *testing.T) {
	c := NewConfig()
	c.ModelGateways = validPerModelConfig()
	c.DBClientCfg.Type = "mock"
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

func TestProcessorConfig_LoadFromYAML_ExplicitZeroMaxRetries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")

	yamlData := []byte(`
poll_interval: 5s
task_wait_time: 1s
num_workers: 1
global_concurrency: 100
per_model_max_concurrency: 10
recovery_max_concurrency: 5
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
e2e_latency_bucket:
  start: 1
  factor: 3
  count: 12
model_gateways:
  "llama-3":
    url: "http://llama-gw:8000"
    request_timeout: 5m
    max_retries: 3
    initial_backoff: 1s
    max_backoff: 60s
  "no-retry-model":
    url: "http://no-retry-gw:8000"
    request_timeout: 5m
    max_retries: 0
    initial_backoff: 1s
    max_backoff: 60s
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

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() should pass with explicit max_retries=0: %v", err)
	}
}

func TestProcessorConfig_Validate_NeitherGlobalNorPerModel(t *testing.T) {
	c := NewConfig()
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() expected error when neither global nor per-model is configured")
	}
}

func TestProcessorConfig_Validate_GlobalOnly(t *testing.T) {
	c := NewConfig()
	c.GlobalInferenceGateway = validGlobalConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() unexpected error with global-only config: %v", err)
	}
}

func TestProcessorConfig_Validate_PerModelWithoutDefault(t *testing.T) {
	c := NewConfig()
	c.ModelGateways = validPerModelConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() unexpected error with per-model-only config: %v", err)
	}
}

func TestProcessorConfig_Validate_GlobalAndPerModelMutuallyExclusive(t *testing.T) {
	c := NewConfig()
	c.GlobalInferenceGateway = validGlobalConfig()
	c.ModelGateways = validPerModelConfig()
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() expected error when both global and per-model are set")
	}
}

func TestProcessorConfig_Validate_APIKeyFile(t *testing.T) {
	t.Run("name_and_file_mutually_exclusive", func(t *testing.T) {
		c := NewConfig()
		c.ModelGateways = map[string]ModelGatewayConfig{
			"llama-3": {
				URL:            "http://gw:8000",
				RequestTimeout: ptr.To(5 * time.Minute),
				MaxRetries:     ptr.To(3),
				InitialBackoff: ptr.To(1 * time.Second),
				MaxBackoff:     ptr.To(60 * time.Second),
				APIKeyName:     "my-key",
				APIKeyFile:     "/some/file",
			},
		}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error when both api_key_name and api_key_file are set, got nil")
		}
	})

	t.Run("file_not_found", func(t *testing.T) {
		c := NewConfig()
		c.ModelGateways = map[string]ModelGatewayConfig{
			"llama-3": {
				URL:            "http://gw:8000",
				RequestTimeout: ptr.To(5 * time.Minute),
				MaxRetries:     ptr.To(3),
				InitialBackoff: ptr.To(1 * time.Second),
				MaxBackoff:     ptr.To(60 * time.Second),
				APIKeyFile:     "/nonexistent/path/to/key",
			},
		}
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
		c.ModelGateways = map[string]ModelGatewayConfig{
			"llama-3": {
				URL:            "http://gw:8000",
				RequestTimeout: ptr.To(5 * time.Minute),
				MaxRetries:     ptr.To(3),
				InitialBackoff: ptr.To(1 * time.Second),
				MaxBackoff:     ptr.To(60 * time.Second),
				APIKeyFile:     keyFile,
			},
		}
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

		cfg := NewConfig()
		cfg.ModelGateways = map[string]ModelGatewayConfig{
			"llama-3": {
				URL:            "http://gateway:8000",
				APIKeyFile:     keyFile,
				RequestTimeout: ptr.To(5 * time.Minute),
				MaxRetries:     ptr.To(3),
				InitialBackoff: ptr.To(1 * time.Second),
				MaxBackoff:     ptr.To(60 * time.Second),
			},
		}

		resolved, err := ResolveModelGateways(cfg)
		if err != nil {
			t.Fatalf("ResolveModelGateways() error: %v", err)
		}

		got := resolved.PerModel["llama-3"].APIKey
		if got != "file-based-token" {
			t.Fatalf("APIKey = %q, want %q", got, "file-based-token")
		}
	})
}

func TestProcessorConfig_Validate_GatewayTLSPartialConfigRejected(t *testing.T) {
	c := NewConfig()
	c.ModelGateways = map[string]ModelGatewayConfig{
		"llama-3": {
			URL:               "http://gw:8000",
			RequestTimeout:    ptr.To(5 * time.Minute),
			MaxRetries:        ptr.To(3),
			InitialBackoff:    ptr.To(1 * time.Second),
			MaxBackoff:        ptr.To(60 * time.Second),
			TLSClientCertFile: "/tmp/client-cert.pem",
		},
	}
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error when only tls_client_cert_file is set, got nil")
	}

	c.ModelGateways = map[string]ModelGatewayConfig{
		"llama-3": {
			URL:              "http://gw:8000",
			RequestTimeout:   ptr.To(5 * time.Minute),
			MaxRetries:       ptr.To(3),
			InitialBackoff:   ptr.To(1 * time.Second),
			MaxBackoff:       ptr.To(60 * time.Second),
			TLSClientKeyFile: "/tmp/client-key.pem",
		},
	}
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error when only tls_client_key_file is set, got nil")
	}
}

func TestProcessorConfig_Validate_MinimumValueChecks(t *testing.T) {
	c := NewConfig()
	c.ModelGateways = validPerModelConfig()
	c.NumWorkers = 0
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error for num_workers <= 0, got nil")
	}

	c = NewConfig()
	c.ModelGateways = validPerModelConfig()
	c.GlobalConcurrency = 0
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error for global_concurrency <= 0, got nil")
	}

	c = NewConfig()
	c.ModelGateways = validPerModelConfig()
	c.PerModelMaxConcurrency = 0
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error for per_model_max_concurrency <= 0, got nil")
	}

	c = NewConfig()
	c.ModelGateways = validPerModelConfig()
	c.ShutdownTimeout = 0
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error for shutdown_timeout <= 0, got nil")
	}

	c = NewConfig()
	c.ModelGateways = validPerModelConfig()
	c.RecoveryMaxConcurrency = 0
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() expected error for recovery_max_concurrency <= 0, got nil")
	}

	c = NewConfig()
	c.ModelGateways = map[string]ModelGatewayConfig{
		"llama-3": {
			URL:            "http://gw:8000",
			RequestTimeout: nil,
			MaxRetries:     ptr.To(3),
			InitialBackoff: ptr.To(1 * time.Second),
			MaxBackoff:     ptr.To(60 * time.Second),
		},
	}
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
  "llama-3":
    url: "http://example:8000"
    request_timeout: 30s
    max_retries: 9
    initial_backoff: 250ms
    max_backoff: 10s
    tls_insecure_skip_verify: true
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

	gw, ok := c.ModelGateways["llama-3"]
	if !ok {
		t.Fatalf("ModelGateways missing %q key after YAML load", "llama-3")
	}
	if gw.URL != "http://example:8000" {
		t.Fatalf("llama-3 URL = %q, want %q", gw.URL, "http://example:8000")
	}
	if gw.RequestTimeout == nil || *gw.RequestTimeout != 30*time.Second {
		t.Fatalf("llama-3 RequestTimeout = %v, want 30s", gw.RequestTimeout)
	}
	if gw.MaxRetries == nil || *gw.MaxRetries != 9 {
		t.Fatalf("llama-3 MaxRetries = %v, want 9", gw.MaxRetries)
	}
	if gw.InitialBackoff == nil || *gw.InitialBackoff != 250*time.Millisecond {
		t.Fatalf("llama-3 InitialBackoff = %v, want 250ms", gw.InitialBackoff)
	}
	if gw.MaxBackoff == nil || *gw.MaxBackoff != 10*time.Second {
		t.Fatalf("llama-3 MaxBackoff = %v, want 10s", gw.MaxBackoff)
	}
	if !gw.TLSInsecureSkipVerify {
		t.Fatalf("llama-3 TLSInsecureSkipVerify = false, want true")
	}

	if c.DefaultOutputExpirationSeconds != 86400 {
		t.Fatalf("DefaultOutputExpirationSeconds = %d, want %d", c.DefaultOutputExpirationSeconds, 86400)
	}
	if c.ProgressTTLSeconds != 3600 {
		t.Fatalf("ProgressTTLSeconds = %d, want %d", c.ProgressTTLSeconds, 3600)
	}
}
