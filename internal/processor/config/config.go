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

// The processor's configuration definitions.

package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/database/postgresql"
	fsclient "github.com/llm-d-incubation/batch-gateway/internal/files_store/fs"
	s3client "github.com/llm-d-incubation/batch-gateway/internal/files_store/s3"
	ucom "github.com/llm-d-incubation/batch-gateway/internal/util/com"
	"github.com/llm-d-incubation/batch-gateway/internal/util/ptr"
	uredis "github.com/llm-d-incubation/batch-gateway/internal/util/redis"
	inference "github.com/llm-d-incubation/batch-gateway/pkg/clients/inference"
	"gopkg.in/yaml.v3"
)

type ProcessorConfig struct {
	// TaskWaitTime is the timeout parameter used when dequeueing from the priority queue
	// This should be shorter than PollInterval
	TaskWaitTime time.Duration `yaml:"task_wait_time"`

	// NumWorkers is the fixed number of worker goroutines spawned to process jobs
	NumWorkers int `yaml:"num_workers"`

	// GlobalConcurrency limits total in-flight inference requests across all workers in a processor.
	// Protects system resources (goroutines, sockets, memory) from unbounded growth.
	GlobalConcurrency int `yaml:"global_concurrency"`

	// PerModelMaxConcurrency limits concurrent inference requests per individual model.
	// Protects downstream inference gateway from being overwhelmed by a single model's requests.
	PerModelMaxConcurrency int `yaml:"per_model_max_concurrency"`

	// PollInterval defines how frequently the processor checks the database for new jobs
	PollInterval time.Duration `yaml:"poll_interval"`

	// QueueTimeBucket defines exponential bucket configs for queue wait time metric
	QueueTimeBucket BucketConfig `yaml:"queue_time_bucket"`

	// ProcessTimeBucket defines exponential bucket configs for process time metric
	ProcessTimeBucket BucketConfig `yaml:"process_time_bucket"`

	// DatabaseType specifies the database backend: "mock", "redis", or "postgresql".
	DatabaseType string `yaml:"database_type"`

	// PostgreSQLCfg holds PostgreSQL connection settings (used when DatabaseType is "postgresql").
	PostgreSQLCfg postgresql.PostgreSQLConfig `yaml:"postgresql"`

	Addr string `yaml:"addr"`
	// TerminateOnObservabilityFailure controls whether observability server failures should terminate the processor.
	// false: best-effort (default), true: fatal.
	TerminateOnObservabilityFailure bool `yaml:"terminate_on_observability_failure"`

	// ShutdownTimeout is the timeout for shutting down the processor
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`

	// WorkDir is the work directory for processor
	WorkDir string `yaml:"work_dir"`

	// ModelGateways maps model names to gateway/inference settings.
	// Reserved key "default" is required and acts as fallback for all models.
	// Lookup order: ModelGateways[model] -> ModelGateways["default"].
	ModelGateways map[string]ModelGatewayConfig `yaml:"model_gateways"`

	// UploadRetry controls retry behaviour when uploading output files to shared storage.
	UploadRetry RetryConfig `yaml:"upload_retry"`

	// DefaultOutputExpirationSeconds is the default TTL for batch output/error files in seconds.
	// Used as fallback when the user does not provide output_expires_after in POST /v1/batches.
	// 0 means no expiration (keep until explicitly deleted).
	DefaultOutputExpirationSeconds int64 `yaml:"default_output_expiration_seconds"`

	// ProgressTTLSeconds is the TTL for temporary progress updates in the status store (Redis).
	ProgressTTLSeconds int `yaml:"progress_ttl_seconds"`

	// RedisCfg holds Redis client settings (timeouts, retries, pool, TLS).
	// URL, ServiceName, EnableTracing, and Certificates are set at runtime, not from YAML.
	RedisCfg uredis.RedisClientConfig `yaml:"redis"`

	// EnablePprof enables pprof profiling endpoints on the observability server.
	EnablePprof bool `yaml:"enable_pprof"`

	// OTel holds OpenTelemetry-related settings.
	OTel struct {
		RedisTracing      bool `yaml:"redis_tracing"`
		PostgresqlTracing bool `yaml:"postgresql_tracing"`
	} `yaml:"otel"`

	// FileClient holds configuration for the shared file storage client (fs or s3).
	FileClientCfg struct {
		Type     string          `yaml:"type"`
		FSConfig fsclient.Config `yaml:"fs"`
		S3Config s3client.Config `yaml:"s3"`
	} `yaml:"file_client"`
}

type RetryConfig struct {
	MaxRetries     int           `yaml:"max_retries"`
	InitialBackoff time.Duration `yaml:"initial_backoff"`
	MaxBackoff     time.Duration `yaml:"max_backoff"`
}

// DefaultModelGatewayKey is the reserved key in ModelGateways that acts as
// the fallback gateway for any model without an explicit mapping.
const DefaultModelGatewayKey = "default"

// ModelGatewayConfig describes the full gateway and HTTP/TLS settings for one
// model (or the default fallback). Per-model entries inherit unset fields from
// the "default" entry via applyModelGatewayDefaults, so only fields that differ
// from the default need to be specified.
//
// HTTP fields use pointers so that nil (unset → inherit from default) is
// distinguishable from explicit zero values (e.g. MaxRetries=0 means "no
// retries", RequestTimeout=0 means "no timeout").
//
// API key resolution (mutually exclusive, first match wins):
//   - api_key_file: read the token/key from an arbitrary file path
//     (e.g. /var/run/secrets/kubernetes.io/serviceaccount/token).
//   - api_key_name: key name under /etc/.secrets/ (mounted Kubernetes secret).
//   - (neither set on "default"): falls back to the mounted inference-api-key secret.
type ModelGatewayConfig struct {
	URL        string `yaml:"url"`
	APIKeyName string `yaml:"api_key_name"`
	APIKeyFile string `yaml:"api_key_file"`

	RequestTimeout *time.Duration `yaml:"request_timeout"`
	MaxRetries     *int           `yaml:"max_retries"`
	InitialBackoff *time.Duration `yaml:"initial_backoff"`
	MaxBackoff     *time.Duration `yaml:"max_backoff"`

	TLSInsecureSkipVerify bool   `yaml:"tls_insecure_skip_verify"`
	TLSCACertFile         string `yaml:"tls_ca_cert_file,omitempty"`
	TLSClientCertFile     string `yaml:"tls_client_cert_file,omitempty"`
	TLSClientKeyFile      string `yaml:"tls_client_key_file,omitempty"`
}

type BucketConfig struct {
	BucketStart  float64 `yaml:"start"`
	BucketFactor float64 `yaml:"factor"`
	BucketCount  int     `yaml:"count"`
}

// LoadFromYaml loads the configuration from a YAML file.
func (pc *ProcessorConfig) LoadFromYAML(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(pc); err != nil {
		return err
	}

	pc.applyModelGatewayDefaults()
	return nil
}

// applyModelGatewayDefaults fills unset (nil) fields in per-model gateway
// entries with the corresponding value from the "default" entry.
// This lets users add a per-model entry with only the fields that differ.
func (pc *ProcessorConfig) applyModelGatewayDefaults() {
	dflt, ok := pc.ModelGateways[DefaultModelGatewayKey]
	if !ok {
		return
	}
	for model, gw := range pc.ModelGateways {
		if model == DefaultModelGatewayKey {
			continue
		}
		if gw.RequestTimeout == nil {
			gw.RequestTimeout = dflt.RequestTimeout
		}
		if gw.MaxRetries == nil {
			gw.MaxRetries = dflt.MaxRetries
		}
		if gw.InitialBackoff == nil {
			gw.InitialBackoff = dflt.InitialBackoff
		}
		if gw.MaxBackoff == nil {
			gw.MaxBackoff = dflt.MaxBackoff
		}
		pc.ModelGateways[model] = gw
	}
}

// NewConfig returns a new ProcessorConfig with default values.
// TaskWaitTime has to be shorter than poll interval
func NewConfig() *ProcessorConfig {
	return &ProcessorConfig{
		PollInterval: 5 * time.Second,
		TaskWaitTime: 1 * time.Second,
		ProcessTimeBucket: BucketConfig{
			BucketStart:  0.1,
			BucketFactor: 2,
			BucketCount:  15,
		},
		QueueTimeBucket: BucketConfig{
			BucketStart:  0.1,
			BucketFactor: 2,
			BucketCount:  10,
		},

		GlobalConcurrency:      100,
		PerModelMaxConcurrency: 10,
		NumWorkers:             1,
		Addr:                   ":9090",
		// Keep observability as best-effort by default.
		TerminateOnObservabilityFailure: false,
		ShutdownTimeout:                 30 * time.Second,
		WorkDir:                         "/var/lib/batch-gateway/processor",
		DatabaseType:                    "redis",
		FileClientCfg: struct {
			Type     string          `yaml:"type"`
			FSConfig fsclient.Config `yaml:"fs"`
			S3Config s3client.Config `yaml:"s3"`
		}{
			Type: "mock",
		},
		ModelGateways: map[string]ModelGatewayConfig{
			DefaultModelGatewayKey: {
				URL:            "http://localhost:8000",
				RequestTimeout: ptr.To(5 * time.Minute),
				MaxRetries:     ptr.To(3),
				InitialBackoff: ptr.To(1 * time.Second),
				MaxBackoff:     ptr.To(60 * time.Second),
			},
		},
		UploadRetry: RetryConfig{
			MaxRetries:     3,
			InitialBackoff: 1 * time.Second,
			MaxBackoff:     10 * time.Second,
		},
		DefaultOutputExpirationSeconds: 90 * 24 * 60 * 60, // 90 days
		ProgressTTLSeconds:             24 * 60 * 60,      // 24 hours
	}
}

func (c *ProcessorConfig) Validate() error {
	if c.PollInterval <= 0 {
		return fmt.Errorf("poll_interval must be > 0")
	}
	if c.TaskWaitTime <= 0 {
		return fmt.Errorf("task_wait_time must be > 0")
	}
	if c.TaskWaitTime >= c.PollInterval {
		return fmt.Errorf("task_wait_time must be shorter than poll_interval")
	}
	if c.NumWorkers <= 0 {
		return fmt.Errorf("num_workers must be > 0")
	}
	if c.GlobalConcurrency <= 0 {
		return fmt.Errorf("global_concurrency must be > 0")
	}
	if c.PerModelMaxConcurrency <= 0 {
		return fmt.Errorf("per_model_max_concurrency must be > 0")
	}
	if c.PerModelMaxConcurrency > c.GlobalConcurrency {
		return fmt.Errorf("per_model_max_concurrency (%d) must be <= global_concurrency (%d)", c.PerModelMaxConcurrency, c.GlobalConcurrency)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("shutdown_timeout must be > 0")
	}
	if c.Addr == "" {
		return fmt.Errorf("addr cannot be empty")
	}
	if c.WorkDir == "" {
		return fmt.Errorf("work_dir cannot be empty")
	}

	if c.QueueTimeBucket.BucketStart <= 0 || c.QueueTimeBucket.BucketFactor <= 1 || c.QueueTimeBucket.BucketCount <= 0 {
		return fmt.Errorf("queue_time_bucket must satisfy: start > 0, factor > 1, count > 0")
	}
	if c.ProcessTimeBucket.BucketStart <= 0 || c.ProcessTimeBucket.BucketFactor <= 1 || c.ProcessTimeBucket.BucketCount <= 0 {
		return fmt.Errorf("process_time_bucket must satisfy: start > 0, factor > 1, count > 0")
	}

	if _, ok := c.ModelGateways[DefaultModelGatewayKey]; !ok {
		return fmt.Errorf("model_gateways.default is required")
	}
	for model, gw := range c.ModelGateways {
		if gw.URL == "" {
			return fmt.Errorf("model_gateways[%s].url cannot be empty", model)
		}
		if gw.RequestTimeout == nil {
			return fmt.Errorf("model_gateways[%s].request_timeout must be set", model)
		}
		if *gw.RequestTimeout < 0 {
			return fmt.Errorf("model_gateways[%s].request_timeout must be >= 0", model)
		}
		if gw.MaxRetries == nil {
			return fmt.Errorf("model_gateways[%s].max_retries must be set", model)
		}
		if *gw.MaxRetries < 0 {
			return fmt.Errorf("model_gateways[%s].max_retries must be >= 0", model)
		}
		if gw.InitialBackoff == nil {
			return fmt.Errorf("model_gateways[%s].initial_backoff must be set", model)
		}
		if *gw.InitialBackoff < 0 {
			return fmt.Errorf("model_gateways[%s].initial_backoff must be >= 0", model)
		}
		if gw.MaxBackoff == nil {
			return fmt.Errorf("model_gateways[%s].max_backoff must be set", model)
		}
		if *gw.MaxBackoff < 0 {
			return fmt.Errorf("model_gateways[%s].max_backoff must be >= 0", model)
		}
		if *gw.MaxBackoff < *gw.InitialBackoff {
			return fmt.Errorf("model_gateways[%s].max_backoff must be >= initial_backoff", model)
		}
		if gw.APIKeyName != "" && gw.APIKeyFile != "" {
			return fmt.Errorf("model_gateways[%s]: api_key_name and api_key_file are mutually exclusive", model)
		}
		if gw.APIKeyFile != "" {
			if _, err := os.Stat(gw.APIKeyFile); err != nil {
				return fmt.Errorf("model_gateways[%s].api_key_file: %w", model, err)
			}
		}
		if (gw.TLSClientCertFile == "") != (gw.TLSClientKeyFile == "") {
			return fmt.Errorf("model_gateways[%s]: tls_client_cert_file and tls_client_key_file must both be set or both be empty", model)
		}
		if gw.TLSCACertFile != "" {
			if _, err := os.Stat(gw.TLSCACertFile); err != nil {
				return fmt.Errorf("model_gateways[%s].tls_ca_cert_file: %w", model, err)
			}
		}
		if gw.TLSClientCertFile != "" {
			if _, err := os.Stat(gw.TLSClientCertFile); err != nil {
				return fmt.Errorf("model_gateways[%s].tls_client_cert_file: %w", model, err)
			}
			if _, err := os.Stat(gw.TLSClientKeyFile); err != nil {
				return fmt.Errorf("model_gateways[%s].tls_client_key_file: %w", model, err)
			}
		}
	}

	if c.UploadRetry.MaxRetries < 0 {
		return fmt.Errorf("upload_retry.max_retries must be >= 0")
	}
	if c.UploadRetry.InitialBackoff <= 0 {
		return fmt.Errorf("upload_retry.initial_backoff must be > 0")
	}
	if c.UploadRetry.MaxBackoff <= 0 {
		return fmt.Errorf("upload_retry.max_backoff must be > 0")
	}
	if c.UploadRetry.MaxBackoff < c.UploadRetry.InitialBackoff {
		return fmt.Errorf("upload_retry.max_backoff must be >= upload_retry.initial_backoff")
	}

	if c.ProgressTTLSeconds <= 0 {
		return fmt.Errorf("progress_ttl_seconds must be > 0")
	}

	return nil
}

// ResolveModelGateways resolves API keys for all model gateways and returns a
// fully-populated GatewayClientConfig map ready to pass to clientset.NewClientset.
// HTTP fields have already been inherited from "default" by applyModelGatewayDefaults.
// Falls back to the mounted inference-api-key secret if the default gateway
// has no explicit API key configured.
func ResolveModelGateways(gateways map[string]ModelGatewayConfig) (map[string]inference.GatewayClientConfig, error) {
	resolved := make(map[string]inference.GatewayClientConfig, len(gateways))
	for model, gw := range gateways {
		apiKey := ""
		switch {
		case gw.APIKeyFile != "":
			data, err := os.ReadFile(gw.APIKeyFile)
			if err != nil {
				return nil, fmt.Errorf("read API key file for model %q: %w", model, err)
			}
			apiKey = strings.TrimSpace(string(data))
		case gw.APIKeyName != "":
			key, err := ucom.ReadSecretFile(gw.APIKeyName)
			if err != nil {
				return nil, fmt.Errorf("read API key for model %q: %w", model, err)
			}
			apiKey = key
		}
		resolved[model] = inference.GatewayClientConfig{
			URL:                   gw.URL,
			APIKey:                apiKey,
			Timeout:               ptr.Deref(gw.RequestTimeout),
			MaxRetries:            ptr.Deref(gw.MaxRetries),
			InitialBackoff:        ptr.Deref(gw.InitialBackoff),
			MaxBackoff:            ptr.Deref(gw.MaxBackoff),
			TLSInsecureSkipVerify: gw.TLSInsecureSkipVerify,
			TLSCACertFile:         gw.TLSCACertFile,
			TLSClientCertFile:     gw.TLSClientCertFile,
			TLSClientKeyFile:      gw.TLSClientKeyFile,
		}
	}

	// Apply the default API key fallback if no explicit key is configured.
	if defaultGW, ok := resolved[DefaultModelGatewayKey]; ok && defaultGW.APIKey == "" {
		apiKey, err := ucom.ReadSecretFile(ucom.SecretKeyInferenceAPI)
		if err != nil {
			return nil, fmt.Errorf("read default inference API key: %w", err)
		}
		defaultGW.APIKey = apiKey
		resolved[DefaultModelGatewayKey] = defaultGW
	}

	return resolved, nil
}
